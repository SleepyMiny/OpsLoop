# Go ↔ Dashboard contract vectors

Backend reference: `b4fb78d0707afdbc93867d087fea5d1511203208` (PR #2 Runtime Safety fix).
Dashboard base: `ccbe94667fa7921b252da422ea2dc6d90e604eec` (PR #1).
No backend files are merged into this branch. `export_test.go.txt` runs **inside a
scratch archive** of the pinned backend alongside its existing fixture helpers.
It calls the real `candidates.Evaluate` and Kubernetes `resource.Quantity.Value`.

To regenerate, provide a local repository containing that backend commit:

```bash
bash contracts/regenerate.sh /path/to/OpsLoop-backend
npm test -- src/domain/go-contract.test.ts src/data/live/quantity.test.ts
```

Go needs its normal toolchain/module cache (or network access to obtain modules).
Set `GOCACHE`, `GOMODCACHE`, and `TMPDIR` to isolated paths when desired.
The script does not fetch, checkout, merge, commit, or write to the backend repo.
Only the two frontend JSON fixtures are overwritten; scratch files are removed.

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
