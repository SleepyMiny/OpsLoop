# Demo B: an observed application defect to a reviewed pull request

This milestone reuses the existing Hybrid cluster, Runtime Controller, readiness
classification, single-action budget, execution grant and strong-stop Guard.
It does not replace the recovered Demo A application. The dedicated workload is
`opsloop-demo-b/engineering-demo`.

## Preserved baseline

The original working tree's 379 source files and physical/logical Git indexes
matched `OpsLoop_DemoA_HybridE2E_20261027_030110_KST.zip`, SHA-256
`c396116dd0670a82c78c2ac9574c0e7a66521739e9f9862b02abd549c0a04ce0`.
The original 355-file comparison has the same 14 intended modifications.
Demo A remains **functional E2E PASS**, with global actor/clock validation and
physical power-loss testing **UNPROVEN**.

User-approved commits were created in an isolated local staging repository:

- Demo A sources: `c1aee110f4e5c953b86b0595e62be321cfe435b1`.
- Faulty application baseline: `171d8c10ed522848d7ccd4dd33b0b4b86d5341fc`.

The original worktree, index, branch and `~/Projects/OpsLoop` remain separate.
The baseline includes the verifier available when the commit was approved.
The execution uses a separately frozen and hashed current verifier, including
subsequent independent security-review fixes; its hash is a mandatory Runner
input. A source commit alone never authorizes publication or deployment.

## Actual defect and runtime boundary

`POST /quote` with exactly `{"items":[]}` reaches the order-pricing function and
causes integer division by zero. The unmodified application returns HTTP 500
with `QUOTE_DIVISION_BY_ZERO`. The expected result is HTTP 200 with zero total
and average. Existing nonempty-order behavior must remain unchanged.

The opt-in `DEMO_B_REPLAY=empty-order-after-30s` executes the same calculation
after a Ready bootstrap period. It accepts no arbitrary command or input URL.
The Runtime verification stability window is 45 seconds, longer than the
bootstrap grace, and the action budget is one. A replacement with the same
defective image must fail verification rather than count as recovery.

Engineering requires an actual `Patched -> Failed` attempt and terminal
`FAILED/RUNTIME_BUDGET_EXHAUSTED`, plus independent reproductions from the
initial and replacement Pod. Reserved/cancelled attempts, candidate absence,
deadline alone, ObserveOnly and Demo A's `RECOVERED` cannot qualify.

## Evidence, repair and verification

`tools.engineering_evidence` validates the separately pinned authenticated
capture, operator authorization and image/source build receipt. Raw objects
remain private. The AI bundle contains allowlisted identities, error codes,
fixture hashes and evidence hashes; arbitrary log/Event text and credentials
are omitted. File hashes and the externally retained manifest hash protect the
sealed input. Details and the capture contract are in
[`tools/engineering_evidence/README.md`](../../tools/engineering_evidence/README.md).

`tools.engineering_runner` exports only the three application Go files from the
exact commit into a separate run directory. A durable SQLite ledger prevents
duplicate failures and refuses to reassign an interrupted job automatically.
Each stage uses bwrap namespaces, read-only mounts and independently confirmed
systemd cgroup CPU, memory and process limits. Tests have no external network,
host home, kubeconfig, Azure credential, SSH key, Docker socket or model broker.

The real Codex CLI generates structured replacement source and appended tests
without tool calls. A host broker permits one request to the configured OpenAI
endpoint/model and keeps the provider credential outside the sandbox. The
coordinator applies only `discount.go` and `discount_test.go`, after checking
the base hash, file set and preservation of existing tests. The unmodified
model response, proposal hash and exact applied patch are retained. This is
recorded as `DIRECT_MODEL_CODE_PROPOSAL_APPLIED_BY_COORDINATOR`; it is not a
claim that Codex ran local edit commands. See the official
[non-interactive Codex documentation](https://learn.chatgpt.com/docs/non-interactive-mode).

The ChatGPT authentication route limits request count, input/output bytes and
wall time; it does not claim a hard billed-token cap. The current profile allows
one candidate and a total 900-second execution deadline, with a 300-second
model stage. No provider credentials or PR permissions enter application tests.

The current experiment is pinned to `gpt-6-luna`, `medium` reasoning and the
existing ChatGPT authentication route. The CLI receives `--model gpt-6-luna`
and `-c model_reasoning_effort="medium"`; the broker checks the outgoing effort
before forwarding. Other models, reasoning levels and API authentication are
rejected. One candidate/request is allowed, without retries or model fallback.
The binary's bundled catalog and recent account catalog advertise Luna Medium;
the approved real run subsequently completed one provider request successfully.
The reasoning override follows the official
[Codex developer settings](https://learn.chatgpt.com/docs/developer-settings).
The prior Astra plan is retired. The replacement approval scope and complete
commands are in `.opsloop/demo-b-20261027/approvals/runtime-plan-luna.json`.

The frozen external verifier checks the same failing HTTP fixture before and
after repair, existing unit tests, normal HTTP behavior, Go vet, native AMD64
build and ARM64 cross-build. An AST policy rejects new dependencies, process or
network imports, compiler hooks, test skips and masking of the failure. ARM64
cross-build is not native ARM validation. Local tests are not remote CI.

## Commands and publication boundary

The build uses the existing Go 1.27.3 toolchain, with `GO111MODULE=off`,
`GOTOOLCHAIN=local`, `GOPROXY=off`, `GOSUMDB=off` and `CGO_ENABLED=0`. The
dedicated application has no third-party dependencies. No Docker daemon is
used to produce its content-addressed OCI images.

```sh
python3 -m unittest discover -s tests -p 'test_engineering*.py' -v
python3 -m tools.engineering_evidence --help
python3 -m tools.engineering_runner --help
python3 -m tools.engineering_publisher --help
```

Set `OPSLOOP_VERIFY_GO` to the pinned Go binary when running the parse-only
security regression. Actual Runner invocation additionally needs the exact
bundle hash, repository, commit, verifier hash and approved model execution.

The publisher gate has no credentials and executes no repository code. It
prepares exact GitHub requests only after independent review of the verified
candidate. A separate authorized host publishes the exact commits and draft
PR, then fetches the real PR/commit/tree/files to bind the receipt. `PR_CREATED`
requires that receipt and its actual URL/number. No merge, repaired-image
deployment or automatic Enforce activation is part of publication.

Durable progress and real observations are under `.opsloop/demo-b-20261027/`.
The milestone and final verification artifacts, not offline fixture tests or
Dashboard lamps, determine whether the real end-to-end run passed.

## Verified Demo B outcome (2026-10-27 KST)

Actual Incident `219f6a02-b8b0-4c47-b685-db1884d0dbb7` exhausted its one
Runtime relocation: the same empty-order HTTP 500 occurred before and after
placement on Azure ARM64. Guard confirmed the stop and Policy generation 4 is
disabled. Luna Medium generated the two-file repair in one ChatGPT-authenticated
request. The unchanged external HTTP reproduction failed before repair and all
15 independent checks passed afterward. Independent security review approved
the exact patch.

[Draft PR #6](https://github.com/SleepyMiny/OpsLoop/pull/6) contains the real
model output at commit `7e38e2b0196b726140c157b8d51b0fd5d33896da`, against
`demo/demo-b-baseline-20261027`. Authenticated GET observations of the PR,
commit parent, full tree and two changed blobs match the approved request.
The host GitHub CLI created the PR after the connector returned HTTP 403;
the coordinator checked that no matching PR existed before that fallback.

The live Demo B workload still runs the defective image; this milestone ends
at a reviewed draft PR, without merge or repaired-image deployment. Remote CI,
native execution of the patched ARM64 build, global actor/clock validation and
physical power-loss testing were not performed. The final evidence bundle
records these limits separately from the demonstrated Engineering E2E PASS.
