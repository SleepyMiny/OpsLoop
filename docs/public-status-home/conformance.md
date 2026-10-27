# Design → implementation traceability

Original design files remain in the unchanged private design archive. Status below is
local implementation evidence; no production Cloudflare/Kubernetes authorization is implied.

| Requirement / phase | Code | Verification |
|---|---|---|
| Phase1 closed public DTO, no raw BFF | shared/public-status.schema.json; internal/publicstatus/{contract,collect}.go; src/status/contract.ts | shared-schema equality, Go+TS strict parser tests; sentinel-bearing TLS K8s projection; Worker rejects extra fields |
| Named GET-only observation and fixed7 PromQL | collect.go; unchanged dashboardbff transports and candidates.QueryMetrics | TLS HTTP mock exact GET paths; POST blocked before network; real Prometheus GET fixture min3/NaN/duplicates/future |
| TLS/private credentials/durable sequence | sender.go; cmd/status-collector/main.go | private-file/symlink/mode/owner, concurrent counters, restart/no-reset, redirect refusal, CA denial and init-only tests |
| HMAC/time/body boundaries | Worker public-status.ts; sender.go | Go golden vector, Go signed envelope and full synthetic collection accepted by actual WebCrypto; wrong key/origin/replay/oversize/duplicate/UTF8/slow-body rejection |
| Atomic one-row D1 / response loss | migration0001; single UPDATE RETURNING | actual local persisted D1:8 concurrent requests1 winner, sequence ordering/key rotation, workerd restart, SQL trigger failure leaves body/counter unchanged, no auto row creation |
| Phase2 default real-observation Home | StatusApp.tsx/client.ts | no fabricated nodes before response; timeout/invalid/403/HTML/503/stale/future/cancel cases; browser503→visible error, fresh recovery |
| Opt-in finite Demo + local History | session.ts/history.ts | no Demo engine before selection; end/seek/StrictMode/idempotent retry; real Chromium IndexedDB persistence,50/TTL, blocked storage |
| Preserve both evidence records/private Live | existing App/createPublicDataSource; status Vite profile | full frontend suite; pure-static boundary tests; browser classifications/manual restore disclosures; original module/file hash comparison |
| Phase3 actual runtime same-origin routing/CSP | wrangler.status.json; local.mjs; status-static/_headers | workerd asset-first +api worker-first runtime; API JSON404/405, static CSP and real browser external-fetch violation |
| Output privacy | isolated build-status + audit-status | graph excludes private live/original evidence; output allowlist/hashes; synthetic raw UID/IP/token sentinel never projected; no VITE env inheritance |

JSON Schema validation implements the checked-in closed keyword subset, not a generic2020-12
engine. Unknown keywords/open objects/unbounded arrays are rejected. Go integer normalization
accepts equivalent safe JSON number forms after validation (1/1.0/1e0); no unsafe precision is
accepted. Production keys/credentials were not read for these tests.

Documented refinements: Up evaluation timestamp vs raw scrape timestamp; pending History marked
unfinished; local-supported Worker compatibility date. See README. These refinements preserve
fail-closed security and source separation rather than weakening tests.
