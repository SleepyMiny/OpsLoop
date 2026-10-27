# Finish adapter — synthetic and explicitly authorized lab-libvirt

`demo-driver finish-run` implements externally requested normal finish for an
explicit `single-vm-pause-resume` run. The default `synthetic-local` /
`test-executable` path retains literal loopback HTTPS API, test URI, and fixture
executable restrictions. The separate `lab-live` / `lab-libvirt` path below
connects the product HTTP client and existing Guard backend for a pinned local
qemu target. This delivery was verified with synthetic TLS APIs and fake
executables only. Real API/libvirt/VM execution is outside this approval.
No discovery, SSH, shell operation, fault injection, Enforce activation or
automatic failback is provided. Do not run `run.sh` for this adapter.

Use a separate executable invocation/configuration from `observe`. The Observer
transport remains GET-only. Separate processes and the execution confirmation
flag are not complete security isolation, authentication, or authorization. A
fixture executable/hash is a local test approval input, not an infrastructure
identity or a secure sandbox. No executable or target should be inferred from
a user's environment.

`finish-run.example.json` contains placeholders only. A local test harness must
supply its own namespace UID, kube-system namespace UID (the explicit cluster
identity pin), Policy UID/generation/spec hash, Deployment UID/spec/template
hashes, Service/Pod identities, VM UUID/URI, CA, synthetic bearer file, and
approved executable/hash. Nothing collects real identifiers. The per-invocation
deadline for external operations is bounded by `deadlineMs`; every reentry uses the same configuration
binding and never authorizes replay of an ambiguous mutation.

```text
demo-driver finish-run -config /tmp/approved-fixture/finish.json
demo-driver finish-run -config /tmp/approved-fixture/finish.json -execute -confirm <binding printed by plan>
```

The default prints a plan without reading credentials, creating output, calling
HTTP, or launching a VM command. Execution requires the exact canonical config
SHA-256. This is an accidental-target guard, not a permission system. Execution must use approved targets and dedicated credentials, under an environment allowlist that excludes
real credentials, proxy settings, kubeconfig and LIBVIRT_DEFAULT_URI. The VM
child receives only LANG/LC_ALL and uses no shell or inherited stdin/environment.
The API client has its own TLS transport, no proxy/redirect/discovery/fallback;
the Service client has a separate authority and no API bearer.

## Stop and restore

Fresh namespace/cluster/Policy/Deployment checks precede a durable stop intent.
The only API write is JSON Patch with tests for UID, resourceVersion and the
complete observed spec, followed by `replace /spec/enabled false`. It does not
rewrite other fields. A 409 permits at most three total attempts, each preceded
by full identity/spec/generation revalidation. An external spec change blocks
execution. A non-409 error or missing response never causes automatic replay:
fresh GETs reconcile its effect. A persisted dispatch without a sent request may
therefore require manual intervention even if no effect occurred.

Already disabled without this run's ledger is `EXTERNAL_STOP_NO_REQUEST`. No
past request timestamp is manufactured. Stop confirmation requires matching
Policy/Deployment UIDs, current disabled state and the expected stop generation,
non-deletion, Quiesced=True/FenceClosed, current condition observedGeneration,
no active incident, matching v2 ack/live fence owner/UID/floor, and unchanged
approved Deployment desired state. Sequential GETs are not atomic snapshots.
Two fresh pairs are required immediately before restoration and after it;
reactivation, owner change, deletion or other contradictions block success.
Stop confirmation does not cancel a rollout, terminate a process or prove
Service health. An enabled ObserveOnly state does not satisfy this adapter's
own explicit disabled request, even though the general stop contract also
supports ObserveOnly.

The only VM verbs are fixed argv:

```text
<approved-test-executable> --connect test:///opsloop-p2d domuuid <uuid>
<approved-test-executable> --connect test:///opsloop-p2d domstate <uuid>
<approved-test-executable> --connect test:///opsloop-p2d resume <uuid>
```

UUID and state are queried before and after resume. In the synthetic path,
only paused to running is the normal path. Running without own resume ledger, shutoff, replacement,
connection/query failure or an unconfirmed effect requires manual attention;
there is no start/destroy/recreate/suspend path. A timeout or lost response is
reconciled by querying; an ambiguous dispatch is never blindly repeated.
Exit zero alone is not confirmation. A nonzero/lost response with independently
confirmed matching running state can be reconciled; otherwise cleanup remains
pending. Once the overall context expires, there is no new unbounded context
for reconciliation: the invocation exits nonzero and the next explicit reentry
queries current UUID/state. No resume is automatically resent, including after
preflight rejection or a crash with only a write-ahead dispatch marker.
There is no automatic next experiment.

The runner distinguishes `vm-call`/`resume-dispatch` intent, `vm-start-attempt`
(after preflight, before Start), `vm-started` (successful process Start), the
returned `vm-result`, and fresh `resume-confirmed` UUID/state evidence. The
started event references the exact call, resume dispatch, UUID, and action/run.
Its BeforeStart stamp is a lower boundary that includes the attempt-record sync;
AfterStart is captured immediately after Start succeeds, before recording it or
waiting. Requested uses **AfterStart**, a local observation of successful Start,
not preflight time, return time, or proof of the external VM transition time.
Hash mismatch, missing/invalid executable, cancellation before Start, and Start
failure record an unstarted result/reason and produce no restore Requested.
A started nonzero/timeout preserves the start fact; only independent matching
UUID/running observations supply confirmation. A crash after Start but before
the durable started event leaves the start unproved. It never authorizes replay
or invention of a past Requested timestamp. Legacy vm-result.Requested fields
are not promoted to started evidence. A legacy/ambiguous ledger can establish
current state through fresh queries without proving the missing request time.

On Linux each approved fixture gets its own process group. The executor enables
child-subreaper adoption, calls Wait for every successful Start, terminates only
that group on cancellation and after Wait (including an exited parent leaving
pipes open), and reaps adopted children only in that group. `WaitDelay=100ms`
limits inherited stdout/stderr drain separately from the 4096-byte stdout cap;
a further 100ms bounds group-reap polling. Expired/canceled context or failed
I/O/group cleanup cannot turn valid-looking command output into success. The
overall external-operation deadline plus at most these 200ms of process cleanup
and local evidence persistence is the termination contract under normal OS and
filesystem progress; journal flock is released on return. No new mutation is
started after the overall deadline. WaitDelay itself does not terminate all
descendants. Detached processes/changed groups, uninterruptible OS stalls,
scheduling starvation, and hung disk/fsync are outside this bound. This is a
trusted local fixture adapter, not a general process sandbox; other platforms
fail preflight rather than claiming Linux ownership guarantees.

## Cleanup and evidence

Only this approved single-target scenario with explicit `additionalChanges: []`
is supported. Cleanup requires stop confirmation, confirmed fault removal,
unchanged Deployment desired state, a fresh expected Pod/Service identity check,
an explicit old-Pod inventory check, a matching Service response, and another
stop recheck. `oldPods: []` is an explicitly supplied empty inventory; omitted or
null inventories are rejected. A present old Pod is recorded by UID; an absent
API object is recorded as API absence only. Neither proves process termination
or physical fencing. A single postcheck is not a new Service stability claim.
No workload is moved back to its original Node and no Policy is re-enabled.
Unsupported additional load/network/resource changes cannot be silently cleaned.

The private `journalDir` uses immutable O_EXCL events, file/directory fsync,
a whole-config action binding, and a local flock. It assumes a filesystem with
working fsync/flock semantics and exclusive custody of the evidence directory.
The lock does not cover external writers. Torn/corrupt events, foreign receipts,
changed config/identity or concurrent invocations fail closed. Valid receipt
temporary files left by a crash are retained; they never supply action state.
Log failure blocks further mutations. If an effect precedes a recording failure,
the CLI exits nonzero and the durable dispatch remains uncertain. It does not
claim exactly-once execution or recovery from arbitrary filesystem corruption.

`receiptFile` must be `journalDir/receipts.json`. Publication syncs bytes, records
receipt readiness, atomically renames and syncs the directory, then records the
receipt hash. Receipt hash disagreement after an interrupted replacement is
manual-review territory. A successful receipt keeps its original timestamps on
redelivery while fresh queries check current validity. Subsequent contradictions
produce an incomplete receipt instead of a new success claim. If a publication
already exists in the journal, missing/nonregular/symlink/foreign/hash-mismatched
current receipt fails closed with nonzero/manual review. A deleted published
file is not treated as first publication and is not automatically regenerated.
Normal redelivery preserves original bytes, hash, and completion time.

Before replacing a previously published receipt, the product preserves its
actual bytes as `archive-<published-SHA256>.json` in the private journal. Creation
is exclusive; file and directory are synced. An existing archive is accepted
only if regular, same byte/hash/run/evidenceKind identity, and backed by a published
hash in this action journal; arbitrary/foreign/tampered archives fail closed.
No different bytes are overwritten. Archive failure prevents current receipt
replacement and exits nonzero even if an older succeeded file remains visible.
A crash before/after archive, ready, rename, directory sync, or publication
record either continues a verifiable state or fails closed (in particular when
renamed bytes disagree with the last durable publication). There is no general
automatic repair engine. Historical events/completion stamps remain immutable.

The unchanged ObserverReceipts/v1 schema carries only observed stop-request
attempts, actual cleanup-actor resume attempts/confirmations, and postcheck
completion. Intent/dispatch and response/GET/result events remain distinct in
the adjacent journal. If a crash leaves only intent/dispatch, that fact cannot
manufacture an actual request receipt. There are no fabricated fault/Controller/
Ready times, clock bounds or complete actor coverage. Standalone executor
receipts consequently cannot establish pure Controller recovery PASS. The
integration test uses a separately labelled synthetic actor/clock provider to
compose source receipts with the unchanged executor facts; it never edits
completed runtime evidence or reports.

Start the Observer before requesting finish. Deliver receipts before its final
observations/finalization. A stop observation must lie between the request and
cleanup, and cleanup must precede the captured collection horizon. Do not copy
collector elapsed/segment values, backdate cleanup, or join unrelated segments.
Receipt publication does not finalize or stop the Observer. Its existing
Acceptance logic owns the final result, including exclusion of early assisted
recovery. This adapter never waits for final PASS before cleaning up.

Receipt bytes describe the publication's observation horizon, not continuous
validity. An Observer that already consumed an older success does not receive a
revocation by reading an archive or by CLI failure alone. On archive/publication
failure an old success file can remain; it must not be presented as fresh
verification. Delivery/custody must carry the current CLI verdict and adjacent
journal, and a subsequent explicit observation run is needed to assess later
contradictions. The Observer's existing GET-only transport, receipt schema and
Acceptance/report semantics are unchanged.

## Local checks and deferred prerequisites

Tests use real httptest HTTP/TLS, a synthetic Controller ack double, injected
VM executors and a Python test executable. A separate built CLI and test child
process exercise real argv and abrupt process exit boundaries. No test connects
to a real Scheduler, Controller, cluster or VM. Build the CLI first and set
P2D_CLI and P2C_CLI to that temporary binary when running the package tests.
No new dependency is required. See the delivery ZIP verification for exact
fixed-toolchain commands and results.

## Explicit lab mode and Guard coordination

`finish-run.lab.example.json` and `finish-run.lab-authorization.example.json`
are **inactive placeholders**. They contain no registered target, current
approval, credential or working path. Do not derive expected UIDs, generation,
hashes or VM UUID from replies being checked. VM UUID and Kubernetes Node UID
are separate identities. `evidenceKind` must explicitly be `observed` or
`synthetic`; tests of this product path use `synthetic`.

Supply all existing API/Service/Policy/Deployment/Pod pins plus `lab-live`,
`lab-libvirt`, an exact local `qemu:///system` or `qemu:///session` URI, UUID,
absolute pinned executable named `virsh`, and the existing Guard config file,
canonical binding, root and epoch. API origins may be explicit non-loopback
HTTPS origins only in this mode. Origin paths, userinfo, query, redirects,
proxies, ambient kubeconfig and discovery remain forbidden. API CA and bearer
are explicit files. The independent Service client carries **no API bearer**;
this adapter supports an unauthenticated approved Service lane, not shared API
credentials or ambient client authentication. The only API write remains the
Policy-specific conditional enabled=false patch described above.

Plan reads only its explicit Finish config. It does not read credentials,
Guard config/state or approval files, preflight executables, open a Guard,
create journals, issue HTTP or launch commands. It prints the canonical Finish
binding and unmet gates. Execution requires `-execute -confirm <exact binding>`
and both files: the existing `opsloop-guard-authorization/v1` approval and a
separate `opsloop-finish-authorization/v1` approval bound to the whole Finish
config, with operator/reference, explicit
`normal-finish-policy-stop-query-resume` scope, notBefore and expiry. Normal
approval may not outlive Guard authority. Approval references and config hashes
are local attestations; they do not authenticate operator identity, resource
registration, VM-to-Node mapping, OS permissions or Kubernetes RBAC. Verify
those under a separate live approval before any real execution.

Normal Finish reads the exact existing Guard state under **the existing target
lock**. Long Kubernetes stop polling holds no target lock. After stop proof,
the bounded target operation uses the Guard backend and rechecks fresh durable
Guard state, the original input-file hashes, normal and Guard approvals before
each command and immediately before Start. For resume, fresh stop proof and paused UUID/state are also
checked at the Start boundary. Foreign root/run/epoch/target/config, unknown or
dead owner, stale heartbeat, hard deadline, expired/uncertain authority,
RESTORING, emergency, terminal state and Guard resume intent reject normal
restoration. Finish never changes the Guard owner PID, heartbeat, deadline,
phase or emergency state, and never disarms it. Finish uses the same target flock
through a small context-aware acquisition helper; Guard CLI acquisition is
unchanged. Finish checks cancellation before waiting and just after acquiring,
and waits on cancellation between retries. Acquisition retains the existing
`6 * guard.timeoutMs + 2s` cap and also respects the earlier parent/operation
deadline. Only its own acquired descriptor is closed; no force unlock or file
deletion occurs. The target operation has a separate
`6 * guard.timeoutMs + 1s` context budget created **before lock acquisition**;
each command
uses the existing bounded runner, with timeout no greater than the Guard's.
Short fresh API checks are inside that budget; stop waiting is outside the lock.

Only `domuuid`, `domstate`, `resume` for that exact UUID/URI are allowed. The
lab path supports fresh running as a no-op, with no invented resume intervention.
Paused is resumed at most once per normal attempt; shutoff, UUID replacement
or unknown state fails closed. It does not start, destroy, recreate or suspend.

Before dispatch, Finish durably writes a versioned `.json.finish-resume` attempt
beside the existing epoch state under the same target lock. It is distinct from
Guard's own `ResumeIntent` and does not change Guard state-machine facts. It
binds Finish/Guard config, run/epoch/URI/UUID. Guard's only new shared behavior is
to inspect this marker before its own resume: if still paused after a normal
attempt (including a crash before Start), it retains that ambiguity as
ABORTED_MANUAL rather than issuing a duplicate resume. If already running,
Guard may preserve its emergency classification and confirm current running
without another resume. A malformed marker also prevents a fresh resume.
This is deliberately conservative; a dispatch-only interruption can require
manual review even if no command started. Finish and Guard do not silently
replay it. Start facts and fresh state remain separate in the Finish journal.
No new resume runner or alternate target lock is introduced.

After releasing the target operation lock, Finish performs existing postchecks
and stop rechecks, then another short fresh Guard check before cleanup success.
That final check uses the Finish context and the same bounded acquisition, and
checks cancellation again after fresh Guard validation. Immediately before a
new cleanup-confirmed event is accepted, the journal checks the context again.
An expired/canceled decision returns nonzero with pending receipt/companion,
preserving any resume Start/effect and earlier immutable history. Reentry may
archive an earlier succeeded receipt; it never rewrites its original event.

The accepted event time is the success decision boundary, before its write and
fsync. Cancellation **after** that decision does not erase a true fact: delayed
storage/publication may complete afterward. Tests distinguish cancellation
before the decision from delayed writing and publication after a timely
decision. Failure recording, receipt/companion persistence and descriptor/process
cleanup are observed separately from the execution deadline. OS fsync does not
provide an absolute hard time bound. A lock waiter is never abandoned in a
background goroutine.
Reentry after a Guard emergency cannot retain a normal current success; existing
receipt archival rules apply. These are bounded observations, not an atomic
snapshot across Guard, Kubernetes, service and external administrators.
Host failure, delayed/repeated fault injection and arbitrary external managers
are outside any exactly-once claim. Guard normal disarm has separate eligibility
and remains an explicit independent operation, never automated by Finish.

## Binding companion and delivery limits

The original receipt schema and request/completion timestamps are preserved.
Lab `evidenceKind` is carried through journal receipt/archive validation; omitted
synthetic evidenceKind retains its previous meaning and canonical binding.
`live-inputs` records exact raw config SHA-256 (CLI input), raw Guard config and
approval file hashes, and the approval reference. An immutable
`<journalDir>.binding-<receiptSHA256>-<inputSHA256>.json` companion links those
inputs with URI, UUID, run, epoch, root, Guard binding, canonical Finish binding,
receipt bytes and current status. It is written with exclusive creation and
file/directory sync; content-address collisions fail closed. Reentry preserves
original receipt bytes and completion time, and verifies input hashes. A
partial companion may require manual custody review; it is never overwritten.
Companion publication failure exits nonzero; an older/just-published success
receipt alone is insufficient evidence for a current successful lab invocation.

Companions declare `liveProof=false` and `FINALIZER_UNINTEGRATED`. Existing
Observer/Finalizer consumers do not read this new companion; the Finalizer was
not extended and cannot authenticate its URI/registration claim. Deliver the
receipt, matching companion, adjacent journal and current CLI verdict together.
A consumer that ignores the companion or already consumed a prior success does
not gain revocation/custody verification from this feature. No external fault
clock, source time or complete actor coverage is produced. Early normal or
emergency restore must remain assistance/intervention under existing Acceptance
rules, never pure automatic recovery. A matching Service postcheck is not a
service stability horizon or Runtime recovery demonstration.

Local validation builds a fresh CLI and exercises this actual product path with
synthetic TLS API/Controller acknowledgements and fake argv-compatible commands,
including command Start, response loss and reentry. Set `P3B_CLI`, `P2D_CLI`,
`P2E_CLI` to the fresh CLI for the targeted Finish/Recovery tests; the new command
boundary test fails if P3B_CLI is missing instead of skipping. Use a sanitized
process environment and disk-backed bounded caches. No actual virsh (including
--version), live credentials, libvirt connection, VM operation, fault, service
installation, deployment or remote CI is part of these checks.

INDEPENDENT_REVIEW_PENDING
LIVE_LAB_NOT_CREATED
LIVE_FINISH_NOT_RUN
WATCHDOG_LIVE_NOT_PROVEN
LIVE_RUNTIME_NOT_PROVEN
REMOTE_CI_NOT_RUN
DEPLOYMENT_NOT_APPROVED
ENFORCE_BLOCKED
