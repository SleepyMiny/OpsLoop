# Local security verification

Use the official Go 1.27.3 archive in a separate directory, verified against the
SHA-256 in `https://go.dev/dl/?mode=json&include=all`. CI and the Docker builder
use this version; `go.mod` also requires it. Keep the system installation intact.
Set `PATH` and `GOROOT` to that installation, `GOTOOLCHAIN=local`, and
`GOEXPERIMENT=''`. Record `go version` and `go env -json`; do not override
`GOVERSION`. Keep HOME, GOPATH, GOMODCACHE and GOCACHE isolated. Do not inherit
cloud credentials or a real kubeconfig. Use `USE_EXISTING_CLUSTER=false` and an
invalid `KUBECONFIG` path for these local tests.

Product selection is x/net 0.60.0 and x/text 0.42.0: x/net's published go.mod
requires text 0.42.0, sys 0.48.0, term 0.46.0 and crypto 0.57.0. Text requires
sync 0.23.0, tools 0.49.0 and mod 0.41.0 in the full selected module graph.
Tools/mod/crypto are selected graph dependencies and are not all linked into
each product. Compare `go list -m -json all` before and after changes, and retain
the complete lists. The direct x/net/text requirements are exercised by the
HTTP/2, verified TLS and IDNA/Unicode regression tests. The x/sys upgrade follows
MVS; it does not establish Windows runtime verification.

The required CI `verify` job installs `golang.org/x/vuln/cmd/govulncheck@v1.9.0`
in an isolated tool cache, then invokes the same repository gate as local runs:

```bash
python3 -B scripts/run-security.py \
  --go /absolute/path/to/go1.27.3/bin/go \
  --scanner /absolute/path/to/govulncheck \
  --gomodcache /absolute/path/to/populated-module-cache \
  --gocache /absolute/path/to/build-cache \
  --output /absolute/path/to/new-security-evidence
```

Use explicit paths for available tools/caches; no user-specific temporary path
or previous result is built into the script. Module downloads are disabled in
the gate. Populate dependencies before invoking it. The gate constructs a new
allowlist subprocess environment, disables GOENV/GOWORK, fixes GOFLAGS and
GOTOOLCHAIN, drops proxy/TLS override and cloud variables, and rejects DB
override variables. It never reads an input stream or an offline snapshot.
Only official online `https://vuln.go.dev` is supported. Online failure exits
nonzero; previous clean streams are never a fallback. Offline snapshot approval
is intentionally out of scope.

The gate records actual Go and scanner binary hashes and scanner buildinfo,
source hashes before/after, unique invocation IDs/times, raw streams and their
hashes, package-load and scanner exit codes/stderr, validator results and binary
buildinfo/hashes. A fresh direct HTTPS index receipt records request/final URL,
HTTP status, receipt times and body hash. It is a separate index request, not
proof that every scanner response came from one identical or atomic snapshot.
Scanner query times are bounded by its actual process start/completion. Every
scanner command explicitly includes `-db=https://vuln.go.dev`.

Scope: source Collector, Controller and `./...`, each with linux amd64/arm64,
CGO0; four freshly compiled Collector/Controller binaries, one per architecture.
Each source target has separate `go list -json TARGET` discovery and
`go list -deps -e -json TARGET` package load. Named roots must match explicit
project paths; all-source roots come from discovery, never scanner SBOM.
Binary roots follow the pinned scanner's module-root output and are checked
against the explicit project module; buildinfo separately checks executable
package path, compiler, OS/architecture and CGO0. Builds use trimpath and
`-ldflags '-s -w -X main.version=security-local'`. ARM64 compilation and static
analysis are not ARM64 execution. Binary scanning is linked-symbol inspection,
not a source call graph or a dynamic reachability proof.

`scripts/verify-security.py` validates the complete stream, compiler/stdlib,
roots, package status/stderr and analysis stage. Pinned v1.9.0's
`internal/govulncheck/govulncheck.go` and observed official output specify
protocol `v1.0.0`, with exactly one of config, SBOM, progress, osv or finding per
event. Unknown/malformed events and duplicate JSON keys fail closed. DB must
be present and exactly official. Timestamps must be RFC3339 and no later than
scan completion plus **300 seconds** clock skew. DB modification time is not
query time: an old modification date alone is not stale evidence. Optional
progress/advisory times are validated when present. Standalone validation is
labelled stream revalidation, and does not establish a new online query or
provenance by itself.

The wrapper runs `test_*security*.py`, including real subprocess/validator
integration with synthetic scanner/package output. Exit 2 (TOOL_ERROR) and
3 (FINDINGS) both fail CI, including module/package findings. Diagnostics remain
available even on failure and the CI artifact step uses `always()`. The existing
`verify` job name, read-only permissions, image dependency, publish needs,
main-push condition and environment approval gate remain unchanged. Local gate
success is not a claim of remote CI success or remote protection settings.

Run the full Go unit suite, race suite, vet and build. For envtest, verify the
SHA-512 of the fixed controller-tools `envtest-v1.35.0-linux-amd64.tar.gz` against
its official release index and point `KUBEBUILDER_ASSETS` to its executables.
Set `OPSLOOP_REQUIRE_ENVTEST=1`; missing assets must fail rather than skip.
Keep per-test names, top-level versus subtest counts, and skip reasons; race
repeats do not represent additional distinct features.

Run Dashboard and Worker typechecks, tests, public/status boundary tests and
all production builds. Install Chromium using the locked Playwright 1.61.1
package's CLI into a separate `PLAYWRIGHT_BROWSERS_PATH`, then run the existing
`worker` browser script against its synthetic localhost Worker/D1 server. This
also aborts a real IndexedDB finalize transaction, checks visible failure and
pending-state retention, retries, and confirms one durable simulation record
after reload. Record the browser revision/version and any host OS fallback.

These checks cover local artifacts only. Independent review is pending;
deployment and Enforce remain blocked pending their separate approvals.
