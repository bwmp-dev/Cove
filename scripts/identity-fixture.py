"""Controlled GitHub HTTPS provider behind a non-forwarding test CONNECT proxy."""
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
import ssl
import threading
from urllib.parse import parse_qs, urlsplit


class Provider(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        # OAuth callback state and credentials must not enter fixture logs.
        return

    def reply(self, status, body):
        encoded = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(encoded)

    def body(self):
        length = int(self.headers.get("Content-Length", "0"))
        if length < 0 or length > 1024 * 1024:
            raise ValueError("invalid fixture body size")
        return self.rfile.read(length)

    def authorized(self, token):
        return hmac.compare_digest(self.headers.get("Authorization", ""), "Bearer " + token)

    def do_GET(self):
        path = urlsplit(self.path)
        if not self.authorized(self.server.access_token):
            self.reply(401, {"message": "Fixture credential required"})
            return
        if path.path == "/user":
            self.reply(200, {"id": 100, "login": "fixture-owner"})
        elif path.path == "/user/emails":
            self.reply(200, [{"email": "admin@example.invalid", "verified": True, "primary": True}])
        elif path.path == "/orgs/acme/teams/engineering/members":
            query = parse_qs(path.query)
            try:
                page = int(query.get("page", ["1"])[0])
                count = int(query.get("per_page", ["100"])[0])
                if page < 1 or count != 100:
                    raise ValueError("invalid page")
            except ValueError:
                self.reply(400, {"message": "Invalid fixture page"})
                return
            with self.server.state_lock:
                members = self.server.members[(page - 1) * count:page * count]
            self.reply(200, members)
        else:
            self.reply(404, {"message": "Unexpected fixture provider endpoint"})

    def do_POST(self):
        if urlsplit(self.path).path != "/login/oauth/access_token":
            self.reply(404, {"message": "Unexpected fixture provider endpoint"})
            return
        try:
            fields = parse_qs(self.body().decode())
            valid = fields.get("client_id") == [self.server.client_id]
            valid = valid and hmac.compare_digest(fields.get("client_secret", [""])[0], self.server.client_secret)
            valid = valid and fields.get("code") == ["fixture"] and len(fields.get("code_verifier", [""])[0]) >= 43
        except (ValueError, UnicodeError):
            valid = False
        if not valid:
            self.reply(400, {"message": "Invalid fixture OAuth exchange"})
            return
        self.reply(200, {"access_token": self.server.access_token, "token_type": "bearer", "scope": "read:user,user:email,read:org"})


class Proxy(Provider):
    def do_CONNECT(self):
        if not self.path.endswith(":443"):
            self.reply(403, {"message": "Only fixture HTTPS is supported"})
            return
        self.send_response(200, "Connection Established")
        self.end_headers()
        self.wfile.flush()
        self.close_connection = True
        try:
            secure = self.server.tls.wrap_socket(self.connection, server_side=True)
            secure.settimeout(20)
            with secure:
                Provider(secure, self.client_address, self.server)
        except (ssl.SSLError, OSError):
            return

    def do_GET(self):
        if self.path == "/fixture/health":
            self.reply(200, {"ready": True})
        else:
            self.reply(404, {"message": "Fixture control endpoint required"})

    def do_POST(self):
        if self.path != "/fixture/roster" or not self.authorized(self.server.control_token):
            self.reply(403, {"message": "Fixture control credential required"})
            return
        try:
            members = json.loads(self.body())["members"]
            if not isinstance(members, list) or len(members) > 10000:
                raise ValueError("invalid roster")
            for member in members:
                if not isinstance(member, dict) or type(member.get("id")) is not int or member["id"] <= 0 or not isinstance(member.get("login"), str) or not 1 <= len(member["login"]) <= 128:
                    raise ValueError("invalid member")
        except (ValueError, KeyError, TypeError):
            self.reply(400, {"message": "A bounded fixture roster is required"})
            return
        with self.server.state_lock:
            self.server.members = [{"id": member["id"], "login": member["login"]} for member in members]
        self.reply(200, {"ok": True})


def main():
    server = ThreadingHTTPServer(("0.0.0.0", 8080), Proxy)
    server.members = [{"id": 101, "login": "fixture-member"}]
    server.state_lock = threading.Lock()
    server.access_token = os.environ["COVE_IDENTITY_ACCESS_TOKEN"]
    server.control_token = os.environ["COVE_IDENTITY_FIXTURE_TOKEN"]
    server.client_id = os.environ["COVE_IDENTITY_CLIENT_ID"]
    server.client_secret = os.environ["COVE_IDENTITY_CLIENT_SECRET"]
    server.tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    server.tls.minimum_version = ssl.TLSVersion.TLSv1_2
    server.tls.load_cert_chain("/fixture/github.pem", "/fixture/github.key")

    def only_fixture_names(_connection, name, _context):
        if name not in ("api.github.com", "github.com"):
            raise ssl.SSLError("Unexpected fixture TLS server name")

    server.tls.set_servername_callback(only_fixture_names)
    server.serve_forever()


if __name__ == "__main__":
    main()
