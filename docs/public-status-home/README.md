# Public Status Home — local implementation

Baseline: `1c2740b9e337bf85c32dba849be36291d2ad3e39`. This is an additive profile.
The original design record remains in the owner’s private review archive; the PR includes
this implementation contract and review summary. No remote deployment is authorized.

## Boundaries

- Collector: `cmd/status-collector`, `internal/publicstatus`. No listener. Explicit private config,
  verified Kubernetes TLS CA, dedicated token file. Reuses `dashboardbff.GetOnly`,
  `PrometheusQueryOnly` and the existing seven `candidates.QueryMetrics` expressions.
- Warning-bearing Prometheus query results are unavailable/INVALID; private warnings are never exposed.
- A new narrow reader performs three named Node GETs, three named Node Lease GETs,
  two named Deployment GETs, one Policy GET and namespace-scoped Incident LIST.
  It does not call BFF Assemble or read Pods, Secrets, logs or exec. The dashboard receives
  only the explicit `shared/public-status.schema.json` projection. Internal IPs used for
  exporter matching never leave the projection boundary.
- Worker: `worker/public-status.ts`. Public GET `/api/status`; HMAC-authenticated POST
  `/api/collect/status`. No Kubernetes credentials, proxy, injection or write API. D1 stores
  one latest snapshot with the sequence fence in the same conditional UPDATE.
- Browser: new `build:status` profile, default Home. `?source=demo` starts only after a choice;
  `?source=history` is browser-local IndexedDB; `?source=replay` preserves the two existing
  sanitized historical experiments. `source=live` and invalid modes are rejected explicitly.
  Existing private `npm run build` and pure-static `build:public` remain separate.

## Local reproduction (no Cloudflare account)

Requires Node 24, npm, Go matching go.mod. Browser tests use the pinned Playwright Chromium
revision, or an explicit CHROME_PATH for a locally installed Chrome.
Run `npm ci` separately in `frontend/dashboard` and `worker` using their checked-in lockfiles.
Do not supply real credentials or use production D1 bindings in local tests.

```bash
# Repository root
npm run build:status --prefix frontend/dashboard
npm run typecheck:status-worker --prefix frontend/dashboard
npm run test:status-build --prefix frontend/dashboard
npm run build --prefix worker
# Working directory is important for the following commands:
(cd worker && npm test)
(cd worker && node node_modules/playwright-core/cli.js install chromium && npm run test:browser)
go test ./internal/publicstatus ./cmd/status-collector
go test -race ./internal/publicstatus ./cmd/status-collector
go vet ./internal/publicstatus ./cmd/status-collector
```

`worker/test/runtime.test.mjs` uses actual local workerd and persisted local D1 via Miniflare.
It also executes the real Go Collector against a synthetic TLS Kubernetes server and seven
synthetic Prometheus query responses, then submits its HMAC envelope to Worker/D1.
The optional `OPSLOOP_TEST_*` paths used by that test are only local synthetic-test files.
No kubeconfig, current cluster, SSH, Cloudflare login or Azure account is used.

```bash
# Terminal foreground; bind only 127.0.0.1:8787. Ctrl+C disposes this local runtime.
cd worker
npm run dev -- --synthetic
# Open http://127.0.0.1:8787/ (synthetic fixtures, NOT actual cluster observations).
# Without --synthetic, an uninitialized observation is correctly shown unavailable.
```

`local.mjs` is a local test host, not a remotely deployed Worker module. Its direct DB
fixture helpers are test-process methods, never network endpoints. The application itself
has no anonymous write endpoint. Synthetic signing keys in tests must never be deployed.

## Collector configuration and state

`collector-config.example.json` contains placeholders only. Copy outside Git into a private
account directory, substitute approved dedicated read-only identity values and use files600,
parent directories700. No administrator kubeconfig, client key, exec plugin, token in argv/env,
or credential-bearing URL is accepted. API origin must use TLS and a pinned CA.
`collector-rbac.yaml.example` is a review template, not an apply instruction.

Use a precreated private700 state directory. Initialize once, independently of collection:

```bash
status-collector --config /private/config.json --init-sequence --init-only
status-collector --config /private/config.json --once
# Or omit --once for 30s cycles with bounded failure backoff and jitter.
```

An existing counter refuses reinitialization; missing state requires explicit initialization.
Counter reservation is fsync + atomic rename + directory fsync, under process flock and a
thread mutex, before sending. Gaps after failures are safe; sequence reset is not.
Do not delete/reset the state directory to fix a409. Do not restore D1 to an older sequence
while ingestion remains open. Restore/key/identity re-sync needs separate approval.
HMAC keys are printable32–128byte server-side files without newline; current/next versions
may overlap (max2), sharing the same counter. Do not use the public synthetic test key.
Short-lived Kubernetes token issuance/rotation is an owner operation, not a Collector privilege.
Replace credential files atomically before expiry; actual token reload/rotation must be tested
before service installation. No service was installed in this task.

## Semantics and design refinements

- CPU and memory use original `timestamp(...)` values from the fixed queries, not collection time.
  `up` instant-vector sample.Time is the **PromQL evaluation timestamp**, not an independently
  measured scrape timestamp. The UI labels it “평가”. This corrects the design's overly strong
  wording about a raw Up scrape timestamp; no eighth query was silently introduced. Healthy
  requires separately fresh CPU, memory and Node Lease observations. `MemTotal` has no
  independent raw timestamp among the seven queries; that existing-source limitation remains.
- Source observation/Deployment read time, Node Lease time, raw CPU/memory sample time,
  collectedAt, sentAt, receivedAt and browser freshness time remain distinct.
- Old stored data become stale after75s; individual samples/Lease after90s; future tolerance5s.
  An outage heartbeat uses null+fixed error enum. No fake zeros or Demo fallback.
  Historical Incident object count is separate from the current active reference count.
- Home is read-only reporting, not proof of Enforce, automatic rescheduling or recovery.
  It reports unexpected Enforce as degraded instead of rewriting it to ObserveOnly.
- An unfinished browser session can still be active in another tab. Pending records are labeled
  `unfinished`, not falsely declared completed/interrupted by a second tab. pagehide persistence
  is best-effort; process termination cannot guarantee an exact end time. History is50/30days.
- Worker profile uses compatibility date2026-08-06, the newest supported by the installed
  local workerd; attempting2026-10-20 failed explicitly. Existing pure-static config is unchanged.
  This profile date must be confirmed before Edge deployment; security checks were not relaxed.

## Evidence and approvals

See `conformance.md`, `approval-checklist.md` and `final-review.md`. Local test logs are written
to ignored `reports/public-status-home/` and preserved privately.
The reports are local/synthetic evidence, not proof of production access or current cluster health.
The original public deployment and existing Demo/Replay remain untouched until explicit approval.

Collector `prometheusURL` must point directly to the final Prometheus API base
address. The Collector rejects every HTTP redirect, including same-origin,
relative and HTTPS-to-HTTP locations, before sending a follow-up request. Use
the final API address in configuration instead of a redirecting proxy URL.
A rejected observation becomes unavailable/unknown; it does not refresh past
metrics or expose the rejected URL, query or internal diagnostic in the DTO.
