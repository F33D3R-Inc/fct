#!/usr/bin/env python3
"""A tiny mock 'brain' for the request→response service demo (examples/service.fct).

Answers every operation with a {"result": ...} envelope. Run it, then run
`facet dev examples/service.fct`:

    python3 examples/services/mock_brain.py    # listens on :9099
"""
import json
from http.server import BaseHTTPRequestHandler, HTTPServer


class Brain(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(n) or b"{}")
        op = self.path.strip("/")
        if op == "balance":
            result = 1_500_000          # µAET
        elif op == "score":
            result = len(body.get("body", "")) % 100   # a toy "risk" score
        elif op == "verify":
            # a toy identity brain: any non-empty handle "verifies" to a UUID
            h = body.get("handle", "")
            result = ("PIAL-" + h + "-uuid") if h else ""
        else:
            result = None               # review() is fire-and-forget
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({"result": result}).encode())

    def log_message(self, *a):          # quiet
        pass


if __name__ == "__main__":
    print("mock brain on http://localhost:9099")
    HTTPServer(("127.0.0.1", 9099), Brain).serve_forever()
