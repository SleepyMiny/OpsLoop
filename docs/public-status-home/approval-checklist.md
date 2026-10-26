# Production approval checklist — all infrastructure steps pending

This task grants local implementation/tests only. Do not run apply, deploy, login, secret creation,
D1 provisioning, service installation, push or PR commands from this checklist without approval.

1. Review the exact code diff and locally tested commit/tree; wire the new status tests into
   existing dashboard CI before merging. Keep verify/image/dashboard/publish approval semantics.
   PR/CI status is recorded in the final review report; merge and deployment remain separately gated.
2. Read-only check Cloudflare account/project bindings, current deployed version/asset manifest,
   free-plan usage/CPU budget/rate namespace compatibility, route and security events.403/1010
   remains NOT VERIFIED; do not weaken security rules. Assess public availability/load disclosure.
3. Approve one D1 and exactly the migration/bootstrap row; fill the D1/rate namespace placeholders.
   Preserve `opsloop` name/current URL and `/api/*` worker-first routes, asset-first elsewhere.
   No new hostname or public private-BFF route. Confirm compatibility date with actual Edge.
4. Approve a server-only HMAC Secret with an owner-generated high-entropy key/version. Never
   put it in vars, VITE_*, artifacts, D1, logs, Git or argv. Test current/next key rotation and
   rejection of old key while preserving the monotonic sequence. Have credential recovery steps.
5. Approve the narrowly scoped Kubernetes SA/roles/bindings separately. Server-side dry-run and
   `auth can-i` must prove named GET/Incident LIST success and all other namespaces/nodes,
   Secrets/Pod logs/exec/proxy/write/tokenRequest denial. Do not use existing admin credentials.
   Local GET transport tests are not Kubernetes authorization proof.
6. Approve short-lived token issuance/transfer/rotation and a dedicated Ubuntu account/paths.
   Verify existing internal Prometheus TLS/auth and Kubernetes endpoint reachability read-only.
   Verify no credentials leak on redirects and no arbitrary URLs/queries. Package a pinned binary.
7. Approve Collector service installation only after private files, clock synchronization, durable
   counter backup, resource limits, outbound TLS, restart/backoff and log redaction are checked.
   No listening port; no k3s/Docker/Jenkins changes. Token expiry must raise unavailable, not healthy.
8. Approve staged Edge publication. First retain existing static UI while testing authenticated
   synthetic ingest/status/CSP/routing; then compare an approved Collector observation against
   private source values before switching the Home entry. Test page/API routes, no-store and
   404/405, public Live block, both Replay records, offline Demo and History at the actual URL.
9. Test budget, rate429, missing bindings/DB outage, delayed observations, Collector interruption
   and recovery without stopping real cluster services. HMAC is not protection against a
   compromised authorized Collector forging values; this trust boundary must be accepted.
10. Prepare explicit rollback authorization: stop only the newly installed Collector, disable
    ingestion and return to recorded static Worker version. Never reset/delete D1/counter or
    silently return Demo values under the observed API. Restore/re-sync approval is separate.

Remaining production limits: Cloudflare per-PoP limits are not a global quota guarantee; D1
atomic write gate controls accepted writes only. Free10ms CPU/resource fit and visitor volume
need Edge measurement. Server service lifecycle and dedicated token rotation remain unverified.
No Enforce, workload changes, fault injection or private Live publication is part of this rollout.
