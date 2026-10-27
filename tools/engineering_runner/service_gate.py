"""Trusted pre-exec gate: workload starts only after host confirms cgroup limits."""
import os
from pathlib import Path
import sys
import time


def main():
    release = Path(sys.argv[1])
    command = sys.argv[2:]
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        if release.is_file() and release.read_bytes() == b"release\n":
            os.execv(command[0], command)
        time.sleep(0.02)
    raise SystemExit("cgroup confirmation was not received; workload not started")


if __name__ == "__main__":
    main()
