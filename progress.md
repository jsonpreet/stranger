# Project Progress

## V2 Hardening Sprint (In Progress)
- [x] Control Plane API token authentication (`Bearer`)
- [x] Agent endpoint authentication (`X-Agent-Token`)
- [x] Deploy job queue with durable SQLite state machine
- [x] Deploy job APIs (`/deploy/jobs`, `/deploy/jobs/{id}`)
- [x] Control Plane deploy dispatch worker (queued → dispatched/failed)
- [x] Router state persistence + atomic Caddy config writes
- [x] Input validation baseline (project name/template/repo URL)
- [x] Agent rollout status callbacks (agent → control-plane)
- [x] Secrets encryption at rest (AES-GCM + SQLite encrypted payloads)
- [x] Secret key versioning + rotation endpoint (dry-run + re-encrypt)
- [x] Fresh-install auth onboarding (setup/login/server selection)
- [x] Dev bootstrap script (`dev-start.sh`) with token/env/fresh commands
- [x] Dashboard token fallback chain (cookie/env/.dev/dev.env)

## Phase 0 – Foundation
- [x] Repo setup (Directory structure)
- [x] Go monorepo setup (skeletons)
- [x] Agent skeleton code
- [x] Control Plane skeleton code
- [x] Docker API integration (initial stub)

## Phase 1 – Core Deploy (MVP)
- [x] Server agent core logic (types, builder, container, router)
- [x] Control Plane logic (SQLite, API)
- [ ] Deploy plans generation (Mocked in Control Plane)
- [x] Docker build integration
- [x] Routing (NGINX/Caddy config generation)
- [x] Log streaming
- [x] Single server deployment verification

## Phase 2 – UI & DX
- [x] Dashboard UI setup (Layout, Sidebar)
- [x] Pages (Overview, New Project, Details)
- [x] Live logs view (streaming + copy logs)
- [ ] Rollback UI
- [x] Project management screens
- [x] Template selection flow
- [x] Auth onboarding flow (create account/login)
- [x] Server onboarding flow (localhost/remote)
- [x] Marketplace/app store UI with GitHub repo mapping
- [x] Repo env detection before deploy

## Phase 3 – Stability
- [ ] Agent auto-heal
- [ ] Disk cleanup
- [ ] Backup/restore
- [ ] Node reconnection logic

## Phase 4 – Power Features
- [ ] Cron jobs
- [ ] Workers
- [ ] Volume management
- [ ] Secrets management

## Phase 5 – Nice-to-Have
- [ ] Multi-node support
- [ ] Marketplace
- [ ] Metrics collection
