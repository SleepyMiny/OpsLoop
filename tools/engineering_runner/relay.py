"""Runs INSIDE the model namespace: TCP loopback to one mounted AF_UNIX socket."""
import os
from pathlib import Path
import socket
import socketserver
import subprocess
import sys
import threading


class Relay(socketserver.BaseRequestHandler):
    def handle(self):
        upstream = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        upstream.settimeout(65)
        upstream.connect("/broker/model.sock")

        def forward(source, target):
            try:
                while True:
                    chunk = source.recv(16384)
                    if not chunk:
                        break
                    target.sendall(chunk)
            except OSError:
                pass
            finally:
                try:
                    target.shutdown(socket.SHUT_WR)
                except OSError:
                    pass

        thread = threading.Thread(target=forward, args=(self.request, upstream), daemon=True)
        thread.start()
        forward(upstream, self.request)
        upstream.close()


def codex_command(settings, port):
    if (settings.get("model"), settings.get("reasoning_effort")) != ("gpt-6-luna", "medium"):
        raise ValueError("only gpt-6-luna with medium reasoning is approved")
    return ["/opt/codex", "--no-daemon", "-a", "never", "exec", "--ignore-user-config", "--ignore-rules",
               "--ephemeral", "--skip-git-repo-check", "--sandbox", "read-only", "--json", "--color", "never",
               "--output-schema", "/runner/proposal-schema.json", "--output-last-message", "/tmp/proposal.json",
               "--model", settings["model"], "-c", 'model_reasoning_effort="medium"',
               "-c", 'model_provider="opsloop_broker"',
               "-c", 'model_providers.opsloop_broker.name="OpsLoop credential broker"',
               "-c", 'model_providers.opsloop_broker.base_url="http://127.0.0.1:' + str(port) + '/v1"',
               "-c", 'model_providers.opsloop_broker.wire_api="responses"',
               "-c", 'model_providers.opsloop_broker.env_key="OPSLOOP_BROKER_CAPABILITY"',
               "-c", 'model_providers.opsloop_broker.requires_openai_auth=false',
               "-c", 'model_providers.opsloop_broker.request_max_retries=0',
               "-c", 'model_providers.opsloop_broker.stream_max_retries=0',
               "-c", 'shell_environment_policy.inherit="none"',
               "-c", 'web_search="disabled"', "-c", 'features.multi_agent=false', "-"]


def main():
    import json
    settings = json.loads(Path("/runner/model.json").read_text())
    Path("/tmp/codex").mkdir(mode=0o700)
    server = socketserver.ThreadingTCPServer(("127.0.0.1", 0), Relay)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    port = server.server_address[1]
    env = dict(os.environ)
    env["OPSLOOP_BROKER_CAPABILITY"] = settings["capability"]
    command = codex_command(settings, port)
    # The outer namespace/file mounts remain the authority if Codex's inner sandbox fails.
    try:
        result = subprocess.run(command, env=env, stdin=sys.stdin.buffer)
    finally:
        server.shutdown()
    if result.returncode == 0:
        proposal = Path("/tmp/proposal.json").read_bytes()
        if len(proposal) > 128 * 1024:
            raise SystemExit("model proposal exceeded byte limit")
        # The broker forbids all tool-call events, so only this trusted relay can
        # emit the completion envelope into the CLI's retained event stream.
        print(json.dumps({"opsloop_model_proposal": json.loads(proposal)}, sort_keys=True))
    raise SystemExit(result.returncode)


if __name__ == "__main__":
    main()
