import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parents[1]))
import manager


class ManagerTests(unittest.TestCase):
    def test_parse_vless_reality(self):
        item = manager.parse_uri(
            "vless://uuid@example.test:443?security=reality&sni=ya.ru&pbk=public&sid=short&type=grpc&serviceName=proxy",
            "node-1",
        )
        self.assertEqual(item["type"], "vless")
        self.assertEqual(item["tls"]["reality"]["public_key"], "public")
        self.assertEqual(item["transport"]["service_name"], "proxy")

    def test_decode_plain_and_base64_subscription(self):
        plain = b"vless://uuid@example.test:443\ninvalid\n"
        self.assertEqual(manager.decode_body(plain), ["vless://uuid@example.test:443"])
        encoded = b"dmxlc3M6Ly91dWlkQGV4YW1wbGUudGVzdDo0NDMK"
        self.assertEqual(manager.decode_body(encoded), ["vless://uuid@example.test:443"])

    def test_build_uses_fake_subscription_and_keeps_base_config(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            base = root / "base.json"
            output = root / "runtime" / "config.json"
            state = root / "subscriptions.json"
            base.write_text(json.dumps({
                "outbounds": [
                    {"type": "urltest", "tag": "telegram-auto", "outbounds": []},
                    {"type": "direct", "tag": "direct-eth"},
                ]
            }))
            state.write_text(json.dumps({"subscriptions": [{"name": "fixture", "url": "https://example.test/sub"}]}))
            with patch.object(manager, "BASE", base), patch.object(manager, "OUTPUT", output), patch.object(manager, "STATE", state), patch.object(manager, "fetch", return_value=b"vless://uuid@example.test:443"), patch.object(manager.subprocess, "run", return_value=type("Result", (), {"returncode": 0, "stderr": ""})()):
                result = manager.build()
            generated = json.loads(output.read_text())
            self.assertEqual(result["nodes"], 1)
            self.assertEqual(generated["outbounds"][0]["tag"], "telegram-auto")
            self.assertEqual(len(generated["outbounds"][0]["outbounds"]), 1)
            self.assertTrue(output.with_suffix(".previous.json").exists() is False)

    def test_write_state_is_readable(self):
        with tempfile.TemporaryDirectory() as directory:
            manager.STATE = Path(directory) / "subscriptions.json"
            manager.write_state({"subscriptions": [{"name": "fixture", "url": "https://example.test"}]})
            self.assertEqual(manager.read_state()["subscriptions"][0]["name"], "fixture")


if __name__ == "__main__":
    unittest.main()
