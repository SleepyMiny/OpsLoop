Offline fixed-target helper packaging

Templates contain symbolic target literals, not private target values, past
approvals or live evidence. provenance.json records logical source identifiers,
original source SHA256 and current template SHA256. Audit and policyHash are
verbatim. Only target/path literals change in grant/quoteProbe. The Kubernetes
identity() body remains unchanged, with symbolic ORIGINS, CLIENT and CONFIG.

The caller must supply a private JSON build-target file outside this module.
It must be a regular file owned by the invoking UID, mode exactly 0600, without
symlinks in its path, at most 16384 bytes. Its exact schema is:
{
  "schema": "opsloop-host-helper-build-target/v1",
  "apiOrigin": "https://192.0.2.10:6443",
  "clockClusterUID": "11111111-2222-4333-8444-555555555555",
  "transportOrigins": ["https://192.0.2.10:6443", "https://198.51.100.20:6443"]
}
These are synthetic documentation values, not an operational target. All four
fields are required; unknown and duplicate fields are rejected. Origins must be
canonical HTTPS origins with a DNS name or IP address and optional port, without
credentials, paths, query strings, fragments, wildcards or control characters.
transportOrigins contains one or two unique exact origins, with apiOrigin first.
clockClusterUID must be a canonical lowercase nonzero UUID. No target is defaulted
or inferred from kubeconfig. The supplied SHA256 must match the exact file bytes.

Create .release-build in the release source, then run with explicit private paths:
python3 -m tools.host_release --output-root /ABS/RELEASE/source/.release-build/helpers --kubeconfig /etc/opsloop/private/kubeconfig --kubectl /opt/opsloop/rc/bin/kubectl --target-config /ABS/PRIVATE/build-target.json --target-config-sha256 EXACT_LOWERCASE_SHA256

The generator reads the private build-target file, but reads no kubeconfig/client
contents and calls no API. Paths must be absolute and normalized, without symlinks.
The existing namespace, fixture, TLS checks, methods, scope/time checks and helper
ABI remain fixed. Existing runtime/host-plan target checks remain independently
required; this config does not expand their allowlists or authorize any target.

Generated code and manifest contain private target values and local paths. Keep
both, the target config and build outputs out of Git and publication manifests.
Output directories use mode 0700 and generated files mode 0600. Generation refuses
an existing output directory; retries require a new private output location.
The generated manifest binds module source, generator, templates, configuration
paths, target-config SHA256, fixed target and output source hashes.

Build each Go package inside the current module, using pinned Go and dependencies,
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 (or arm64),
go build -trimpath -ldflags='-s -w' -o OUTPUT ./RELATIVE_OUTPUT/PACKAGE.
The build receipt must bind the manifest SHA256, Go binary SHA256, exact argv,
architecture, exit code and final executable SHA256. The generator does not build.

New generated helper/source and executable pins require a newly approved host
plan before installation or execution. The build-target file is build input only;
its ownership and hash checks issue no operational authorization. Do not use it
to change an approved target, bypass host-plan approval or reuse expired approval.
Keep credentials, issuer keys, approvals and ledgers outside this source package.
The caller must arrange private config and pinned kubectl separately.
