# Go ↔ Dashboard contract vectors

Two fixture versions serve separate checks:

- `src/domain/__fixtures__/go-{candidates,quantities}.json` retains the exact
  historical backend contract at `b4fb78d0707afdbc93867d087fea5d1511203208`
  (PR #2 Runtime Safety fix). These bytes are not replaced by current output.
- `src/domain/__fixtures__/current/go-{candidates,quantities}.json` is the contract
  exported by the checked-out `HEAD`, including every field serialized by the
  current Go `Input` and actual `Decision`. In PR CI this is the test merge commit.

`export_test.go.txt` runs **inside a scratch archive** alongside the backend's
existing fixture helpers. It calls real `candidates.Evaluate` and Kubernetes
`resource.Quantity.Value`; it does not strip current fields to imitate history.
The current inputs explicitly include `RunID`, `Epoch`, `PolicyUID`, and
`ResourceConditions`. These numerical vectors have empty bindings and no resource
conditions; TS tests assert those values rather than silently applying the ordinary
candidate adapter to a nonempty experimental input. Runtime resource-condition
and Controller-owned decision tests remain separate.

To check both versions without rewriting committed fixtures:

```bash
bash contracts/verify.sh /path/to/OpsLoop historical
bash contracts/verify.sh /path/to/OpsLoop current
npm test -- src/domain/go-contract.test.ts src/data/live/quantity.test.ts
```

To intentionally regenerate the current version after a backend contract change:

```bash
bash contracts/regenerate.sh /path/to/OpsLoop current
```

`regenerate.sh` defaults to `historical` for compatibility. Its optional third
argument selects a separate output directory. Both generation modes archive a
local Git object (`HEAD` or the fixed historical commit); neither reads uncommitted
backend changes. After a backend edit, generate from a prepared Git snapshot and
verify again against the committed candidate. The historical fixture is an
immutable compatibility reference; a new current fixture does not change it.

Go needs its normal toolchain/module cache (or network access to obtain modules).
Set `GOCACHE`, `GOMODCACHE`, and `TMPDIR` to isolated paths when desired.
Scripts do not fetch, checkout, merge, commit, or write to the backend repo.
`verify.sh` generates temporary files and byte-compares both JSON contracts.
It never updates fixtures to hide drift. Scratch files are removed on exit.

`go-candidates.json` stores raw Go Inputs and actual Decisions. The TS adapter
converts shape/units; it does not calculate expected eligibility or scores.
Assertions cover selected UID, reject reasons, eligibility, all three integer
headrooms, penalty, and total score. Vectors include the audit's rounding,
threshold and 111m reserve examples, ties, stale/missing/future metrics, cooldown,
negative score, count/up gates, raw memory range and epoch-time boundaries.

This covers the numerical candidate contract on valid P1 workload inputs. It
is not a reimplementation of Kubernetes scheduling, selectors, Pod request
aggregation, or duplicate Prometheus series handling: the BFF must supply those
validated node inputs. Persisted Decisions always remain Controller-owned.

`go-quantities.json` stores Go Value() as strings, preserving int64 precision.
The Dashboard intentionally rejects values above Number.MAX_SAFE_INTEGER,
negative memory, and digitless tokens (Go ParseQuantity accepts `.` as zero).
UI conversion rejects unsupported values with their field path instead of
silently approximating them. Existing fixed examples additionally cover
malformed input and very large exponents without allocating enormous integers.

The exporter is a `.go.txt` template deliberately excluded from the root Go
package scan. It references private helpers in the backend candidates package.
`regenerate.sh` installs it as `dashboard_contract_test.go` only in its scratch
archive. Integration regression: root `go vet ./...` and `go test ./...` must not
compile a standalone frontend/dashboard/contracts package.
