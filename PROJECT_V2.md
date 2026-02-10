# PROJECT V2 — Stranger (Coolify-style, Go-first)

## 1) Decision: Go or TypeScript?

## Short answer
Use **Go for the backend/control-plane/agent**, and keep **TypeScript only for dashboard/frontend**.

## Why
For a secure, fast, and predictable server/app management platform, the critical deploy path benefits from Go:

- Better runtime efficiency (lower memory, stable latency under load)
- Static single binaries for agent/control-plane distribution
- Strong concurrency model for deploy/log stream workers
- Smaller operational surface than Node-based backend stacks
- Lower dependency churn compared to large npm ecosystems

TypeScript is still excellent for:
- Dashboard UI (Next.js)
- Admin UX and realtime views
- Fast product iteration on frontend

## Final recommendation
- **Go**: control-plane API, deploy engine, queue, agent, router integration
- **TypeScript**: web dashboard only
- **Do not build deploy-critical backend in TypeScript**

---

## 2) Product Goal (V2)

Build a **secure-by-default**, **fast**, and **operationally boring** self-hosted PaaS:
- Git push to live app with deterministic deploy plans
- Safe multi-tenant boundaries on single server first
- Fast rollback and low-downtime deploys
- Minimal moving parts

---

## 3) Current Status (from repo)

Good foundation exists:
- Go control-plane and Go agent skeletons are present
- Docker build/deploy path exists
- Caddy route generation exists
- SQLite-backed project storage exists

Critical gaps to solve before scaling:
- No authn/authz between control-plane and agent
- `/deploy`, `/projects`, `/logs` have no access control
- No deploy job state machine or durable queueing
- Caddy route handling is overwrite-only (single-route MVP)
- No strict validation for domain/template/repo inputs
- No secret management/encryption model yet

---

## 4) V2 Architecture Direction

- **Control Plane (Go)**: API, auth, project/deploy orchestration, job state, audit logs
- **Agent (Go)**: build/deploy/rollback executor, logs streaming, health reporting
- **Router (Caddy)**: atomic config writes + reload with full route table
- **Database**: SQLite WAL (single-node), clean migration path to Postgres later
- **Frontend**: Next.js dashboard consuming control-plane API

---

## 5) Security Model (must-have)

1. **Service auth**
   - mTLS or signed agent tokens between control-plane and agents
   - Short-lived deploy tokens (scoped per job)

2. **API protection**
   - JWT/session auth for users
   - RBAC (owner/admin/viewer at minimum)
   - Rate limiting + request size limits

3. **Input and config safety**
   - Strict schema validation for all API payloads
   - Domain sanitization before router templating
   - Template allowlist only (no arbitrary dockerfile paths in v1)

4. **Secrets**
   - Encrypt at rest (AES-GCM with key from env/KMS)
   - Never log secret values
   - Redaction in log streams and deploy events

5. **Runtime hardening**
   - Container security defaults (read-only FS where possible, dropped caps)
   - Network isolation between app containers
   - Bind internal app ports to localhost and proxy through Caddy

---

## 6) Performance Model (must-have)

- Deploy plan precompilation and strict template system
- Build hash caching: `template + git_sha + env_hash + lockfile_hash`
- BuildKit cache import/export
- Bounded worker pools for builds/deploys
- Streaming logs via SSE/WebSocket with backpressure
- Fast rollback by switching stable image/tag pointer

---

## 7) Execution Plan (V2)

## Phase A — Secure MVP hardening (now)
- Add auth middleware for control-plane APIs
- Add control-plane ↔ agent authentication
- Introduce deploy job table + state machine
- Add strict request validation and template allowlist
- Implement proper route table persistence and atomic Caddy reload
- Add structured audit events

## Phase B — Reliable deploy pipeline
- Queue workers with retries/timeouts/cancellation
- Deterministic build hashing and cache skip logic
- Health-check driven rollout + rollback
- Deployment history and status API
- Log stream cleanup (demux Docker stream output)

## Phase C — Safety + operations
- Secrets encryption and rotation model
- Resource limits and quotas per project
- Garbage collection (old images/containers/build cache)
- Backup/restore flows for DB and metadata
- Agent heartbeat and reconnect logic

## Phase D — UX and scale
- Dashboard: live logs, rollback, deploy timeline
- Multi-server support (without Kubernetes)
- Optional Postgres backend for larger installs
- Metrics and alert hooks

---

## 8) Concrete Next Sprint (recommended)

1. Implement authn/authz baseline in control-plane
2. Add deploy_jobs schema + worker loop
3. Replace direct `/deploy` fire-and-forget with queued execution
4. Harden router layer (full routes + atomic write + validation)
5. Add end-to-end test: create project → deploy → logs → rollback

---

## 9) Success Criteria

- Cached deploy: < 2s target path
- Fresh build deploy: < 20s (small apps)
- Rollback trigger to healthy: < 1s route switch
- Zero unauthenticated deploy/log endpoints
- Reproducible deploy behavior across restarts

---

## Final Position

For this product category, **Go is the right backend language** for your goals (secure, safe, fast, predictable).  
Use **TypeScript where it shines** (dashboard UX), not on the deploy-critical backend path.
