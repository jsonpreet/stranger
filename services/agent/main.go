package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	"github.com/stranger/agent/internal/builder"
	"github.com/stranger/agent/internal/container"
	"github.com/stranger/agent/internal/logs"
	"github.com/stranger/agent/internal/router"
	"github.com/stranger/agent/types"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	agentToken := strings.TrimSpace(os.Getenv("STRANGER_AGENT_TOKEN"))
	if agentToken == "" {
		slog.Error("Missing required env var", "name", "STRANGER_AGENT_TOKEN")
		os.Exit(1)
	}
	listenAddr := envOrDefault("STRANGER_AGENT_LISTEN_ADDR", ":3000")
	controlPlaneURL := envOrDefault("STRANGER_CONTROL_PLANE_URL", "http://localhost:8080")

	// Init Docker
	dockerCli, err := newDockerClient()
	if err != nil {
		slog.Warn("Failed to initialize Docker client", "error", err)
		// We proceed without a functional dockerCli. 
		// Most operations will fail with a warning, but the service stays up.
	}

	// Init Services
	buildSvc := builder.NewBuilder(dockerCli)
	containerSvc := container.NewManager(dockerCli)
	routerSvc := router.NewCaddyRouter()
	logSvc := logs.NewStreamer(dockerCli)
	callbackClient := &http.Client{
		Timeout: 10 * time.Second,
	}

	// In-flight deploy tracking for graceful shutdown
	var deployWg sync.WaitGroup

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()

	// Deploy Handler
	mux.HandleFunc("/deploy", withAgentAuth(agentToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var plan types.DeployPlan
		if err := json.NewDecoder(r.Body).Decode(&plan); err != nil {
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}

		slog.Info("Received deploy plan", "id", plan.ID, "repo", plan.Build.RepoURL)

		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"status": "deploy_started", "id": plan.ID})

		deployWg.Add(1)
		go func() {
			defer deployWg.Done()
			deployCtx := context.Background()
			containerName := fmt.Sprintf("app-%s", plan.ProjectID)

			// Create a build log for this deploy so /logs can stream build output
			buildLog := logs.DefaultRegistry.NewBuild(plan.ProjectID)
			defer buildLog.Close()

			reportStatus := func(status, errorMessage, domain, hostPort, imageTag string) {
				if strings.TrimSpace(plan.JobID) == "" {
					return
				}
				callback := types.DeployCallback{
					JobID:    plan.JobID,
					PlanID:   plan.ID,
					Status:   status,
					Error:    errorMessage,
					Domain:   domain,
					HostPort: hostPort,
					ImageTag: imageTag,
				}
				if err := sendDeployCallback(deployCtx, callbackClient, controlPlaneURL, agentToken, callback); err != nil {
					slog.Error("Failed to send deploy callback", "job_id", plan.JobID, "status", status, "error", err)
				}
			}

			reportStatus(types.DeployCallbackStatusRunning, "", "", "", "")

			// 1. Build
			imageTag, err := buildSvc.Build(deployCtx, plan, buildLog)
			if err != nil {
				slog.Error("Build failed", "error", err)
				fmt.Fprintf(buildLog, "\n[deploy] Build failed: %v\n", err)
				reportStatus(types.DeployCallbackStatusFailed, fmt.Sprintf("build failed: %v", err), "", "", "")
				return
			}
			fmt.Fprintf(buildLog, "\n[deploy] Starting container...\n")

			// 2. Run
			if err := containerSvc.Deploy(deployCtx, plan, imageTag); err != nil {
				slog.Error("Deploy failed", "error", err)
				reportStatus(types.DeployCallbackStatusFailed, fmt.Sprintf("deploy failed: %v", err), "", "", "")
				return
			}

			// 3. Find Port
			hostPort, err := containerSvc.GetHostPort(deployCtx, containerName, plan.Runtime.Port)
			if err != nil {
				slog.Error("Failed to resolve port", "error", err)
				reportStatus(types.DeployCallbackStatusFailed, fmt.Sprintf("port resolution failed: %v", err), "", "", "")
				return
			}

			// 4. Health Check
			if err := containerSvc.WaitReady(deployCtx, hostPort, 30*time.Second); err != nil {
				slog.Error("Health check failed", "error", err)
				_ = containerSvc.Remove(deployCtx, containerName)
				containerSvc.PruneStoppedContainers(deployCtx, plan.ProjectID)
				reportStatus(types.DeployCallbackStatusFailed, fmt.Sprintf("health check failed: %v", err), "", "", "")
				return
			}

			// 5. Update Router
			if err := routerSvc.UpdateRoute(plan.Router.Domain, hostPort); err != nil {
				slog.Error("Router update failed", "error", err)
				reportStatus(types.DeployCallbackStatusFailed, fmt.Sprintf("router update failed: %v", err), plan.Router.Domain, hostPort, "")
				return
			}

			// 6. Prune old images
			if pruneErr := containerSvc.PruneProjectImages(deployCtx, plan.ProjectID, 3); pruneErr != nil {
				slog.Warn("Failed to prune old images", "project_id", plan.ProjectID, "error", pruneErr)
			}

			slog.Info("Deploy Success!", "domain", plan.Router.Domain, "host_port", hostPort)
			fmt.Fprintf(buildLog, "[deploy] Deploy succeeded. Domain: %s  Port: %s\n", plan.Router.Domain, hostPort)
			reportStatus(types.DeployCallbackStatusSucceeded, "", plan.Router.Domain, hostPort, imageTag)
		}()
	}))

	// Stack Deploy Handler — multi-container pipeline
	mux.HandleFunc("/deploy/stack", withAgentAuth(agentToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var plan types.StackDeployPlan
		if err := json.NewDecoder(r.Body).Decode(&plan); err != nil {
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}

		slog.Info("Received stack deploy plan", "id", plan.ID, "services", len(plan.Services))
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"status": "stack_deploy_started", "id": plan.ID})

		deployWg.Add(1)
		go func() {
			defer deployWg.Done()
			deployCtx := context.Background()

			buildLog := logs.DefaultRegistry.NewBuild(plan.ProjectID)
			defer buildLog.Close()

			reportStatus := func(status, errorMessage, domain, hostPort, imageTag string) {
				if strings.TrimSpace(plan.JobID) == "" {
					return
				}
				callback := types.DeployCallback{
					JobID:    plan.JobID,
					PlanID:   plan.ID,
					Status:   status,
					Error:    errorMessage,
					Domain:   domain,
					HostPort: hostPort,
					ImageTag: imageTag,
				}
				if err := sendDeployCallback(deployCtx, callbackClient, controlPlaneURL, agentToken, callback); err != nil {
					slog.Error("Failed to send stack callback", "job_id", plan.JobID, "status", status, "error", err)
				}
			}

			reportStatus(types.DeployCallbackStatusRunning, "", "", "", "")

			hostPort, err := containerSvc.DeployStack(deployCtx, plan, buildLog, buildSvc)
			if err != nil {
				slog.Error("Stack deploy failed", "error", err)
				fmt.Fprintf(buildLog, "\n[stack] Deploy failed: %v\n", err)
				reportStatus(types.DeployCallbackStatusFailed, err.Error(), "", "", "")
				return
			}

			if err := routerSvc.UpdateRoute(plan.Router.Domain, hostPort); err != nil {
				slog.Error("Stack router update failed", "error", err)
				reportStatus(types.DeployCallbackStatusFailed, fmt.Sprintf("router update failed: %v", err), plan.Router.Domain, hostPort, "")
				return
			}

			slog.Info("Stack deploy succeeded", "domain", plan.Router.Domain, "host_port", hostPort)
			fmt.Fprintf(buildLog, "[stack] Deploy succeeded. Domain: %s  Port: %s\n", plan.Router.Domain, hostPort)
			reportStatus(types.DeployCallbackStatusSucceeded, "", plan.Router.Domain, hostPort, "")
		}()
	}))

	// Teardown Handler — stop container + remove Caddy route for a project
	mux.HandleFunc("/deploy/", withAgentAuth(agentToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		projectID := strings.TrimPrefix(r.URL.Path, "/deploy/")
		projectID = strings.TrimSpace(projectID)
		if projectID == "" {
			http.Error(w, "Missing project_id", http.StatusBadRequest)
			return
		}

		containerName := fmt.Sprintf("app-%s", projectID)
		ctx := r.Context()

		// Remove container (best-effort)
		if err := containerSvc.Remove(ctx, containerName); err != nil {
			slog.Warn("Teardown: container remove failed (may not exist)", "project_id", projectID, "error", err)
		}
		containerSvc.PruneStoppedContainers(ctx, projectID)

		// Remove Caddy route — we need the domain, so reconstruct it from what caddy knows
		// We pass projectID as domain hint; caller (control-plane) sends the domain via query param
		domain := strings.TrimSpace(r.URL.Query().Get("domain"))
		if domain != "" {
			if err := routerSvc.DeleteRoute(domain); err != nil {
				slog.Warn("Teardown: caddy route delete failed", "domain", domain, "error", err)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "torn_down", "project_id": projectID})
	}))

	// Log Stream Handler
	mux.HandleFunc("/logs", withAgentAuth(agentToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		containerID := r.URL.Query().Get("id")
		projectID := r.URL.Query().Get("project_id")

		if containerID == "" && projectID == "" {
			http.Error(w, "Missing id or project_id", http.StatusBadRequest)
			return
		}

		if containerID == "" && projectID != "" {
			containerID = fmt.Sprintf("app-%s", projectID)
		}

		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		fw := &flushResponseWriter{w: w, flusher: nil}
		if f, ok := w.(http.Flusher); ok {
			fw.flusher = f
		}
		fw.Flush()

		// 1. Stream build log (git clone + docker build output) if available.
		//    StreamTo blocks until the build is done or ctx is cancelled.
		//    If no active build log, fall back to the persisted on-disk log.
		if projectID != "" {
			if bl := logs.DefaultRegistry.Get(projectID); bl != nil {
				if err := bl.StreamTo(r.Context(), fw); err != nil && r.Context().Err() == nil {
					slog.Warn("Build log stream interrupted", "error", err)
				}
			} else if data, err := logs.ReadLogFile(projectID); err == nil && len(data) > 0 {
				_, _ = fw.Write(data)
				fw.Flush()
			}
		}

		// 2. Then follow container logs (runtime output).
		if r.Context().Err() != nil {
			return
		}
		if err := logSvc.Stream(r.Context(), containerID, w); err != nil && r.Context().Err() == nil {
			slog.Error("Container log stream ending", "error", err)
		}
	}))

	mux.HandleFunc("/system/status", withAgentAuth(agentToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, dockerStatusPayload(dockerCli))
	}))

	mux.HandleFunc("/system/docker", withAgentAuth(agentToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}
		action := strings.ToLower(strings.TrimSpace(req.Action))
		if action != "start" && action != "stop" && action != "restart" {
			http.Error(w, "Invalid action", http.StatusBadRequest)
			return
		}

		message, err := controlDocker(action, dockerCli)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		payload := dockerStatusPayload(dockerCli)
		payload["message"] = message
		payload["action"] = action
		writeJSON(w, http.StatusOK, payload)
	}))

	// Per-project container action: start / stop / restart
	mux.HandleFunc("/container/action", withAgentAuth(agentToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ProjectID string `json:"project_id"`
			Action    string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}
		req.ProjectID = strings.TrimSpace(req.ProjectID)
		req.Action = strings.ToLower(strings.TrimSpace(req.Action))

		if req.ProjectID == "" {
			http.Error(w, "project_id is required", http.StatusBadRequest)
			return
		}
		if req.Action != "start" && req.Action != "stop" && req.Action != "restart" {
			http.Error(w, "action must be start, stop, or restart", http.StatusBadRequest)
			return
		}

		if err := containerSvc.ContainerAction(r.Context(), req.ProjectID, req.Action); err != nil {
			slog.Warn("ContainerAction error", "project_id", req.ProjectID, "action", req.Action, "error", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"project_id": req.ProjectID,
			"action":     req.Action,
			"status":     "ok",
		})
	}))

	mux.HandleFunc("/container/status", withAgentAuth(agentToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		projectID := r.URL.Query().Get("project_id")
		if projectID == "" {
			http.Error(w, "Missing project_id", http.StatusBadRequest)
			return
		}

		status, err := containerSvc.GetContainerStatus(r.Context(), projectID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, status)
	}))

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go startHeartbeat(ctx, controlPlaneURL, agentToken)

	slog.Info("Agent listening", "addr", listenAddr)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Agent server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("Agent shutting down, waiting for in-flight deploys...")

	// Wait for in-flight deploys with a 60s timeout
	waitCh := make(chan struct{})
	go func() {
		deployWg.Wait()
		close(waitCh)
	}()

	shutdownTimer := time.NewTimer(60 * time.Second)
	defer shutdownTimer.Stop()

	select {
	case <-waitCh:
		slog.Info("In-flight deploys completed")
	case <-shutdownTimer.C:
		slog.Warn("Shutdown timeout: in-flight deploys may be interrupted")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

func startHeartbeat(ctx context.Context, controlPlaneURL, agentToken string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendHeartbeat(ctx, controlPlaneURL, agentToken)
		}
	}
}

func sendHeartbeat(ctx context.Context, controlPlaneURL, agentToken string) {
	payload := []byte(`{"status":"ok"}`)
	hbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(hbCtx, http.MethodPost,
		strings.TrimRight(controlPlaneURL, "/")+"/internal/agent/heartbeat",
		bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Token", agentToken)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("Heartbeat failed", "error", err)
		return
	}
	resp.Body.Close()
}

func withAgentAuth(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestToken := strings.TrimSpace(r.Header.Get("X-Agent-Token"))
		if !secureEquals(requestToken, token) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	}
}

func secureEquals(left, right string) bool {
	if left == "" || right == "" || len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func sendDeployCallback(parent context.Context, client *http.Client, controlPlaneURL, agentToken string, callback types.DeployCallback) error {
	payload, err := json.Marshal(callback)
	if err != nil {
		return fmt.Errorf("failed to marshal callback: %w", err)
	}

	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(controlPlaneURL, "/")+"/internal/deploy/callback", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to build callback request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Token", agentToken)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("callback request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("callback rejected: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// flushResponseWriter wraps http.ResponseWriter with a Flush() method
// that satisfies the logs.flushWriter interface.
type flushResponseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (fw *flushResponseWriter) Write(p []byte) (int, error) {
	return fw.w.Write(p)
}

func (fw *flushResponseWriter) Flush() {
	if fw.flusher != nil {
		fw.flusher.Flush()
	}
}

func newDockerClient() (*client.Client, error) {
	baseClient, baseErr := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if baseErr == nil {
		if err := pingDocker(baseClient); err == nil {
			return baseClient, nil
		}
		_ = baseClient.Close()
	}

	fallbackHost := detectFallbackDockerHost()
	if fallbackHost == "" {
		if baseErr != nil {
			return nil, baseErr
		}
		return nil, fmt.Errorf("docker daemon is not reachable with current configuration")
	}

	fallbackClient, fallbackErr := client.NewClientWithOpts(client.WithHost(fallbackHost), client.WithAPIVersionNegotiation())
	if fallbackErr != nil {
		if baseErr != nil {
			return nil, fmt.Errorf("default docker client init failed: %v; fallback init failed: %w", baseErr, fallbackErr)
		}
		return nil, fallbackErr
	}

	if err := pingDocker(fallbackClient); err != nil {
		slog.Warn("Docker daemon not reachable during startup", "host", fallbackHost, "error", err)
		// Return the client anyway so the agent can still start.
		// Subsequent Docker calls will return errors, but the agent won't crash.
		return fallbackClient, nil
	}

	slog.Info("Using fallback docker host", "host", fallbackHost)
	return fallbackClient, nil
}

func pingDocker(dockerCli *client.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := dockerCli.Ping(ctx)
	return err
}

func dockerStatusPayload(dockerCli *client.Client) map[string]any {
	payload := map[string]any{
		"running":           false,
		"host":              strings.TrimSpace(dockerCli.DaemonHost()),
		"platform":          runtime.GOOS,
		"control_mode":      detectDockerControlMode(),
		"control_supported": dockerControlSupported(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if version, err := dockerCli.ServerVersion(ctx); err == nil {
		payload["server_version"] = version.Version
	}

	info, err := dockerCli.Info(ctx)
	if err != nil {
		payload["error"] = err.Error()
		return payload
	}

	payload["running"] = true
	payload["engine_os"] = info.OperatingSystem
	payload["server_name"] = info.Name
	return payload
}

func controlDocker(action string, dockerCli *client.Client) (string, error) {
	mode := detectDockerControlMode()
	if mode == "" {
		return "", fmt.Errorf("docker control is not supported on this host")
	}

	switch mode {
	case "colima":
		if _, err := runCommand(60*time.Second, "colima", action); err != nil {
			return "", fmt.Errorf("colima %s failed: %w", action, err)
		}
	case "docker-desktop":
		if err := controlDockerDesktop(action); err != nil {
			return "", err
		}
	case "systemd":
		if _, err := runCommand(60*time.Second, "systemctl", action, "docker"); err != nil {
			return "", fmt.Errorf("systemctl %s docker failed: %w", action, err)
		}
	default:
		return "", fmt.Errorf("unsupported docker control mode: %s", mode)
	}

	switch action {
	case "start", "restart":
		if err := waitForDockerState(dockerCli, true, 60*time.Second); err != nil {
			return fmt.Sprintf("Docker %s requested. Engine may still be starting.", action), nil
		}
		return fmt.Sprintf("Docker %sed successfully.", action), nil
	case "stop":
		if err := waitForDockerState(dockerCli, false, 20*time.Second); err != nil {
			return "Docker stop requested.", nil
		}
		return "Docker stopped successfully.", nil
	default:
		return "Docker action completed.", nil
	}
}

func controlDockerDesktop(action string) error {
	switch action {
	case "start":
		_, err := runCommand(15*time.Second, "open", "-a", "Docker")
		if err != nil {
			return fmt.Errorf("failed to open Docker Desktop: %w", err)
		}
		return nil
	case "stop":
		_, err := runCommand(15*time.Second, "osascript", "-e", `quit app "Docker"`)
		if err != nil {
			return fmt.Errorf("failed to quit Docker Desktop: %w", err)
		}
		return nil
	case "restart":
		if _, err := runCommand(15*time.Second, "osascript", "-e", `quit app "Docker"`); err != nil {
			slog.Warn("Docker Desktop quit step failed during restart", "error", err)
		}
		time.Sleep(2 * time.Second)
		_, err := runCommand(15*time.Second, "open", "-a", "Docker")
		if err != nil {
			return fmt.Errorf("failed to reopen Docker Desktop: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported Docker Desktop action: %s", action)
	}
}

func waitForDockerState(dockerCli *client.Client, wantRunning bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		running := pingDocker(dockerCli) == nil
		if running == wantRunning {
			return nil
		}
		if time.Now().After(deadline) {
			if wantRunning {
				return fmt.Errorf("docker did not become ready within %s", timeout)
			}
			return fmt.Errorf("docker did not stop within %s", timeout)
		}
		time.Sleep(1500 * time.Millisecond)
	}
}

func runCommand(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(output))
		if trimmed == "" {
			return "", err
		}
		return trimmed, fmt.Errorf("%w: %s", err, trimmed)
	}
	return strings.TrimSpace(string(output)), nil
}

func dockerControlSupported() bool {
	return detectDockerControlMode() != ""
}

func detectDockerControlMode() string {
	if looksLikeColimaHost() && commandExists("colima") {
		return "colima"
	}
	switch runtime.GOOS {
	case "darwin":
		if commandExists("open") && commandExists("osascript") {
			return "docker-desktop"
		}
	case "linux":
		if commandExists("systemctl") {
			return "systemd"
		}
	}
	return ""
}

func looksLikeColimaHost() bool {
	host := strings.TrimSpace(os.Getenv("STRANGER_DOCKER_HOST"))
	if host == "" {
		host = strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	}
	if strings.Contains(host, ".colima/") {
		return true
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	colimaSocketPath := filepath.Join(homeDir, ".colima", "default", "docker.sock")
	_, statErr := os.Stat(colimaSocketPath)
	return statErr == nil
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func detectFallbackDockerHost() string {
	if customHost := strings.TrimSpace(os.Getenv("STRANGER_DOCKER_HOST")); customHost != "" {
		return customHost
	}

	if dockerHost := strings.TrimSpace(os.Getenv("DOCKER_HOST")); dockerHost != "" {
		return dockerHost
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	macSocketPath := filepath.Join(homeDir, ".docker", "run", "docker.sock")
	if _, statErr := os.Stat(macSocketPath); statErr == nil {
		return "unix://" + macSocketPath
	}

	colimaSocketPath := filepath.Join(homeDir, ".colima", "default", "docker.sock")
	if _, statErr := os.Stat(colimaSocketPath); statErr == nil {
		return "unix://" + colimaSocketPath
	}

	if _, statErr := os.Stat("/var/run/docker.sock"); statErr == nil {
		return "unix:///var/run/docker.sock"
	}

	return ""
}
