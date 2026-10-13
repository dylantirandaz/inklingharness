"""Pass only recordable Messages traffic from Hermes to harbor-wire.

Hermes 0.19.0 treats a loopback custom endpoint as a possible local model
server. It sends GET probes for Ollama, LM Studio, llama.cpp, and vLLM, and a
GET /models request for pricing. The probes come from vision capability checks,
agent setup, and cost estimates. No setting disables all of them. harbor-wire
treats any path other than its inference routes as a fatal recorder error.

This filter forwards POST /api/v1/messages and the root HEAD warmups byte for
byte. It answers every other request with a local 404, the same answer a server
without those local APIs gives. Each refused request is appended to the probe
log. The filter uses only the standard library and does not change Hermes.
It exits when the process that started it exits.

Usage: python hermes_wire_filter.py UPSTREAM_BASE PORT_FILE PROBE_LOG
"""

import http.client
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

FORWARDED = {("POST", "/api/v1/messages"), ("HEAD", "/api"), ("HEAD", "/api/v1")}
_HOP_BY_HOP = {
    "connection", "keep-alive", "proxy-connection", "transfer-encoding", "te",
    "trailer", "upgrade", "host", "content-length",
}


def forwarded(method: str, target: str) -> bool:
    return (method, urlsplit(target).path) in FORWARDED


class _Handler(BaseHTTPRequestHandler):
    # HTTP/1.0 closes each connection, so streamed bodies need no re-chunking.
    protocol_version = "HTTP/1.0"
    upstream: tuple[str, int] = ("127.0.0.1", 0)
    probe_log = ""
    lock = threading.Lock()

    def log_message(self, format: str, *args: object) -> None:
        pass

    def _note(self, outcome: str, detail: str = "") -> None:
        entry = {"time": time.time(), "method": self.command, "target": self.path, "outcome": outcome}
        if detail:
            entry["detail"] = detail
        with self.lock, open(self.probe_log, "a", encoding="utf-8") as log:
            log.write(json.dumps(entry) + "\n")

    def _refuse(self, status: int, outcome: str, detail: str = "") -> None:
        self._note(outcome, detail)
        body = json.dumps({"type": "error", "error": {"type": "not_found_error", "message": f"{outcome}: {self.command} {self.path}"}}).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _body(self) -> bytes:
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            parts = []
            while True:
                size = int(self.rfile.readline().split(b";", 1)[0].strip(), 16)
                if size == 0:
                    while self.rfile.readline() not in (b"\r\n", b"\n", b""):
                        pass
                    return b"".join(parts)
                parts.append(self.rfile.read(size))
                self.rfile.readline()
        return self.rfile.read(int(self.headers.get("Content-Length") or 0))

    def _handle(self) -> None:
        body = self._body() if self.command == "POST" else b""
        if not forwarded(self.command, self.path):
            self._refuse(404, "refused_unrecordable_endpoint")
            return
        headers = {name: value for name, value in self.headers.items() if name.lower() not in _HOP_BY_HOP}
        connection = http.client.HTTPConnection(*self.upstream, timeout=None)
        try:
            try:
                connection.request(self.command, self.path, body=body if self.command == "POST" else None, headers=headers)
                response = connection.getresponse()
            except OSError as error:
                self._refuse(502, "upstream_unavailable", str(error))
                return
            self.send_response(response.status, response.reason)
            for name, value in response.getheaders():
                if name.lower() not in _HOP_BY_HOP or name.lower() == "content-length":
                    self.send_header(name, value)
            self.end_headers()
            if self.command == "HEAD":
                return
            while chunk := response.read1(65536):
                self.wfile.write(chunk)
                self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass
        finally:
            connection.close()

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = do_HEAD = do_OPTIONS = _handle


def serve(upstream_base: str, port_file: str, probe_log: str) -> ThreadingHTTPServer:
    upstream = urlsplit(upstream_base)
    if upstream.scheme != "http" or upstream.hostname != "127.0.0.1" or upstream.path.rstrip("/") != "/api":
        raise ValueError(f"Expected the loopback harbor-wire Messages base, got {upstream_base!r}")
    handler = type("Handler", (_Handler,), {"upstream": (upstream.hostname, upstream.port or 80), "probe_log": probe_log})
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    server.daemon_threads = True
    temporary = f"{port_file}.tmp"
    with open(temporary, "w", encoding="utf-8") as stream:
        stream.write(f"{server.server_address[1]}\n")
    os.replace(temporary, port_file)
    return server


def main(arguments: list[str]) -> int:
    if len(arguments) != 3:
        print(__doc__.strip().splitlines()[-1], file=sys.stderr)
        return 2
    parent = os.getppid()
    server = serve(*arguments)

    def watch_parent() -> None:
        # The parent shell execs Hermes, so its PID is the Hermes process.
        while os.getppid() == parent:
            time.sleep(0.2)
        os._exit(0)

    threading.Thread(target=watch_parent, daemon=True).start()
    server.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
