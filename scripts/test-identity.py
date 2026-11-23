"""Native identity acceptance inside the owned, locked packaged browser fixture."""
import base64
from http.cookiejar import CookieJar
import hashlib
import hmac
import json
import os
import re
import ssl
import struct
import time
import urllib.error
import urllib.parse
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, _request, _response, _code, _message, _headers, _url):
        return None


class Client:
    def __init__(self, origin, context, token=""):
        self.origin = origin
        self.token = token
        self.cookies = CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
            urllib.request.HTTPSHandler(context=context), urllib.request.HTTPCookieProcessor(self.cookies), NoRedirect())

    def call(self, method, path, body=None, expected=200, headers=None):
        request_headers = {"Origin": self.origin, "Content-Type": "application/json"}
        if self.token:
            request_headers["Authorization"] = "Bearer " + self.token
        request_headers.update(headers or {})
        request = urllib.request.Request(self.origin + path, method=method,
            data=None if body is None else json.dumps(body).encode(), headers=request_headers)
        try:
            response = self.opener.open(request, timeout=30)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            raw = response.read()
            if response.code != expected:
                raise RuntimeError(f"Identity acceptance {method} {path.split('?')[0]}: status {response.code}, expected {expected}")
            content = json.loads(raw) if raw and "json" in response.headers.get("Content-Type", "") else None
            return content, response.headers


def totp(secret):
    key = base64.b32decode(secret.upper() + "=" * ((8 - len(secret) % 8) % 8))
    digest = hmac.new(key, struct.pack(">Q", int(time.time()) // 30), hashlib.sha1).digest()
    offset = digest[-1] & 15
    return str((struct.unpack(">I", digest[offset:offset + 4])[0] & 0x7fffffff) % 1000000).zfill(6)


def main():
    if os.environ.get("COVE_BROWSER_LOCK_OWNER") != str(os.getppid()):
        raise RuntimeError("Run python scripts/test-browser.py --identity under its shared fixture lock")
    origin = os.environ["COVE_BROWSER_URL"]
    if not re.fullmatch(r"https://localhost:[0-9]+", origin) or not re.fullmatch(r"cove-browser-[0-9a-f]{12}-db", os.environ["COVE_BROWSER_DATABASE"]):
        raise RuntimeError("Owned loopback identity fixture required")
    context = ssl.create_default_context(cafile=os.environ["COVE_BROWSER_CA"])
    admin = Client(origin, context)
    admin.call("POST", "/auth/login", {"email": os.environ["COVE_BROWSER_EMAIL"], "password": os.environ["COVE_BROWSER_PASSWORD"]})
    targets, _ = admin.call("GET", "/api/settings/directory/teams")
    assert any(team["id"] == "default" for team in targets["teams"])

    config = {"display_name": "Native OpenLDAP", "host": "directory", "port": 1636, "transport": "ldaps",
        "ca_pem": os.environ["COVE_IDENTITY_LDAP_CA"], "bind_dn": "cn=service,dc=example,dc=invalid",
        "bind_password": os.environ["COVE_IDENTITY_LDAP_PASSWORD"], "user_base_dn": "ou=users,dc=example,dc=invalid",
        "user_filter": "(&(objectClass=inetOrgPerson)(uid={username}))", "subject_attribute": "entryUUID", "email_attribute": "mail",
        "name_attribute": "cn", "group_base_dn": "ou=groups,dc=example,dc=invalid", "group_filter": "(&(objectClass=groupOfNames)(member={dn}))", "allow_registration": True}
    mappings = [{"group": "cn=engineering,ou=groups,dc=example,dc=invalid", "team_id": "default", "role": "editor"}]
    saved, _ = admin.call("POST", "/api/settings/ldap/providers", {"configuration": config, "enabled": True, "mappings": mappings})
    provider = saved["id"]
    admin.call("POST", f"/api/settings/ldap/providers/{provider}/test")
    admin.call("PUT", "/api/settings/sign-in", {"password_login_disabled": True, "oidc_registration_enabled": False, "oidc_auto_redirect": ""})
    methods, _ = admin.call("GET", "/auth/sign-in-methods")
    assert not methods["password_enabled"] and methods["ldap_enabled"]
    alice = Client(origin, context)
    login, _ = alice.call("POST", f"/auth/ldap/{provider}/sign-in", {"email": "alice", "password": config["bind_password"]})
    alice_id = login["user"]
    alice.call("GET", "/api/session")
    alice.call("POST", f"/auth/ldap/{provider}/sign-in", {"email": "alice", "password": ""}, 400)
    admin.call("PUT", "/api/settings/sign-in", {"password_login_disabled": False, "oidc_registration_enabled": False, "oidc_auto_redirect": ""})
    enrollment, _ = alice.call("POST", "/api/account/totp", {}, 201)
    secret = urllib.parse.parse_qs(urllib.parse.urlsplit(enrollment["provisioning_uri"]).query)["secret"][0]
    confirmation, _ = alice.call("POST", "/api/account/totp/confirm", {"code": totp(secret)})
    alice.call("POST", "/auth/logout", {})
    login, _ = alice.call("POST", f"/auth/ldap/{provider}/sign-in", {"email": "alice", "password": config["bind_password"]})
    assert login["second_factor_required"]
    alice.call("GET", "/api/workloads", expected=401)
    # Confirmation already consumed this TOTP time step; recovery is a real single-use second factor too.
    alice.call("POST", "/auth/second-factor", {"code": confirmation["recovery_codes"][0]})
    alice.call("GET", "/api/session")
    print("PASS packaged verified LDAP, local-password policy independence and MFA", flush=True)

    token, _ = admin.call("POST", "/api/settings/scim/token", {"version": 0, "confirm": True})
    admin.call("PUT", "/api/settings/scim", {"version": token["version"], "enabled": True, "oidc_provider_id": ""})
    scim = Client(origin, context, token["token"])
    user, _ = scim.call("POST", "/scim/v2/Users", {"schemas": ["urn:ietf:params:scim:schemas:core:2.0:User"],
        "userName": "native-scim@example.invalid", "active": True, "displayName": "Native SCIM fixture"}, 201)
    group, _ = scim.call("POST", "/scim/v2/Groups", {"schemas": ["urn:ietf:params:scim:schemas:core:2.0:Group"],
        "displayName": "Native SCIM group", "members": [{"value": user["id"]}]}, 201)
    patch = lambda path, value: {"schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"], "Operations": [{"op": "Replace", "path": path, "value": value}]}
    scim.call("PATCH", "/scim/v2/Groups/" + group["id"], patch("displayName", "Updated native group"), 204)
    body = {"group_version": 1, "confirm": True, "mappings": [{"team_id": "default", "role": "viewer"}]}
    admin.call("PUT", f"/api/settings/scim/groups/{group['id']}/mappings", body, 409)
    body["group_version"] = 2
    admin.call("PUT", f"/api/settings/scim/groups/{group['id']}/mappings", body)
    scim.call("PATCH", "/scim/v2/Groups/" + group["id"], patch("displayName", "Stale"), 412, {"If-Match": 'W/"1"'})
    scim.call("PATCH", "/scim/v2/Users/" + user["id"], patch("active", False))
    users, _ = admin.call("GET", "/api/users")
    assert next(item for item in users if item["id"] == user["id"])["disabled"]
    activity, _ = admin.call("GET", "/api/settings/scim/activity")
    assert any(item["outcome"] == "failed" and item["status"] == 412 for item in activity["activity"])
    replacement, _ = admin.call("POST", "/api/settings/scim/token", {"version": 2, "confirm": True})
    scim.call("GET", "/scim/v2/Users", expected=401)
    Client(origin, context, replacement["token"]).call("GET", "/scim/v2/Users")
    print("PASS packaged SCIM rotation, stale mapping, activity and deprovisioning", flush=True)

    source, _ = admin.call("POST", "/api/settings/github-team-sync", {"name": "Native GitHub source", "mode": "oauth", "account": "acme",
        "enabled": False, "mappings": [{"group": "engineering", "team_id": "default", "role": "editor"}]})
    start, _ = admin.call("POST", "/auth/github/start", {"link": True, "current_password": os.environ["COVE_BROWSER_PASSWORD"]})
    state = urllib.parse.parse_qs(urllib.parse.urlsplit(start["url"]).query)["state"][0]
    _, headers = admin.call("GET", "/auth/github/callback?state=" + urllib.parse.quote(state) + "&code=fixture", expected=303)
    assert headers["Location"] == "/admin?github_team_sync=connected#github-team-sync"
    sources, _ = admin.call("GET", "/api/settings/github-team-sync")
    version = next(item for item in sources["sources"] if item["id"] == source["id"])["version"]
    path = "/api/settings/github-team-sync/" + source["id"]
    preview, _ = admin.call("POST", path + "/preview", {"version": version})
    assert any(change.get("email") == "github-member@example.invalid" for change in preview["changes"])
    admin.call("POST", path + "/reconcile", {"version": version, "roster_hash": preview["roster_hash"], "confirm": True})
    control = Client(os.environ["COVE_IDENTITY_FIXTURE_URL"], context, os.environ["COVE_IDENTITY_FIXTURE_TOKEN"])
    control.call("POST", "/fixture/roster", {"members": []})
    removal, _ = admin.call("POST", path + "/preview", {"version": version})
    assert any(change["after"] is None for change in removal["changes"])
    control.call("POST", "/fixture/roster", {"members": [{"id": 101, "login": "fixture-member"}]})
    admin.call("POST", path + "/reconcile", {"version": version, "roster_hash": removal["roster_hash"], "confirm": True}, 409)
    control.call("POST", "/fixture/roster", {"members": []})
    admin.call("POST", path + "/reconcile", {"version": version, "roster_hash": removal["roster_hash"], "confirm": True})
    print("PASS packaged GitHub TLS OAuth, review labels, stale preview and removal", flush=True)

    members, _ = admin.call("GET", "/api/teams/default/members")
    assert next(member for member in members if member["user_id"] == alice_id)["managed_by"][0]["display_name"] == "Native OpenLDAP"
    admin.call("PUT", f"/api/teams/default/members/{alice_id}", {"role": "owner", "grants": []})
    config.pop("bind_password")
    admin.call("PUT", f"/api/settings/ldap/providers/{provider}", {"configuration": config, "enabled": True, "version": 1, "mappings": []})
    alice.call("POST", f"/auth/ldap/{provider}/sign-in", {"email": "alice", "password": os.environ["COVE_IDENTITY_LDAP_PASSWORD"]})
    members, _ = admin.call("GET", "/api/teams/default/members")
    owner = next(member for member in members if member["user_id"] == alice_id)
    assert owner["role"] == "owner" and owner["managed_by"] == []
    print("PASS packaged provenance and retained local owner", flush=True)


if __name__ == "__main__":
    main()
