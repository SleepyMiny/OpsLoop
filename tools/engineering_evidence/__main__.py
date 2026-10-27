"""Only seal observations after their read path and external hashes are approved."""

import argparse
import json
import sys

from .seal import EvidenceError, build_bundle, seal_bundle


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for flag in ("capture-dir", "capture-sha256", "authorization-path", "authorization-sha256",
                 "provenance-path", "provenance-sha256", "repository-root", "output"):
        p.add_argument("--" + flag, required=True)
    args = vars(p.parse_args())
    output = args.pop("output")
    try:
        bundle = build_bundle(**args)
        result = seal_bundle(bundle, output)
    except (EvidenceError, OSError, KeyError, TypeError, ValueError) as exc:
        # Exception text is our bounded diagnostics, not raw objects/credentials.
        print(json.dumps({"phase": "BLOCKED_EVIDENCE", "error_type": type(exc).__name__}), file=sys.stderr)
        return 2
    print(json.dumps({"phase": "ENGINEERING_QUEUED", **result}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
