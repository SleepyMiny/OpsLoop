# Guard-aware run finalization (P2-F)

`demo-driver finalize-run -config /absolute/finalize-config.json` consumes one
explicit Observer run and one explicit Guard epoch. It reads files and writes
only a fresh result directory. It never opens a Guard action store, calls its
CLI, inspects a process, queries Kubernetes/libvirt, or executes the configured
VM executable. No new runtime execution authority is introduced.

Prepare the `opsloop-finalize/v1` configuration **before collection/Guard arm**.
Pin the expected full Guard configuration and its SHA-256 binding from the
approved run configuration, not from the recorded state being checked. The
binding is the digest of Go `json.Marshal(guardConfig)` (the established Guard
contract). The example is a synthetic, inert configuration; it does not grant
permission to execute anything. Generate a new approved configuration/binding
for a real run. Never choose a file by name sorting, mtime or latest-file search.

The configuration fixes runID, evidenceKind, expected epoch/config/URI/VM UUID,
Policy and Deployment names/UIDs, full Service identity, expected Runtime node
and version, platform digests, exact Observer directory, Guard file and new
output directory. `targetMapping` explicitly connects a Kubernetes Node
name/UID from this Incident's candidates with the Guard VM and Finish VM.
Nonempty trigger Node identity must also agree. The Node UID is not a VM UUID.
The mapping checks supplied identities; it does not verify real target
registration or prove a physical host/VM relationship. Synthetic tests declare
`test:///opsloop-p2d` ↔ `qemu:///system` in this map and use a test adapter to the
same in-memory VM. Product code never rewrites/aliases those URIs. Observed
configurations reject different Finish/Guard URI or UUID aliases. The Finish
receipt does not encode its URI; that part of the supplied map remains an
unverified assertion. No live Finish backend is added.

Inputs are bounded regular files with no symlink path components. The finalizer
reuses the strict typed JSON decoder, probe parser, evidence validator and pure
Guard invariant validation. A Finalizer-local counted reader leaves the shared
Guard/Observer reader unchanged. It reads manifest.json, COMPLETE and every
explicit manifest member (including receipts and original summary), unless the
snapshot budget has already failed. It verifies byte hashes/sizes, required
files, run/identity/source bindings, and completion. Unsupported schemas, partial
JSON, duplicate typed keys, invalid paths/types and hash mismatches are input
errors. Missing evidence is UNPROVEN. A missing Guard file cannot mask an
independent binding error.

Limits are 2 MiB each for config, Guard, manifest and COMPLETE; 16 MiB per Observer
member; 8192 manifest entries; and 64 MiB of actual input bytes per snapshot,
including all four metadata files. Before reading, the reader applies the smaller
of the per-file limit and remaining snapshot budget. An already oversized regular
file is rejected by descriptor stat without reading its payload. Declared manifest
size does not authorize extra reads. A file that grows during reading may consume
one extra byte to detect the limit: that byte is charged too. Thus one snapshot
can read at most 64 MiB + 1 byte, and no further read is issued after cumulative
excess is confirmed. Per-file growth probes, partial reads followed by error,
and bytes discarded after revision/hash/size validation all consume the same
snapshot budget, even when no data is retained.

A confirmed cumulative excess is sticky INPUT_BUDGET_EXCEEDED, with INPUT_ERROR
and nonzero exit. Unvisited members have no successful data/reference; later
members are marked budget-blocked, not missing or verified. It immediately stops
subsequent file reads and captures. No completed stability comparison is claimed,
and the reason is not replaced by INPUT_CHANGED_DURING_FINALIZATION. For other
inputs, at most three attempts each compare two complete snapshots using bytes
and file revision metadata. A settled revision may succeed; config bytes must
match the initial configuration. Each capture has a fresh 64 MiB budget (at most
six captures per call). Before these captures, CLI configuration discovery is one
separate bounded read of at most 2 MiB + 1 detection byte. This is not charged to
a snapshot. A changing/unreadable revision cannot produce PASS.

The five original probe/incident/pods/env/evidence inputs run through the
unchanged `Acceptance` runtime-v1 path. The detailed `AcceptanceResult` is
embedded verbatim and must equal the original Observer summary. A changed result
is `RUNTIME_REEVALUATION_DIFFERS_FROM_SUMMARY`, not an edited summary. The copied
Observer receipt keeps its original bytes/hash and run/kind binding; its normal
P2-D interpretation is already reflected in Observer evidence. Cleanup restore
VM identities must match the declared Finish UUID. The finalizer never fills
fault/source times, clock bounds, actor coverage, StopProof or CleanupProof.

Guard normal completion requires the expected supported config, original
invariants, armed event and contiguous actor/sequence history. Each read-only
command follows command-intent → vm-start-attempt → vm-started → command-result.
A durable prefix interrupted by a save failure can restart only at a new domuuid
command-intent. Each restart discards query progress, never stored counterevidence:
the final disarm must immediately follow a newly complete expected UUID + running
pair. Complete paused queries may precede a fresh running pair; an unfinished
prefix or final paused observation cannot prove normal completion. Stored
unconfirmed/wrong UUID, shut-off or unknown results remain invalid even if a later
query succeeds.
Emergency, resume intent, manual/effect events or impossible query order cannot
be hidden by `phase=DISARMED` or `emergency=false`. Historical expiry is not
compared with current time. The original clock/permit rules are evaluated at
the recorded terminal tick/At: hard eligibility stays monotonic, including
supported bounded wall skew across the wall deadline. Event At is retained as wall time, with sequence
providing record order; bounded wall regressions are not sorted/clamped and are
not turned into Runtime elapsed/segment/offset values. A historical DISARMED
record is not a current VM health check.

`run-result.json` has fixed schema `opsloop-run-result/v1`:

| Field | Meaning |
| --- | --- |
| runtimeAcceptance | Original computed runtime-v1 details, or null if unparseable |
| guardAssessment | status, phase, reasons, emergency, resumeIntent |
| status | PASS, FAIL, UNPROVEN, INPUT_ERROR |
| overallPass | Runtime PASS AND proven normal Guard close, with valid complete input |
| evidenceKind / liveProof | synthetic or observed; liveProof is always false |
| inputReferences | Explicit role/path, original byte SHA-256 and byte count |
| guardEpoch / guardBinding / identity / targetMapping | Expected integration bindings |
| consumerIntegration | GUARD_HISTORY_AND_RUNTIME_V1_OFFLINE |
| reasons / limitations | Reasons and limits of this consumer |

Guard assessment statuses are NORMAL_COMPLETION, EMERGENCY, INCOMPLETE, MANUAL,
MISSING and INVALID. ARMED is incomplete. RESTORING with emergency evidence is
EMERGENCY, with its incomplete phase retained. Emergency-confirmed running is
not normal cleanup, including when no resume occurred or Service was already
stable. ABORTED_MANUAL is MANUAL/UNPROVEN unless a separate emergency/Runtime
failure or input error is present. Input errors take status precedence while
independent valid Guard emergency details and Runtime details are preserved.
All non-PASS paths return nonzero and write a structured result when the trusted
config/output permit it. Guard's `automaticSuccessEligible=false` and
`consumerIntegration=UNINTEGRATED` remain untouched.

The output parent must already exist with mode 0700 and no symlink ancestors.
`outputDir` must not exist; it is exclusively reserved with mode 0700. Complete
bytes are synced to `.run-result.pending`, then linked exclusively to
`run-result.json` and the directory synced. An occupied output or a previous
partial attempt is refused. No PASS line is emitted on write/publication/sync
failure. A failed reservation is retained for diagnosis, never reused. Files
are not authenticated signatures; hashes prove byte identity only. The snapshot
cannot prevent later edits after its final read or certify crash/power-loss
behavior. Config errors before a trusted output is available cannot safely
publish a structured result.

The CLI identifies run-result as the whole-run result and the Runtime summary
as subordinate. Existing `summarize` remains a summary-only legacy/Runtime
viewer; it does not consume/aggregate run-result files. It must not be used to
count whole-run successes. Synthetic and observed samples remain distinct and
no combined live-recovery total is produced.

Local regression command (use the fixed project toolchain/cache and an explicit
clean allowlist environment):

```
go build -o /tmp/opsloop-p2f/demo-driver ./cmd/demo-driver
P2F_CLI=/tmp/opsloop-p2f/demo-driver P2E_CLI=/tmp/opsloop-p2f/demo-driver P2D_CLI=/tmp/opsloop-p2f/demo-driver P2C_CLI=/tmp/opsloop-p2f/demo-driver go test -count=1 -timeout=5m -v ./cmd/demo-driver -run '^TestFinalize'
```

`P2F_CLI` is mandatory, not a SKIP condition. Integration reuses the existing
Observer + Finish collection/receipt fixture, explicit synthetic external clock
and actor provider, real Guard transition/store code with test VM boundaries,
and the built production CLI. Normal evidence and terminal Guard files are not
hand-edited to obtain PASS. Manipulated negative fixtures are labeled separately.
Tests cover late emergency with/without resume, incomplete/manual Guard,
Runtime FAIL/UNPROVEN, missing/invalid bindings and raw files, changing inputs,
duplicate finalization, partial/failed output, and unfilled clock/coverage/cleanup
gaps. These local tests do not prove live Runtime, watchdog or libvirt execution.

The TestFinalize scope also includes real beforeSave interruption at each query
save boundary, store close/open/load and fresh normal disarm, plus production CLI
reentry and explicit counterevidence controls. Budget regressions use small
injected budgets and counted readers for exact boundaries, discarded reads,
growth/error cases and capture counts. A single 8 MiB file with hardlinks checks
the 12-member review shape. CLI budget failure is tested against newly collected,
complete Runtime PASS evidence and must preserve Runtime details and Guard bytes.
Optional P2F_FIX_REPRO exports only minimal synthetic Guard/result/hash artifacts.
