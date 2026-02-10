# Stranger Local Testing Guide

## 1) Prerequisites

- Docker running locally
- Go installed
- Node.js + npm installed
- Caddy installed and available in `PATH`

## 2) Required environment variables

Use the same values in both `agent` and `control-plane` terminals.

```bash
export STRANGER_API_TOKEN="dev-api-token"
export STRANGER_AGENT_TOKEN="dev-agent-token"
export STRANGER_SECRETS_KEY="base64:$(openssl rand -base64 32)"
```

Optional key-rotation config:

```bash
export STRANGER_SECRETS_KEYS="k1=$(openssl rand -base64 32),k2=$(openssl rand -base64 32)"
export STRANGER_SECRETS_PRIMARY_KEY_ID="k2"
```

## 3) Start backend services

### One-command startup (recommended)

```bash
cd /Users/harpreet/Github/stranger
./dev-start.sh start
```

This starts:
- control-plane (`:8080`)
- agent (`:3000`)
- dashboard (`:3001`)
- preflight checks (including Docker daemon reachability)

Useful commands:

```bash
./dev-start.sh status
./dev-start.sh logs
./dev-start.sh stop
./dev-start.sh token
./dev-start.sh env
./dev-start.sh fresh
```

If Docker is not running, startup stops early with fix instructions.
For UI-only testing without deployments:

```bash
STRANGER_SKIP_DOCKER_CHECK=1 ./dev-start.sh start
```

`dev-start.sh` now auto-detects Docker host from current Docker context (including macOS `~/.docker/run/docker.sock`) and passes it to the agent.

If you prefer the old path, this still works:

```bash
./scripts/dev-start.sh start
```

### Manual startup (alternative)

Terminal A:

```bash
cd /Users/harpreet/Github/stranger/services/agent
export STRANGER_CONTROL_PLANE_URL="http://localhost:8080"
go run .
```

Terminal B:

```bash
cd /Users/harpreet/Github/stranger/services/control-plane
export STRANGER_AGENT_URL="http://localhost:3000"
go run .
```

## 4) Start dashboard

Terminal C:

```bash
cd /Users/harpreet/Github/stranger/apps/dashboard
npm install
npm run dev
```

Open `http://localhost:3001`.

First-run UX now:
1. Create admin account (`/auth/setup`)
2. Login (`/auth/login`)
3. Add first server (`/onboarding/server`) with localhost or remote
4. Use full dashboard with sidebar and pages

If you used `./dev-start.sh`, the dashboard receives `STRANGER_DASHBOARD_API_TOKEN` fallback automatically.

If Settings still shows missing token:
1. Run `./dev-start.sh token` and verify a token is printed.
2. Run `./dev-start.sh restart` (or `./dev-start.sh fresh` for clean install flow).
3. Refresh `/settings`; source should show `Process env` or `Dev env file`.

## 5) Suggested smoke test flow

1. Create admin account + login + add first server
2. Open **Marketplace** and choose an app (or create project manually)
3. Verify required env vars are detected from repo
4. Click **Prepare + Deploy** and fill missing env vars
5. Watch live logs stream and copy logs if needed
6. Verify deploy status reaches `succeeded`

## 6) CLI verification (optional)

```bash
cd /Users/harpreet/Github/stranger
./scripts/verify_deploy.sh
```
