#!/usr/bin/env python3
"""Fake Ollama HTTP server for integration tests.

Impersonates just enough of the Ollama HTTP surface that openclaw's
ollama plugin is satisfied and an isolated cron agent turn can round-
trip through "LLM → exec tool call → tool result → LLM → final text"
without a real LLM installed on the box. Used by the Multipass and
Linode cron integration tests to avoid the ~1.3 GiB ollama tarball
download + 352 MiB model pull per run (which dominate those tiers'
runtime, especially Multipass where the upstream is ISP-bound).

Endpoints implemented (minimum the openclaw ollama plugin hits):

  GET  /api/tags        -> { models: [<configured>] }         (discovery)
  POST /api/show        -> { model_info, capabilities }       (ctx window probe)
  POST /api/pull        -> { status: "success" }              (no-op pull)
  POST /api/generate    -> single-shot generate (for curl sanity checks)
  POST /api/chat        -> NDJSON streaming response:
                             turn with no prior tool msgs -> exec tool call
                             turn with a tool msg         -> final text

The "decide based on prior tool msgs" rule is what makes the fake usable
for the full cron flow: the agent framework makes TWO /api/chat calls
per turn when tool calls are present (first returns tool_calls, agent
runs the tool, second returns the final assistant text). A stateless
"always return tool call" fake would loop forever; a stateless "always
return text" fake never exercises exec-over-node. Inspecting the
incoming messages[] for role="tool" gives us the deterministic
two-step behavior without any server-side state.

Configuration via env vars (all optional, sensible defaults for tests):

  FAKE_OLLAMA_PORT       listen port on 127.0.0.1 (default: 11499)
  FAKE_OLLAMA_MODEL      model name advertised + echoed (default: qwen2.5:0.5b)
  FAKE_OLLAMA_EXEC_CMD   command emitted in the exec tool call
                         (default: "echo fake-ollama-exec-ok && hostname")
  FAKE_OLLAMA_FINAL_TEXT assistant content for the post-tool turn
                         (default: "Exec completed.")

The port default deliberately diverges from Ollama's 11434 so this fake
can coexist with a real ollama install on the same box (useful when
iterating locally). Tests pin `baseUrl: http://localhost:<port>` in
openclaw.json; that also ensures the ollama plugin's
hasMeaningfulExplicitOllamaConfig() check trips the "non-default
baseUrl" branch and mints a synthetic local auth token — without it,
isolated cron-agent sessions 500 with "No API key found for provider
'ollama'" (see the docker tier's cron_test.go for the same gotcha).

Dependencies: Python 3.8+ stdlib only. No pip install, no venv. The
integration tests SFTP this file onto a fresh Ubuntu VM where python3
is already part of the base image.
"""
from __future__ import annotations

import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODEL = os.environ.get("FAKE_OLLAMA_MODEL", "qwen2.5:0.5b")
EXEC_CMD = os.environ.get(
    "FAKE_OLLAMA_EXEC_CMD",
    "echo fake-ollama-exec-ok && hostname",
)
FINAL_TEXT = os.environ.get("FAKE_OLLAMA_FINAL_TEXT", "Exec completed.")
PORT = int(os.environ.get("FAKE_OLLAMA_PORT", "11499"))


class Handler(BaseHTTPRequestHandler):
    # Quiet the default access log; we log one line per request to stderr
    # from our own handlers so the systemd journal shows meaningful traces
    # without the noisy "127.0.0.1 - - [...]" prefix.
    def log_message(self, fmt: str, *args: object) -> None:
        sys.stderr.write("[fake-ollama] " + (fmt % args) + "\n")

    def _read_json(self) -> dict:
        length = int(self.headers.get("Content-Length", "0") or 0)
        if length <= 0:
            return {}
        raw = self.rfile.read(length)
        try:
            return json.loads(raw.decode("utf-8"))
        except Exception:
            return {}

    def _send_json(self, status: int, body: dict) -> None:
        data = json.dumps(body).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _send_ndjson_line(self, payload: dict) -> None:
        line = (json.dumps(payload) + "\n").encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/x-ndjson")
        self.send_header("Content-Length", str(len(line)))
        self.end_headers()
        self.wfile.write(line)

    def do_GET(self) -> None:  # noqa: N802 — stdlib naming
        path = self.path.rstrip("/") or "/"
        if path == "/api/tags":
            return self._send_json(
                200,
                {
                    "models": [
                        {
                            "name": MODEL,
                            "modified_at": "2026-01-01T00:00:00Z",
                            "digest": "sha256:fake",
                            "size": 1,
                        }
                    ]
                },
            )
        if path in ("/", "/healthz"):
            return self._send_json(200, {"ok": True, "model": MODEL})
        self._send_json(404, {"error": "not found", "path": path})

    def do_POST(self) -> None:  # noqa: N802 — stdlib naming
        path = self.path.rstrip("/") or "/"
        body = self._read_json()

        if path == "/api/show":
            # context_length keys are per-architecture in real ollama;
            # the openclaw plugin just reads model_info keyed by any
            # "<arch>.context_length" so we use a generic "qwen2" prefix.
            return self._send_json(
                200,
                {
                    "model_info": {"qwen2.context_length": 32768},
                    "capabilities": [],
                },
            )
        if path == "/api/pull":
            return self._send_json(200, {"status": "success"})
        if path == "/api/generate":
            # Non-streaming generate — used by the test's own curl
            # sanity check, not by the agent. Agent uses /api/chat.
            return self._send_json(
                200,
                {"model": MODEL, "response": FINAL_TEXT, "done": True},
            )
        if path == "/api/chat":
            messages = body.get("messages") or []
            has_tool_result = any(
                isinstance(m, dict) and m.get("role") == "tool" for m in messages
            )
            if has_tool_result:
                # Second turn: tool result is back in context, return
                # a final assistant text and mark done. This is what
                # the agent surfaces as the cron run's `summary`.
                payload = {
                    "model": MODEL,
                    "message": {"role": "assistant", "content": FINAL_TEXT},
                    "done": True,
                    "done_reason": "stop",
                }
            else:
                # First turn: emit a single exec tool call with a
                # scripted command. The agent dispatches it to the
                # node pinned in tools.exec.node (scraper-node in our
                # cron manifests) over the mesh ws.
                payload = {
                    "model": MODEL,
                    "message": {
                        "role": "assistant",
                        "content": "",
                        "tool_calls": [
                            {
                                "function": {
                                    "name": "exec",
                                    "arguments": {"command": EXEC_CMD},
                                }
                            }
                        ],
                    },
                    "done": True,
                    "done_reason": "stop",
                }
            return self._send_ndjson_line(payload)

        self._send_json(404, {"error": "not found", "path": path})


def main() -> None:
    server = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    sys.stderr.write(
        f"[fake-ollama] listening on 127.0.0.1:{PORT} (model={MODEL})\n"
    )
    sys.stderr.flush()
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
