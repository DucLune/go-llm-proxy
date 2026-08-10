#!/usr/bin/env python3
"""Mock OpenAI-compatible chat completions server for PDF comparison testing.

The go-llm-proxy test instance points its backend at this server. When the
proxy forwards the PDF-processed request, this server records the FULL request
body verbatim (which contains the pipeline's injected <pdf_content> text) and
returns a minimal valid chat completion.

Pure stdlib — no third-party dependencies. Run:
    python mock_openai.py --port 18887 --outfile ../out/compare/captured.jsonl
"""
import argparse
import json
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Accepted paths — cover both `backend + /chat/completions` and
# `backend + /v1/chat/completions` forwarding conventions.
ACCEPTED_PREFIXES = ("/chat/completions", "/v1/chat/completions")


class MockHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write(f"[mock] {fmt % args}\n")

    def do_GET(self):
        # /v1/models — used by the proxy's context-window probe. Returning a
        # well-formed list makes the health probe succeed (and quiets 501s).
        if self.path in ("/v1/models", "/models"):
            payload = {
                "object": "list",
                "data": [{"id": "test-pdf-model", "object": "model", "owned_by": "mock"}],
            }
            self._json(200, payload)
            return
        self._json(404, {"error": {"message": f"no mock route for GET {self.path}"}})

    def do_POST(self):
        if not self.path.startswith(ACCEPTED_PREFIXES):
            self._json(404, {"error": {"message": f"no mock route for {self.path}"}})
            return

        length = int(self.headers.get("Content-Length", 0) or 0)
        body = self.rfile.read(length)

        # Record the forwarded request verbatim (JSON line) for later analysis.
        record = {
            "ts": time.strftime("%Y-%m-%dT%H:%M:%S"),
            "path": self.path,
            "auth": self.headers.get("Authorization", ""),
            "body": body.decode("utf-8", errors="replace"),
        }
        with open(self.server.outfile, "a", encoding="utf-8") as fh:
            fh.write(json.dumps(record, ensure_ascii=False) + "\n")

        # Parse the request so we can echo the model id and count messages.
        model = "unknown"
        n_messages = 0
        try:
            req = json.loads(body)
            model = req.get("model", model)
            n_messages = len(req.get("messages", []) or [])
        except Exception:
            pass

        resp = {
            "id": "mock-chatcmpl",
            "object": "chat.completion",
            "created": int(time.time()),
            "model": model,
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": "ok"},
                "finish_reason": "stop",
            }],
            "usage": {"prompt_tokens": 0, "completion_tokens": 1, "total_tokens": n_messages + 1},
        }
        self._json(200, resp)

    def _json(self, status, payload):
        data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--port", type=int, default=18887)
    ap.add_argument("--outfile", default="captured.jsonl", help="path to append captured requests to")
    args = ap.parse_args()

    srv = ThreadingHTTPServer(("127.0.0.1", args.port), MockHandler)
    srv.outfile = args.outfile
    sys.stderr.write(f"[mock] listening on 127.0.0.1:{args.port}, recording to {args.outfile}\n")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        srv.server_close()


if __name__ == "__main__":
    main()
