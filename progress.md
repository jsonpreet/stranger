# Project Progress

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
- [ ] Log streaming
- [ ] Single server deployment verification

## Phase 2 – UI & DX
- [x] Dashboard UI setup (Layout, Sidebar)
- [x] Pages (Overview, New Project, Details)
- [ ] Live logs view (UI ready, waiting for backend)
- [ ] Rollback UI
- [ ] Project management screens
- [ ] Template selection flow

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
