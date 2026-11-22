import contextlib
import io
import json
import tempfile
import unittest
import urllib.error
from pathlib import Path

from deploy import deploy


class FakePanel:
    def __init__(self, states, error=None):
        self.states = iter(states)
        self.error = error
        self.requests = []

    def open(self, request, timeout):
        self.requests.append(request)
        if self.error:
            raise self.error
        if request.method == "POST":
            result = {"operation_id": "a" * 32}
        else:
            result = {"operation_id": "a" * 32, "state": next(self.states)}
        return io.BytesIO(json.dumps(result).encode())


class DeployActionTests(unittest.TestCase):
    def setUp(self):
        self.environment = {
            "COVE_DEPLOY_HOOK_URL": "https://panel.example.com/hooks/deploy/" + "b" * 32,
            "COVE_DEPLOY_HOOK_TOKEN": "fixture-secret-token",
            "COVE_DEPLOY_DELIVERY": "workflow-123-1",
            "COVE_DEPLOY_REVISION": "c" * 40,
            "COVE_DEPLOY_TIMEOUT": "10",
        }
        self.elapsed = 0

    def pause(self, seconds):
        self.elapsed += seconds

    def run_deploy(self, panel):
        return deploy(self.environment, opener=panel, clock=lambda: self.elapsed, pause=self.pause)

    def test_trigger_wait_and_output(self):
        panel = FakePanel(["pending", "queued", "running", "succeeded"])
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "output"
            self.environment["GITHUB_OUTPUT"] = str(output)
            with contextlib.redirect_stdout(io.StringIO()) as captured:
                self.assertEqual(self.run_deploy(panel), "a" * 32)
            self.assertEqual(output.read_text(), "operation-id=" + "a" * 32 + "\n")
        body = json.loads(panel.requests[0].data)
        self.assertEqual(body, {"delivery": "workflow-123-1", "revision": "c" * 40})
        for request in panel.requests:
            self.assertEqual(request.get_header("Authorization"), "Bearer fixture-secret-token")
            self.assertNotIn("fixture-secret-token", request.full_url)
        self.assertNotIn("fixture-secret-token", captured.getvalue())

    def test_lost_trigger_reply_reuses_delivery(self):
        class AmbiguousPanel(FakePanel):
            def open(self, request, timeout):
                if not self.requests:
                    self.requests.append(request)
                    raise urllib.error.URLError("fixture response lost after acceptance")
                return super().open(request, timeout)

        panel = AmbiguousPanel(["succeeded"])
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(self.run_deploy(panel), "a" * 32)
        triggers = [request for request in panel.requests if request.method == "POST"]
        self.assertEqual(len(triggers), 2)
        self.assertEqual(triggers[0].data, triggers[1].data)

    def test_terminal_failures(self):
        for state in ("failed", "interrupted", "cancelled", "superseded"):
            with self.subTest(state=state), contextlib.redirect_stdout(io.StringIO()):
                with self.assertRaisesRegex(RuntimeError, state):
                    self.run_deploy(FakePanel([state]))

    def test_rejects_redirect_and_hides_response_body(self):
        panel = FakePanel([], urllib.error.HTTPError(self.environment["COVE_DEPLOY_HOOK_URL"], 302,
                         "fixture-secret-token", {}, io.BytesIO(b"fixture-secret-token")))
        with self.assertRaisesRegex(RuntimeError, "HTTP 302") as error:
            self.run_deploy(panel)
        self.assertNotIn("fixture-secret-token", str(error.exception))
        self.assertEqual(len(panel.requests), 1)

    def test_timeout_and_unknown_state(self):
        with contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(TimeoutError):
                self.run_deploy(FakePanel(["running"] * 5))
            self.elapsed = 0
            with self.assertRaisesRegex(ValueError, "invalid operation state"):
                self.run_deploy(FakePanel(["unexpected"]))

    def test_invalid_inputs_make_no_requests(self):
        for field, value in (("COVE_DEPLOY_HOOK_URL", "http://panel.example.com/hooks/deploy/" + "b" * 32),
                             ("COVE_DEPLOY_REVISION", "0" * 40),
                             ("COVE_DEPLOY_DELIVERY", "invalid/delivery"),
                             ("COVE_DEPLOY_TIMEOUT", "3601")):
            with self.subTest(field=field):
                environment = dict(self.environment)
                environment[field] = value
                panel = FakePanel([])
                with self.assertRaises(ValueError):
                    deploy(environment, opener=panel)
                self.assertEqual(panel.requests, [])


if __name__ == "__main__":
    unittest.main()
