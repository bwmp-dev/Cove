import base64
import hashlib
import hmac
import importlib.util
import json
from pathlib import Path
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location("panel_recovery", Path(__file__).with_name("panel-recovery.py"))
recovery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(recovery)


class PanelRecoveryURLTests(unittest.TestCase):
    def inspect(self, address, pinned=None):
        instance = recovery.Recovery.__new__(recovery.Recovery)
        instance.compose = ["docker", "compose"]
        instance.root = Path("unused-fixture")
        outputs = [b"fixture-container", json.dumps([{"Image": "fixture-image", "Config": {"Env": ["COVE_PUBLIC_URL=" + address]}}]).encode()]
        with mock.patch.object(recovery, "command", side_effect=outputs), mock.patch.object(recovery, "read_json", return_value=pinned), mock.patch.object(recovery, "private_json"):
            return instance.inspect()

    def test_exact_root_or_prefixed_https_url_is_pinned(self):
        for path in ("", "/control/cove", "/api", "/a_B-1"):
            url = "https://localhost:8443" + path
            with self.subTest(path=path):
                self.assertEqual(self.inspect(url), url)
                self.assertEqual(self.inspect(url, {"origin": url}), url)
        with self.assertRaisesRegex(RuntimeError, "changed"):
            self.inspect("https://localhost:8443/control/cove", {"origin": "https://localhost:8443"})

    def test_rejects_unsafe_or_ambiguous_prefixes(self):
        for url in ("http://localhost", "https://user@localhost", "https://localhost/", "https://localhost/control/", "https://localhost/a.b", "https://localhost/%61", "https://localhost/../x", "https://localhost//x", "https://localhost/healthz", "https://localhost?", "https://localhost#", "https://localhost/a\\b", "https://localhost/" + "a" * 201):
            with self.subTest(url=url), self.assertRaises(RuntimeError):
                self.inspect(url)

    def test_prefixed_request_signs_logical_path_and_never_redirects_credentials(self):
        instance = recovery.Recovery.__new__(recovery.Recovery)
        instance.root = Path("unused-fixture")
        url = "https://localhost:8443/control/cove"
        instance.environment = {"COVE_PUBLIC_URL": url, "COVE_ENCRYPTION_KEY": base64.b64encode(b"x" * 32).decode(), "COVE_ADMIN_TOKEN": "synthetic-only"}
        response = mock.MagicMock()
        response.__enter__.return_value.read.return_value = b'{"ok":true}'
        opener = mock.Mock()
        opener.open.return_value = response
        with mock.patch.object(recovery, "read_json", return_value={"origin": url}), mock.patch.object(recovery.time, "time", return_value=123), mock.patch.object(recovery.urllib.request, "build_opener", return_value=opener):
            self.assertEqual(instance.api("poll", {"agent_id": "fixture"}), {"ok": True})
        request = opener.open.call_args.args[0]
        self.assertEqual(request.full_url, url + "/api/internal/panel-host/poll")
        derived = hmac.digest(b"x" * 32, b"cove.panel.host.v1", "sha256")
        expected = hmac.new(derived, b"123\nPOST\n/api/internal/panel-host/poll\n" + request.data, hashlib.sha256).hexdigest()
        self.assertEqual(request.get_header("X-cove-host-signature"), expected)
        with self.assertRaisesRegex(RuntimeError, "redirect"):
            recovery.NoRedirect().redirect_request(None, None, 302, "", {}, "https://elsewhere.example")


if __name__ == "__main__":
    unittest.main()
