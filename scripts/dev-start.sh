#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEV_DIR="$ROOT_DIR/.dev"
LOG_DIR="$DEV_DIR/logs"
ENV_FILE="$DEV_DIR/dev.env"
PID_FILE="$DEV_DIR/dev.pids"

CONTROL_PLANE_DIR="$ROOT_DIR/services/control-plane"
AGENT_DIR="$ROOT_DIR/services/agent"
DASHBOARD_DIR="$ROOT_DIR/apps/dashboard"
CONTROL_PLANE_DB_BASE="$CONTROL_PLANE_DIR/stranger.db"
DASHBOARD_STATE_FILE="$DEV_DIR/dashboard-state.json"
CONTROL_PLANE_PORT="8080"
AGENT_PORT="3000"
DASHBOARD_PORT="3301"
RESOLVED_DOCKER_HOST=""

mkdir -p "$DEV_DIR" "$LOG_DIR"

command="${1:-start}"

require_commands() {
  local commands=("go" "node" "npm" "curl" "openssl" "lsof" "docker")
  for cmd in "${commands[@]}"; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
      echo "Missing required command: $cmd"
      exit 1
    fi
  done
}

# Install nixpacks once globally — needed to build PHP/Python/Ruby apps that
# have no Dockerfile. Uses the official install script (curl | bash).
install_nixpacks_if_missing() {
  if command -v nixpacks >/dev/null 2>&1; then
    return
  fi
  echo "Installing nixpacks (one-time, required for PHP/Python app builds)..."
  if ! curl -sSfL https://nixpacks.com/install.sh | bash; then
    echo "Warning: nixpacks install failed. PHP/Python apps may not build correctly."
    echo "  Manual install: https://nixpacks.com/docs/install"
  fi
}

require_docker_daemon() {
  if [[ "${STRANGER_SKIP_DOCKER_CHECK:-}" == "1" ]]; then
    return
  fi

  local docker_host
  docker_host="$(resolve_docker_host || true)"
  if [[ -n "$docker_host" ]]; then
    RESOLVED_DOCKER_HOST="$docker_host"
    return
  fi

  echo "Docker daemon is not reachable (or slow to respond)."
  echo "1. Start/Restart Docker Desktop and wait for 'Engine running'."
  echo "2. If Docker is running, try: ./dev-start.sh restart"
  echo "3. To bypass this check for UI-only development, run:"
  echo "   STRANGER_SKIP_DOCKER_CHECK=1 ./dev-start.sh start"
  exit 1
}

list_candidate_docker_hosts() {
  if [[ -n "${STRANGER_DOCKER_HOST:-}" ]]; then
    echo "$STRANGER_DOCKER_HOST"
  fi

  if [[ -n "${DOCKER_HOST:-}" ]]; then
    echo "$DOCKER_HOST"
  fi

  local current_context
  current_context="$(docker context show 2>/dev/null || true)"
  if [[ -n "$current_context" ]]; then
    docker context inspect "$current_context" --format '{{ (index .Endpoints "docker").Host }}' 2>/dev/null | head -n 1
  fi

  docker context ls --format '{{.DockerEndpoint}}' 2>/dev/null || true

  local mac_socket="$HOME/.docker/run/docker.sock"
  if [[ -S "$mac_socket" ]]; then
    echo "unix://$mac_socket"
  fi

  local colima_socket="$HOME/.colima/default/docker.sock"
  if [[ -S "$colima_socket" ]]; then
    echo "unix://$colima_socket"
  fi

  if [[ -S "/var/run/docker.sock" ]]; then
    echo "unix:///var/run/docker.sock"
  fi
}

resolve_docker_host() {
  local host
  while IFS= read -r host; do
    host="$(echo "$host" | tr -d '\r' | xargs)"
    if [[ -z "$host" || "$host" == "<no value>" ]]; then
      continue
    fi
    if docker --host "$host" info >/dev/null 2>&1; then
      echo "$host"
      return 0
    fi
  done < <(list_candidate_docker_hosts | awk 'NF && !seen[$0]++')

  return 1
}

generate_env_file() {
  local api_token
  local agent_token
  local secrets_key

  api_token="dev-api-$(openssl rand -hex 10)"
  agent_token="dev-agent-$(openssl rand -hex 10)"
  secrets_key="base64:$(openssl rand -base64 32 | tr -d '\n')"

  cat >"$ENV_FILE" <<EOF
STRANGER_API_TOKEN="$api_token"
STRANGER_AGENT_TOKEN="$agent_token"
STRANGER_SECRETS_KEY="$secrets_key"
STRANGER_AGENT_URL="http://localhost:3000"
STRANGER_CONTROL_PLANE_URL="http://localhost:8080"
STRANGER_DASHBOARD_URL="http://localhost:3301"
STRANGER_DASHBOARD_API_TOKEN="$api_token"
EOF
}

load_env() {
  if [[ ! -f "$ENV_FILE" ]]; then
    generate_env_file
  fi
  set -a
  source "$ENV_FILE"
  set +a
}

load_pids() {
  CONTROL_PLANE_PID=""
  AGENT_PID=""
  DASHBOARD_PID=""
  if [[ -f "$PID_FILE" ]]; then
    while IFS='=' read -r name pid; do
      case "$name" in
        control_plane) CONTROL_PLANE_PID="$pid" ;;
        agent) AGENT_PID="$pid" ;;
        dashboard) DASHBOARD_PID="$pid" ;;
      esac
    done <"$PID_FILE"
  fi
}

save_pids() {
  cat >"$PID_FILE" <<EOF
control_plane=${CONTROL_PLANE_PID:-}
agent=${AGENT_PID:-}
dashboard=${DASHBOARD_PID:-}
EOF
}

is_running() {
  local pid="${1:-}"
  [[ -n "$pid" ]] && kill -0 "$pid" >/dev/null 2>&1
}

pid_on_port() {
  local port="$1"
  lsof -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null | head -n 1 || true
}

service_status() {
  local managed_pid="$1"
  local port="$2"

  if is_running "$managed_pid"; then
    echo "running (pid: $managed_pid)"
    return
  fi

  local external_pid
  external_pid="$(pid_on_port "$port")"
  if [[ -n "$external_pid" ]]; then
    echo "running (external pid: $external_pid)"
    return
  fi

  echo "stopped"
}

start_control_plane() {
  if is_running "$CONTROL_PLANE_PID"; then
    echo "Control-plane already running (pid: $CONTROL_PLANE_PID)"
    return
  fi

  local external_pid
  external_pid="$(pid_on_port "$CONTROL_PLANE_PORT")"
  if [[ -n "$external_pid" ]]; then
    echo "Control-plane already listening on :$CONTROL_PLANE_PORT (external pid: $external_pid)"
    CONTROL_PLANE_PID=""
    return
  fi

  (
    cd "$CONTROL_PLANE_DIR"
    export STRANGER_API_TOKEN STRANGER_AGENT_TOKEN STRANGER_SECRETS_KEY
    export STRANGER_SECRETS_KEYS="${STRANGER_SECRETS_KEYS:-}"
    export STRANGER_SECRETS_PRIMARY_KEY_ID="${STRANGER_SECRETS_PRIMARY_KEY_ID:-}"
    export STRANGER_AGENT_URL STRANGER_CONTROL_PLANE_URL
    nohup go run . >"$LOG_DIR/control-plane.log" 2>&1 &
    echo $! >"$DEV_DIR/control-plane.pid"
  )
  CONTROL_PLANE_PID="$(cat "$DEV_DIR/control-plane.pid")"
}

start_agent() {
  if is_running "$AGENT_PID"; then
    echo "Agent already running (pid: $AGENT_PID)"
    return
  fi

  local external_pid
  external_pid="$(pid_on_port "$AGENT_PORT")"
  if [[ -n "$external_pid" ]]; then
    echo "Agent already listening on :$AGENT_PORT (external pid: $external_pid)"
    AGENT_PID=""
    return
  fi

  (
    cd "$AGENT_DIR"
    local docker_host
    docker_host="${RESOLVED_DOCKER_HOST:-$(resolve_docker_host || true)}"
    if [[ -n "$docker_host" ]]; then
      export STRANGER_DOCKER_HOST="$docker_host"
      export DOCKER_HOST="$docker_host"
    fi
    export STRANGER_AGENT_TOKEN STRANGER_CONTROL_PLANE_URL
    nohup go run . >"$LOG_DIR/agent.log" 2>&1 &
    echo $! >"$DEV_DIR/agent.pid"
  )
  AGENT_PID="$(cat "$DEV_DIR/agent.pid")"
}

start_dashboard() {
  if is_running "$DASHBOARD_PID"; then
    echo "Dashboard already running (pid: $DASHBOARD_PID)"
    return
  fi

  local external_pid
  external_pid="$(pid_on_port "$DASHBOARD_PORT")"
  if [[ -n "$external_pid" ]]; then
    echo "Dashboard already listening on :$DASHBOARD_PORT (external pid: $external_pid)"
    DASHBOARD_PID=""
    return
  fi

  (
    cd "$DASHBOARD_DIR"
    if [[ ! -d node_modules ]]; then
      npm install >"$LOG_DIR/dashboard-install.log" 2>&1
    fi
    export STRANGER_CONTROL_PLANE_URL
    export NEXT_PUBLIC_CONTROL_PLANE_URL="$STRANGER_CONTROL_PLANE_URL"
    export STRANGER_DASHBOARD_API_TOKEN="$STRANGER_API_TOKEN"
    nohup npm run dev >"$LOG_DIR/dashboard.log" 2>&1 &
    echo $! >"$DEV_DIR/dashboard.pid"
  )
  DASHBOARD_PID="$(cat "$DEV_DIR/dashboard.pid")"
}

wait_for_http_status() {
  local name="$1"
  local url="$2"
  local expected_status="$3"
  local header="${4:-}"

  for _ in {1..60}; do
    local status
    if [[ -n "$header" ]]; then
      status="$(curl -s -o /dev/null -w "%{http_code}" -H "$header" "$url" || true)"
    else
      status="$(curl -s -o /dev/null -w "%{http_code}" "$url" || true)"
    fi

    if [[ "$status" == "$expected_status" ]]; then
      return 0
    fi
    sleep 1
  done

  echo "Timed out waiting for $name ($url). Check log: $LOG_DIR/$name.log"
  return 1
}

start_all() {
  require_commands
  load_env
  require_docker_daemon
  install_nixpacks_if_missing
  load_pids

  start_control_plane
  start_agent
  start_dashboard
  save_pids

  wait_for_http_status "control-plane" "${STRANGER_CONTROL_PLANE_URL}/healthz" "200"
  wait_for_http_status "agent" "http://localhost:3000/deploy" "405" "X-Agent-Token: ${STRANGER_AGENT_TOKEN}"
  wait_for_http_status "dashboard" "${STRANGER_DASHBOARD_URL}" "200"

  echo
  echo "✅ Stranger dev stack is running"
  echo "Dashboard:        ${STRANGER_DASHBOARD_URL}"
  echo "Control-plane:    ${STRANGER_CONTROL_PLANE_URL}"
  echo "Agent:            http://localhost:3000"
  echo "Docker host:      ${RESOLVED_DOCKER_HOST:-unknown}"
  echo "API token:        ${STRANGER_API_TOKEN}"
  echo
  echo "Logs:"
  echo "  $LOG_DIR/control-plane.log"
  echo "  $LOG_DIR/agent.log"
  echo "  $LOG_DIR/dashboard.log"
  echo
  echo "Tips:"
  echo "  - Stop services: ./dev-start.sh stop"
  echo "  - Service status: ./dev-start.sh status"
  echo "  - Follows logs: ./dev-start.sh logs"
}

stop_process() {
  local name="$1"
  local pid="$2"
  if is_running "$pid"; then
    kill "$pid" >/dev/null 2>&1 || true
    sleep 1
    if is_running "$pid"; then
      kill -9 "$pid" >/dev/null 2>&1 || true
    fi
    echo "Stopped $name (pid: $pid)"
  fi
}

stop_by_port() {
  local name="$1"
  local port="$2"
  local pid
  pid="$(pid_on_port "$port")"
  if [[ -n "$pid" ]]; then
    stop_process "$name" "$pid"
  fi
}

stop_all() {
  load_pids
  stop_process "dashboard" "${DASHBOARD_PID:-}"
  stop_process "agent" "${AGENT_PID:-}"
  stop_process "control-plane" "${CONTROL_PLANE_PID:-}"
  stop_by_port "dashboard" "$DASHBOARD_PORT"
  stop_by_port "agent" "$AGENT_PORT"
  stop_by_port "control-plane" "$CONTROL_PLANE_PORT"
  rm -f "$PID_FILE" "$DEV_DIR/control-plane.pid" "$DEV_DIR/agent.pid" "$DEV_DIR/dashboard.pid"
}

status_all() {
  load_env
  load_pids

  echo "Control-plane: $(service_status "$CONTROL_PLANE_PID" "$CONTROL_PLANE_PORT")"
  echo "Agent:         $(service_status "$AGENT_PID" "$AGENT_PORT")"
  echo "Dashboard:     $(service_status "$DASHBOARD_PID" "$DASHBOARD_PORT")"
  echo
  echo "URLs:"
  echo "  Dashboard:     ${STRANGER_DASHBOARD_URL}"
  echo "  Control-plane: ${STRANGER_CONTROL_PLANE_URL}"
}

show_logs() {
  tail -f "$LOG_DIR/control-plane.log" "$LOG_DIR/agent.log" "$LOG_DIR/dashboard.log"
}

show_token() {
  load_env
  echo "${STRANGER_API_TOKEN}"
}

show_env_source() {
  load_env
  echo "source \"$ENV_FILE\""
}

fresh_start() {
  load_env
  stop_all
  rm -f "$CONTROL_PLANE_DB_BASE" "$CONTROL_PLANE_DB_BASE-shm" "$CONTROL_PLANE_DB_BASE-wal"
  rm -f "$DASHBOARD_STATE_FILE"
  rm -f "$LOG_DIR/control-plane.log" "$LOG_DIR/agent.log" "$LOG_DIR/dashboard.log"
  echo "Reset local control-plane DB and dashboard state."
  start_all
}

usage() {
  cat <<EOF
Usage: ./dev-start.sh [start|stop|restart|fresh|status|logs|token|env]

start   Start control-plane, agent, and dashboard (default)
stop    Stop all started services
restart Restart all services
fresh   Reset local DB/state and start fresh install flow
status  Show service status
logs    Tail all service logs
token   Print current STRANGER_API_TOKEN
env     Print command to load env in current shell

Alias:
  ./scripts/dev-start.sh ... (works the same)
EOF
}

case "$command" in
  start)
    start_all
    ;;
  stop)
    stop_all
    ;;
  restart)
    stop_all
    start_all
    ;;
  fresh)
    fresh_start
    ;;
  status)
    status_all
    ;;
  logs)
    show_logs
    ;;
  token)
    show_token
    ;;
  env)
    show_env_source
    ;;
  *)
    usage
    exit 1
    ;;
esac
