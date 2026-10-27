"""Single-run credential broker. Only fixed Responses destinations are reachable.

The model sandbox sees a random capability, never the provider credential. ChatGPT
Codex authentication has no documented per-request token hard cap; its hard usage
limits here are requests, input/output bytes and wall time, not billed tokens.
"""
from __future__ import annotations

from dataclasses import dataclass
import http.client
from http.server import BaseHTTPRequestHandler
import json
from pathlib import Path
import secrets
import socket
import socketserver
import ssl
import threading
import time

from .contracts import Rejected, canonical, regular_file


@dataclass(frozen=True)
class BrokerLimits:
    requests: int = 1
    request_bytes: int = 512 * 1024
    total_input_bytes: int = 3 * 1024 * 1024
    total_output_bytes: int = 2 * 1024 * 1024
    output_tokens_per_request: int = 4096
    wall_seconds: int = 300

    def validate(self):
        if not (1 <= self.requests <= 16 and 1024 <= self.request_bytes <= 512 * 1024
                and self.request_bytes <= self.total_input_bytes <= 4 * 1024**2
                and 1024 <= self.total_output_bytes <= 4 * 1024**2
                and 1 <= self.output_tokens_per_request <= 8192 and 1 <= self.wall_seconds <= 600):
            raise Rejected("broker limits outside trusted maximum")


class ModelBroker:
    def __init__(self, directory: Path, auth_path: Path, model: str, mode: str,
                 limits: BrokerLimits = BrokerLimits(), *, reasoning_effort: str = "medium"):
        limits.validate()
        if mode not in ("chatgpt", "api") or not model or len(model) > 80:
            raise Rejected("unsupported model or authentication mode")
        if reasoning_effort != "medium":
            raise Rejected("only the approved medium reasoning effort is allowed")
        self.directory, self.model, self.mode, self.limits = directory, model, mode, limits
        self.reasoning_effort = reasoning_effort
        self.capability = secrets.token_urlsafe(32)
        self._auth = self._read_auth(auth_path)
        self.started = time.monotonic()
        self.requests = self.connections = self.input_bytes = self.output_bytes = 0
        self.errors = {}
        self._lock = threading.Lock()
        self._server = self._thread = None

    def _read_auth(self, path):
        try:
            auth = json.loads(regular_file(path, 64 * 1024))
            if self.mode == "chatgpt":
                token = auth["tokens"]["access_token"]
                account = auth["tokens"]["account_id"]
                if not isinstance(token, str) or not token or not isinstance(account, str) or not account:
                    raise ValueError("missing credential")
                return (token, account)
            token = auth["OPENAI_API_KEY"]
            if not isinstance(token, str) or not token:
                raise ValueError("missing credential")
            return (token, None)
        except (KeyError, TypeError, ValueError) as exc:
            raise Rejected("provider credential unavailable; values withheld") from exc

    def __enter__(self):
        self.directory.mkdir(mode=0o700, parents=True, exist_ok=False)
        broker = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.0"

            def setup(self):
                self.request.settimeout(10)
                super().setup()
                self.rfile = LimitedReader(self.rfile, broker.limits.request_bytes + 64 * 1024)

            def log_message(self, *args):
                pass  # Never write credentials, prompts or URLs to a server log.

            def do_POST(self):
                broker.handle(self)

        class Server(socketserver.UnixStreamServer):
            def get_request(self):
                connection, address = super().get_request()
                broker.connections += 1
                if broker.connections > broker.limits.requests + 2 or time.monotonic() - broker.started >= broker.limits.wall_seconds:
                    connection.close()
                    # Stop accepting rather than processing an unbounded stream of
                    # rejected sockets on the trusted host broker.
                    self._BaseServer__shutdown_request = True
                    self.socket.close()
                    raise OSError("broker transport budget exhausted")
                return connection, address

            def handle_error(self, *unused):
                broker.record_error("handler_error")

        self._server = Server(str(self.directory / "model.sock"), Handler)
        self._server.timeout = 1
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()
        return self

    def __exit__(self, *unused):
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=3)
        self._auth = None

    def handle(self, handler):
        try:
            if handler.path != "/v1/responses" or handler.headers.get("Transfer-Encoding"):
                raise Rejected("endpoint not allowed")
            if handler.headers.get_all("Authorization") != ["Bearer " + self.capability]:
                raise Rejected("broker capability invalid")
            lengths = handler.headers.get_all("Content-Length")
            if not lengths or len(lengths) != 1 or not lengths[0].isdigit():
                raise Rejected("content length required")
            size = int(lengths[0])
            if not 0 < size <= self.limits.request_bytes:
                raise Rejected("request size limit")
            handler.connection.settimeout(10)
            data = handler.rfile.read(size)
            if len(data) != size:
                raise Rejected("incomplete request")
            body = json.loads(data)
            if not isinstance(body, dict) or body.get("model") != self.model:
                raise Rejected("model is not approved")
            reasoning = body.get("reasoning")
            if not isinstance(reasoning, dict) or reasoning.get("effort") != self.reasoning_effort:
                raise Rejected("reasoning effort is not approved")
            # The approved mode is a code proposal, not an agent tool session. Model
            # tool declarations from the CLI are removed at this trusted boundary.
            body["tools"] = []
            body["tool_choice"] = "none"
            body["parallel_tool_calls"] = False
            if self.mode == "api":
                body["max_output_tokens"] = min(int(body.get("max_output_tokens", self.limits.output_tokens_per_request)),
                                                self.limits.output_tokens_per_request)
                if body["max_output_tokens"] <= 0:
                    raise Rejected("output budget invalid")
            body["store"] = False
            body["stream"] = True
            encoded = canonical(body)
            with self._lock:
                if (time.monotonic() - self.started >= self.limits.wall_seconds
                        or self.requests >= self.limits.requests
                        or self.input_bytes + len(encoded) > self.limits.total_input_bytes
                        or self.output_bytes >= self.limits.total_output_bytes):
                    raise Rejected("model communications budget exhausted")
                self.requests += 1
                self.input_bytes += len(encoded)
            self._forward(handler, encoded)
        except (Rejected, ValueError, TypeError, OSError) as exc:
            # All failures are bounded metadata; never include provider response/auth content.
            self.record_error(type(exc).__name__)
            try:
                handler.send_error(403, "OpsLoop model broker rejected request")
            except OSError:
                pass

    def _forward(self, handler, encoded):
        host, path = ("chatgpt.com", "/backend-api/codex/responses") if self.mode == "chatgpt" else ("api.openai.com", "/v1/responses")
        remaining = max(1, self.limits.wall_seconds - int(time.monotonic() - self.started))
        connection = http.client.HTTPSConnection(host, timeout=min(15, remaining), context=ssl.create_default_context())
        headers = {"Authorization": "Bearer " + self._auth[0], "Content-Type": "application/json",
                   "Accept": "text/event-stream", "User-Agent": "codex_cli_rs/0.164.0 (OpsLoop broker)",
                   "originator": "codex_cli_rs"}
        if self._auth[1]:
            headers["ChatGPT-Account-Id"] = self._auth[1]
        try:
            # http.client ignores proxy environment variables and never follows redirects.
            connection.request("POST", path, body=encoded, headers=headers)
            upstream = connection.getresponse()
            if upstream.status != 200:
                self.record_error("upstream_status_" + str(upstream.status))
                handler.send_error(502, "Provider request did not succeed; response body withheld")
                return
            handler.send_response(200)
            handler.send_header("Content-Type", "text/event-stream")
            handler.send_header("Connection", "close")
            handler.end_headers()
            pending = b""
            while True:
                if time.monotonic() - self.started >= self.limits.wall_seconds:
                    raise Rejected("model stream deadline")
                available = self.limits.total_output_bytes - self.output_bytes
                if available <= 0:
                    raise Rejected("model response byte budget exhausted")
                chunk = upstream.read1(min(8192, available + 1))
                if not chunk:
                    if pending.strip():
                        raise Rejected("incomplete provider event stream")
                    break
                if len(chunk) > available:
                    raise Rejected("model response byte budget exhausted")
                self.output_bytes += len(chunk)
                pending = (pending + chunk).replace(b"\r\n", b"\n")
                if len(pending) > self.limits.total_output_bytes:
                    raise Rejected("provider event too large")
                # Inspect complete events before exposing them to the CLI. Even an
                # unexpected provider tool-call response cannot invoke a local tool.
                while b"\n\n" in pending:
                    event, pending = pending.split(b"\n\n", 1)
                    reject_tool_event(event)
                    handler.wfile.write(event + b"\n\n")
                    handler.wfile.flush()
        finally:
            connection.close()

    def summary(self):
        return {"mode": self.mode, "model": self.model, "reasoning_effort": self.reasoning_effort,
                "requests": self.requests, "connections": self.connections,
                "input_bytes": self.input_bytes, "output_bytes": self.output_bytes,
                "limits": self.limits.__dict__, "errors": self.errors,
                "provider_credentials_in_sandbox": False,
                "token_hard_cap": self.mode == "api",
                "usage_cap_units": ["requests", "input_bytes", "output_bytes", "wall_seconds"]}

    def record_error(self, name):
        # Fixed local labels/status codes only; memory remains bounded under abuse.
        if name not in self.errors and len(self.errors) >= 16:
            name = "other"
        self.errors[name] = min(1000, self.errors.get(name, 0) + 1)


class LimitedReader:
    """Bound all bytes before BaseHTTPRequestHandler parses request headers/body."""
    def __init__(self, source, remaining):
        self.source, self.remaining = source, remaining

    def _take(self, method, size):
        if self.remaining <= 0:
            raise OSError("broker input byte budget exhausted")
        size = self.remaining + 1 if size < 0 else min(size, self.remaining + 1)
        data = method(size)
        self.remaining -= len(data)
        if self.remaining < 0:
            raise OSError("broker input byte budget exhausted")
        return data

    def read(self, size=-1):
        return self._take(self.source.read, size)

    def readline(self, size=-1):
        return self._take(self.source.readline, size)

    def close(self):
        self.source.close()


def reject_tool_event(event: bytes):
    data = b"\n".join(line[5:].lstrip() for line in event.splitlines() if line.startswith(b"data:"))
    if not data or data == b"[DONE]":
        return
    try:
        value = json.loads(data)
    except ValueError as exc:
        raise Rejected("invalid provider event") from exc
    if not isinstance(value, dict):
        raise Rejected("invalid provider event object")
    kind = value.get("type", "")
    if "function_call" in kind or "custom_tool" in kind or "web_search" in kind:
        raise Rejected("provider tool call is forbidden")
    items = [value.get("item", {})] + value.get("response", {}).get("output", [])
    for item in items:
        if item and item.get("type") not in ("message", "reasoning"):
            raise Rejected("provider tool or unknown output item is forbidden")
