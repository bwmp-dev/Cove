import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def deploy(environment, opener=None, clock=time.monotonic, pause=time.sleep):
    url = environment.get("COVE_DEPLOY_HOOK_URL", "")
    token = environment.get("COVE_DEPLOY_HOOK_TOKEN", "")
    delivery = environment.get("COVE_DEPLOY_DELIVERY", "")
    revision = environment.get("COVE_DEPLOY_REVISION", "")
    parsed = urllib.parse.urlsplit(url)
    if (parsed.scheme != "https" or not parsed.hostname or parsed.username
            or parsed.query or parsed.fragment
            or not re.fullmatch(r"(?:/[A-Za-z0-9_-]+)*/hooks/deploy/[a-f0-9]{32}", parsed.path)):
        raise ValueError("hook-url must be a Cove HTTPS deploy-hook URL")
    if not token or "\n" in token or "\r" in token:
        raise ValueError("a deploy-hook token is required")
    if not re.fullmatch(r"[a-zA-Z0-9_-]{1,100}", delivery):
        raise ValueError("delivery-id must contain 1-100 letters, digits, hyphens or underscores")
    if revision and (not re.fullmatch(r"[a-f0-9]{40}", revision) or revision == "0" * 40):
        raise ValueError("revision must be an immutable 40-character commit SHA")
    timeout = int(environment.get("COVE_DEPLOY_TIMEOUT", "600"))
    if not 10 <= timeout <= 3600:
        raise ValueError("timeout-seconds must be between 10 and 3600")
    client = opener or urllib.request.build_opener(NoRedirect())
    deadline = clock() + timeout

    def request(method, endpoint, data=None):
        payload = None if data is None else json.dumps(data).encode()
        req = urllib.request.Request(endpoint, data=payload, method=method, headers={
            "Authorization": "Bearer " + token, "Content-Type": "application/json"})
        while True:
            remaining = deadline - clock()
            if remaining <= 0:
                raise TimeoutError("Cove deployment wait timed out")
            delay = 2
            try:
                with client.open(req, timeout=min(15, remaining)) as response:
                    raw = response.read(1024 * 1024 + 1)
                    if len(raw) > 1024 * 1024:
                        raise ValueError("Cove returned an oversized response")
                    return json.loads(raw)
            except urllib.error.HTTPError as error:
                status = error.code
                if status == 429:
                    try:
                        delay = min(60, max(1, int(error.headers.get("Retry-After", "2"))))
                    except (ValueError, AttributeError):
                        delay = 2
                error.close()
                if status not in {429, 500, 502, 503, 504}:
                    raise RuntimeError("Cove request failed with HTTP " + str(status)) from None
            except (urllib.error.URLError, OSError):
                pass
            pause(min(delay, max(0, deadline - clock())))

    result = request("POST", url, {"delivery": delivery, "revision": revision})
    operation = result.get("operation_id", "")
    if not re.fullmatch(r"[a-f0-9]{32}", operation):
        raise ValueError("Cove returned an invalid operation ID")
    output = environment.get("GITHUB_OUTPUT")
    if output:
        with open(output, "a", encoding="utf-8") as stream:
            stream.write("operation-id=" + operation + "\n")
    previous = ""
    while clock() < deadline:
        receipt = request("GET", url + "/deliveries/" + urllib.parse.quote(delivery, safe=""))
        if receipt.get("operation_id") != operation:
            raise ValueError("Cove returned a different delivery operation")
        state = receipt.get("state")
        if state not in {"pending", "queued", "running", "succeeded", "failed", "interrupted", "cancelled", "superseded"}:
            raise ValueError("Cove returned an invalid operation state")
        if state != previous:
            print("Cove operation " + operation + ": " + state, flush=True)
            previous = state
        if state == "succeeded":
            return operation
        if state in {"failed", "interrupted", "cancelled", "superseded"}:
            raise RuntimeError("Cove deployment " + state)
        pause(min(2, max(0, deadline - clock())))
    raise TimeoutError("Cove deployment wait timed out")


if __name__ == "__main__":
    try:
        deploy(os.environ)
    except (ValueError, RuntimeError, TimeoutError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
