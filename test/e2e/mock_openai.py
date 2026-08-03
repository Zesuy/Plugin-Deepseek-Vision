#!/usr/bin/env python3
"""Small, dependency-free OpenAI-compatible VLM/upstream fixture for host E2E.

The fixture intentionally keeps request bodies in a state file so the shell
driver can assert what crossed the real CLIProxyAPI process boundary. It never
prints request data or authorization headers.
"""

from __future__ import annotations

import argparse
import json
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


class State:
    def __init__(self, path: Path, role: str, mode_file: Path | None):
        self.path = path
        self.role = role
        self.mode_file = mode_file
        self.lock = threading.Lock()
        self.requests: list[dict] = []

    def mode(self) -> str:
        if self.mode_file is None:
            return "ok"
        try:
            return self.mode_file.read_text(encoding="utf-8").strip() or "ok"
        except OSError:
            return "ok"

    def add(self, path: str, body: bytes) -> None:
        try:
            payload = json.loads(body.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError):
            payload = None
        # Keep only a bounded diagnostic record. This is a fixture, not a log.
        item = {"path": path, "body": payload}
        with self.lock:
            self.requests.append(item)
            self.path.write_text(json.dumps({"role": self.role, "requests": self.requests}, ensure_ascii=False), encoding="utf-8")

    def snapshot(self) -> dict:
        with self.lock:
            return {"role": self.role, "requests": list(self.requests), "mode": self.mode()}


def handler_for(state: State):
    class Handler(BaseHTTPRequestHandler):
        server_version = "deepseek-vision-e2e/1"

        def log_message(self, _format: str, *_args) -> None:
            # Request bodies and headers must not leak into process logs.
            return

        def _json(self, status: int, value: dict) -> None:
            raw = json.dumps(value, ensure_ascii=False).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def do_GET(self) -> None:  # noqa: N802
            if self.path in ("/healthz", "/v1/healthz"):
                self._json(200, {"ok": True})
                return
            if self.path in ("/state", "/v1/state"):
                self._json(200, state.snapshot())
                return
            self._json(404, {"error": "not_found"})

        def do_POST(self) -> None:  # noqa: N802
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            state.add(self.path, body)
            if state.mode() == "fail" and state.role == "vlm":
                self._json(502, {"error": {"type": "mock_vlm_failure", "message": "fixture failure"}})
                return
            try:
                payload = json.loads(body.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError):
                payload = {}

            if state.role == "vlm":
                analysis = "Visible text: E2E fixture text\nVisual description: E2E fixture visual analysis"
                # CLIProxyAPI's OpenAI-compatible provider translates a
                # Responses host callback to Chat Completions before it
                # reaches the mock. Return the protocol requested on the wire
                # so the host can translate it back to a Responses payload.
                if self.path.endswith("/chat/completions"):
                    self._json(200, {
                        "id": "e2e-vlm-chat",
                        "object": "chat.completion",
                        "model": payload.get("model", "gpt-5.6-luna"),
                        "choices": [{"index": 0, "message": {"role": "assistant", "content": analysis}, "finish_reason": "stop"}],
                        "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
                    })
                    return
                # Direct external-backend tests use the Responses endpoint.
                self._json(200, {
                    "id": "e2e-vlm-response",
                    "object": "response",
                    "model": payload.get("model", "gpt-5.6-luna"),
                    "output_text": analysis,
                })
                return

            # OpenAI compatibility translates Responses input to chat completions.
            stream = bool(payload.get("stream"))
            if stream:
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-cache")
                self.end_headers()
                self.wfile.write(b'data: {"id":"e2e-chat","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}\n\n')
                self.wfile.write(b'data: {"id":"e2e-chat","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}\n\n')
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()
                return
            self._json(200, {
                "id": "e2e-chat",
                "object": "chat.completion",
                "model": payload.get("model", "deepseek-v4-flash"),
                "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
            })

    return Handler


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--role", choices=("vlm", "upstream"), required=True)
    parser.add_argument("--state", type=Path, required=True)
    parser.add_argument("--mode-file", type=Path)
    args = parser.parse_args()
    args.state.parent.mkdir(parents=True, exist_ok=True)
    state = State(args.state, args.role, args.mode_file)
    args.state.write_text(json.dumps(state.snapshot()), encoding="utf-8")
    server = ThreadingHTTPServer(("127.0.0.1", args.port), handler_for(state))
    server.serve_forever()


if __name__ == "__main__":
    main()
