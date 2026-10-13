"""Host-side checks for the Hermes adapter and its recorder filter."""

import http.client
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import shlex
import subprocess
import tempfile
import threading
import unittest

import yaml
from harbor.models.job.config import JobConfig

from evals import harbor_hermes, hermes_wire_filter
from evals.harbor_wire import MODEL


class _Upstream(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    seen: list[tuple[str, str, bytes, str | None]] = []

    def log_message(self, format: str, *args: object) -> None:
        pass

    def do_HEAD(self) -> None:
        self.seen.append((self.command, self.path, b"", None))
        self.send_response(200)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_POST(self) -> None:
        body = self.rfile.read(int(self.headers["Content-Length"]))
        self.seen.append((self.command, self.path, body, self.headers.get("anthropic-beta")))
        # A chunked event stream, like an upstream Messages response.
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        for frame in (b"event: message_start\ndata: {}\n\n", b"event: message_stop\ndata: {}\n\n"):
            self.wfile.write(b"%x\r\n%s\r\n" % (len(frame), frame))
        self.wfile.write(b"0\r\n\r\n")

    def do_GET(self) -> None:
        self.seen.append((self.command, self.path, b"", None))
        self.send_response(500)
        self.send_header("Content-Length", "0")
        self.end_headers()


class FilterTests(unittest.TestCase):
    def setUp(self) -> None:
        _Upstream.seen = []
        self.upstream = ThreadingHTTPServer(("127.0.0.1", 0), _Upstream)
        threading.Thread(target=self.upstream.serve_forever, daemon=True).start()
        self.directory = tempfile.TemporaryDirectory()
        root = Path(self.directory.name)
        self.probe_log = root / "probes.jsonl"
        self.filter = hermes_wire_filter.serve(
            f"http://127.0.0.1:{self.upstream.server_address[1]}/api", str(root / "port"), str(self.probe_log)
        )
        threading.Thread(target=self.filter.serve_forever, daemon=True).start()
        self.port = int((root / "port").read_text())

    def tearDown(self) -> None:
        self.filter.shutdown()
        self.upstream.shutdown()
        self.filter.server_close()
        self.upstream.server_close()
        self.directory.cleanup()

    def _request(self, method: str, path: str, body: bytes | None = None) -> tuple[int, bytes]:
        connection = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        headers = {"anthropic-beta": "interleaved-thinking-2025-05-14", "x-api-key": "private-loopback-only"}
        connection.request(method, path, body=body, headers=headers)
        response = connection.getresponse()
        payload = response.read()
        connection.close()
        return response.status, payload

    def test_messages_and_warmups_pass_through_unchanged(self) -> None:
        body = json.dumps({"model": MODEL, "messages": [{"role": "user", "content": "x"}]}).encode()
        status, payload = self._request("POST", "/api/v1/messages?beta=true", body)
        self.assertEqual(status, 200)
        self.assertEqual(payload, b"event: message_start\ndata: {}\n\nevent: message_stop\ndata: {}\n\n")
        self.assertEqual(self._request("HEAD", "/api")[0], 200)
        self.assertEqual(
            _Upstream.seen,
            [("POST", "/api/v1/messages?beta=true", body, "interleaved-thinking-2025-05-14"), ("HEAD", "/api", b"", None)],
        )
        self.assertFalse(self.probe_log.exists())

    def test_local_server_probes_never_reach_the_recorder(self) -> None:
        probes = [("GET", "/api/v1/models"), ("GET", "/api/api/tags"), ("GET", "/api/props"), ("GET", "/api/version"),
                  ("POST", "/api/v1/chat/completions"), ("POST", "/api/v1/responses"), ("HEAD", "/api/v1/models")]
        for method, path in probes:
            with self.subTest(method=method, path=path):
                self.assertEqual(self._request(method, path, b"{}" if method == "POST" else None)[0], 404)
        self.assertEqual(_Upstream.seen, [])
        logged = [json.loads(line) for line in self.probe_log.read_text().splitlines()]
        self.assertEqual([(entry["method"], entry["target"]) for entry in logged], probes)

    def test_only_the_loopback_messages_base_is_accepted(self) -> None:
        for base in ("http://127.0.0.1:1/api/v1", "http://localhost:1/api", "https://openrouter.ai/api"):
            with self.subTest(base=base), self.assertRaises(ValueError):
                hermes_wire_filter.serve(base, "unused", "unused")


class AdapterTests(unittest.TestCase):
    def test_pins_model_and_version(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / "harbor-wire"
            binary.write_bytes(b"\x7fELF\x02\x01" + bytes(12) + b"\x3e\x00")
            logs = Path(directory) / "logs"
            agent = harbor_hermes.HermesAgent(logs, MODEL, wire_binary_path=binary, version="0.19.0")
            self.assertEqual(agent.version(), "0.19.0")
            with self.assertRaises(ValueError):
                harbor_hermes.HermesAgent(logs, "other/model", wire_binary_path=binary)
            with self.assertRaises(ValueError):
                harbor_hermes.HermesAgent(logs, MODEL, wire_binary_path=binary, version="0.18.0")

    def test_config_routes_the_exact_model_through_the_filter(self) -> None:
        config = yaml.safe_load(harbor_hermes.config_content())
        self.assertEqual(config["model"]["provider"], "harbor-wire")
        self.assertEqual(config["model"]["default"], MODEL)
        provider = config["providers"]["harbor-wire"]
        self.assertEqual(provider["base_url"], "${HERMES_WIRE_BASE_URL}")
        self.assertEqual(provider["api_mode"], "anthropic_messages")
        self.assertEqual(provider["key_env"], "HARBOR_WIRE_TOKEN")

    def test_launch_script_is_valid_and_takes_the_instruction_as_an_argument(self) -> None:
        script = harbor_hermes.child_command()
        subprocess.run(["bash", "-n", "-c", script], check=True)
        self.assertIn('-z "$1" --provider harbor-wire -m ' + MODEL, script)
        self.assertTrue(script.split("; ")[-1].startswith("exec /installed-agent/hermes/venv/bin/hermes "))
        install = harbor_hermes.install_command()
        subprocess.run(["bash", "-n", "-c", install], check=True)
        self.assertIn(shlex.quote("hermes-agent[anthropic]==0.19.0"), install)
        self.assertIn("--exclude-newer 2026-07-21T00:00:00Z", install)

    def test_job_selects_only_hermes(self) -> None:
        path = Path(__file__).with_name("terminal-bench-hermes.json")
        config = JobConfig.model_validate_json(path.read_text())
        self.assertEqual(config.job_name, "terminal-bench-2.0-hermes-native-0.19.0")
        self.assertEqual([agent.import_path for agent in config.agents], ["evals.harbor_hermes:HermesAgent"])


if __name__ == "__main__":
    unittest.main()
