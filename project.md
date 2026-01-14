⚡ Fast PaaS Platform Blueprint (Coolify-But-Faster)

Goal: Build a self-hosted PaaS like Coolify, but significantly faster, simpler, and more predictable.

North Star: Git push → live app in seconds.

This document is a full end-to-end blueprint covering:
• Product philosophy
• Tech stack (exact choices)
• System architecture
• Deploy engine
• Server agent
• UI/UX
• Phased execution plan

This is written so you can start building immediately.

⸻

1. Product Philosophy (Why This Beats Coolify)

Coolify is powerful — but slow — because it:
• Makes runtime decisions
• Supports too many infra styles
• Rebuilds too often
• Solves orchestration dynamically

Our Core Belief

Speed comes from opinionation + precomputation.

Non-Negotiables
• ❌ No Kubernetes
• ❌ No Swarm
• ❌ No YAML pipelines
• ❌ No runtime framework detection
• ❌ No plugin marketplace (v1)

What We Do Instead
• Precompiled deploy plans
• Static routing
• Hash-based builds
• Single binary agents
• Dumb, fast deploys

⸻

2. High-Level Architecture

User
│
▼
Web Dashboard (Next.js)
│ API
▼
Control Plane (Go API)
│ Jobs
▼
Server Agent (Go)
│ Docker API
▼
Containers + Router (NGINX/Caddy)

Key Rule

The deploy path must be the shortest path.

Everything else is secondary.

⸻

3. Tech Stack (Exact Choices)

Backend / Infra

Area Tech Reason
Control Plane Go Fast, static, low memory
Server Agent Go Single binary, reliable
Job Queue SQLite (WAL) Zero network, fast
Builds Docker BuildKit Layer caching
Runtime Docker Universal, boring
Router NGINX or Caddy Static + reload
OS Support Ubuntu/Debian Predictable

Frontend / UI

Area Tech Reason
Dashboard Next.js (App Router) Fast dev + SSR
Styling Tailwind Speed, consistency
Components shadcn/ui Clean SaaS UI
Realtime SSE/WebSockets Live logs

Storage
• SQLite (single node)
• Optional S3-compatible backups later

⸻

4. Core System Components

4.1 Control Plane (API Server)

Responsibilities:
• Project creation
• Template selection
• Git webhook handling
• Job creation
• Node registration

Does NOT:
• Build
• Run containers
• Manage routing

Stateless by design.

⸻

4.2 Server Agent (The Muscle)

Runs on each server.

Responsibilities:
• Execute deploy plans
• Build Docker images
• Start/stop containers
• Update router
• Stream logs
• Heal on restart

Single systemd service.

⸻

4.3 Deploy Engine (Heart of Speed)

Deploy Plan (Generated Once)

{
"template": "nextjs-standalone",
"build": { "dockerfile": "Dockerfile.next" },
"runtime": { "port": 3000 },
"router": { "domain": "app.example.com" }
}

At deploy time:
• ❌ No detection
• ❌ No guessing
• ✅ Just execution

Build Hash Strategy

hash = sha256(
template + git_sha + env_hash + lockfile_hash
)

If hash exists → skip build.

⸻

4.4 Templates (Huge Performance Win)

Supported templates (v1):
• node-static
• node-server
• nextjs-standalone
• php-fpm
• laravel-octane
• astro-static

Each template defines:
• Dockerfile
• Exposed ports
• Health checks
• Startup command

No runtime analysis.

⸻

4.5 Routing (Static & Atomic)
• Full config generated
• Written to temp file
• Atomic replace
• Hot reload

Rollback = pointer switch.

⸻

5. Server Agent Installer

One-Liner Install

curl -fsSL https://get.yourpaas.sh | sudo sh

What It Does 1. Validate OS 2. Install Docker (if missing) 3. Download static agent 4. Setup directories 5. Register systemd service 6. Verify health

Target time: < 60 seconds.

⸻

6. UI / UX Design (Speed Perception Matters)

UI Principles
• No wizards
• No clutter
• Immediate feedback
• Logs visible instantly

Core Screens

6.1 Dashboard
• Servers
• Projects
• Recent deploys
• Health status

6.2 New Project Flow 1. Connect Git repo 2. Select template 3. Set env vars 4. Deploy

(4 steps. No more.)

6.3 Deploy View
• Live logs
• Build cache hit/miss
• Status badge
• Rollback button

6.4 Server View
• Disk usage
• CPU/RAM
• Agent status
• Restart agent

Minimal, dense, SaaS-grade.

⸻

7. Performance Targets

Action Target
Cached deploy < 2s
Fresh build < 20s
Rollback < 1s
Router reload < 200ms
Install < 60s

If targets slip → cut features.

⸻

8. Phased Build Plan

Phase 0 – Foundation
• Repo setup
• Go monorepo
• Agent skeleton
• Docker API integration

⸻

Phase 1 – Core Deploy (MVP)
• Server agent
• Deploy plans
• Docker builds
• Routing
• Logs

✅ Single server

⸻

Phase 2 – UI & DX
• Dashboard UI
• Live logs
• Rollbacks
• Project management

⸻

Phase 3 – Stability
• Agent auto-heal
• Disk cleanup
• Backup/restore
• Node reconnect

⸻

Phase 4 – Power Features
• Cron jobs
• Workers
• Volumes
• Secrets management

⸻

Phase 5 – Nice-to-Have (Careful)
• Multi-node (no clusters)
• Marketplace (read-only)
• Metrics

Never touch deploy path.

⸻

9. Why This Will Be Faster Than Coolify

Area Coolify This Platform
Deploy logic Dynamic Precompiled
Infra Flexible Opinionated
Build reuse Partial Hash-perfect
Install Heavy One-liner
Runtime Complex Minimal

Speed by subtraction.

⸻

10. Final Word

This platform wins by being:
• Boring internally
• Fast externally
• Honest about limits

Predictability beats flexibility.

If you ship this well, developers will feel the difference.

⸻

Next logical docs to create:
• Template specification
• Agent ↔ Control Plane protocol
• Security model
• Auto-update strategy

Ready when you are.
