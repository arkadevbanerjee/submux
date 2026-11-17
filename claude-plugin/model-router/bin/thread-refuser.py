#!/usr/bin/env python3
"""Answers every POST with the 400 Claude Code reads as `thread_unsupported_request`.

model-router sends a threaded `kiro/*` request here. A hook cannot answer it itself (Claude Code treats a
hook-made answer as a mod refusal), so the 400 has to come over the network. Claude Code then resends the turn
with the full history and keeps that subagent stateless; the resend goes to kirocc. Loopback only.
"""

import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 3460
BODY = json.dumps(
    {
        "type": "error",
        "error": {
            "type": "invalid_request_error",
            "message": "model-router: kirocc has no server-side threads; resend this turn stateless",
            "details": {"error_code": "thread_unsupported_request"},
        },
    }
).encode()


class Handler(BaseHTTPRequestHandler):
    def _send(self, status, body, ctype):
        self.send_response(status)
        self.send_header("content-type", ctype)
        self.send_header("request-id", "model-router-thread-refuser")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self._send(200, b"model-router-thread-refuser", "text/plain")

    def do_POST(self):
        n = int(self.headers.get("content-length") or 0)
        if n:
            self.rfile.read(n)
        self._send(400, BODY, "application/json")

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
