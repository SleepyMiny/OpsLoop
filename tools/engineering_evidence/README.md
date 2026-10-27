# Demo B evidence handoff

This package performs **offline validation of observations collected by a trusted
coordinator**. It does not authenticate an arbitrary JSON document as a Kubernetes
observation. The coordinator must approve and pin the capture, authorization and
build-provenance SHA-256 values outside the model's writable space.

Run `python3 -m tools.engineering_evidence --help` for the sealing command. It reads
the supplied files and Git objects only; it does not call Kubernetes, a model or
GitHub, and does not stage or commit files. Failure exits 2 with
`BLOCKED_EVIDENCE`. Success writes `bundle.json` and `manifest.json` into a newly
created directory and returns both SHA-256 values. Existing output is never
overwritten. Read-only modes prevent accidental edits; external hash pins detect
owner tampering and remain mandatory.

The capture directory must contain `capture-manifest.json` and these raw files:

- `incident.json`, `policy.json`: actual terminal Incident and its still-enabled
  Policy snapshot taken immediately before the strong stop.
- `deployment-before.json`, `deployment-after.json`: the same Deployment UID,
  before and after the actual Runtime change.
- `pod-before.json`, `pod-after.json`, `replicasets.json`: old/new Pod identities,
  running image IDs and controller owner chains.
- `events.json`: bounded Kubernetes Event list.
- `observations.json`: exactly two direct Pod-proxy `/quote` POST observations,
  one before Runtime patch and one after the failed attempt completes. Each
  includes Pod identity, response hash, fixture hash, status and known error code.
- `logs.json`: bounded original logs bound to old/new Pod UIDs. Only the fixed
  diagnostic code and raw log hash enter the model bundle; arbitrary log text and
  Event messages do not.

`capture-manifest.json` uses schema `opsloop-engineering-capture/v1`,
`evidence_kind: observed`, `captured_at`, `run_id`, `epoch`, `cluster_uid`,
`api_origin` and `files`. Each file entry has `sha256`, `observed_at` and
`evidence_kind: observed`. The external capture hash is the trust anchor, not
these self-described labels. Synthetic fixtures must never be pinned as live
evidence. Capture must be no older than ten minutes at sealing.

The separately pinned authorization uses schema
`opsloop-engineering-authorization/v1`, `engineering_enabled: true`,
`profile_id: demo-b-discount-v1`, `issued_at`, `expires_at`,
`max_runtime_attempts: 1`, the exact two `allowed_paths`, the capture identity
fields, `namespace`, `incident_uid`, `policy_name`, `policy_uid`,
`deployment_name`, `deployment_uid`, `repository`, `base_commit`, and
`image_digest`. Runtime grant run/epoch/cluster/Policy/Deployment/Incident identity
and action time must agree. Engineering authorization has its own expiry;
stopping Runtime after capture does not grant or revoke Engineering authority.

The separately pinned provenance uses schema
`opsloop-image-source-provenance/v1`, `evidence_kind: observed-build`,
`repository`, `base_commit`, `image_digest`, `platform_image_digests`, and
`source_files` (relative path to SHA-256). The entire committed
`apps/engineering-demo/` directory must be covered. The commit must exist as a
real Git commit; every file is compared directly to its Git blob. This check
does not replace a trusted build receipt linking those inputs to the image.

The exact structured input shape is exercised in
`tests/test_engineering_evidence.py`. Those fixtures and all passing tests are
**synthetic contract regression evidence**, not a Demo B live result. Real
cluster collection, real image build, fixed baseline commit and authorized
Engineering execution must be established independently.
