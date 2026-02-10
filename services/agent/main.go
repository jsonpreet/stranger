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
	"path/filepath"
	"strings"
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
		slog.Error("Failed to create docker client", "error", err)
		os.Exit(1)
	}

	// Init Services
	buildSvc := builder.NewBuilder(dockerCli)
	containerSvc := container.NewManager(dockerCli)
	routerSvc := router.NewCaddyRouter()
	logSvc := logs.NewStreamer(dockerCli)
	callbackClient := &http.Client{
		Timeout: 10 * time.Second,
	}

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

		go func() {
			ctx := context.Background()
			reportStatus := func(status, errorMessage, domain, hostPort string) {
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
				}
				if err := sendDeployCallback(ctx, callbackClient, controlPlaneURL, agentToken, callback); err != nil {
					slog.Error("Failed to send deploy callback", "job_id", plan.JobID, "status", status, "error", err)
				}
			}

			reportStatus(types.DeployCallbackStatusRunning, "", "", "")

			// 1. Build
			imageTag, err := buildSvc.Build(ctx, plan)
			if err != nil {
				slog.Error("Build failed", "error", err)
				reportStatus(types.DeployCallbackStatusFailed, fmt.Sprintf("build failed: %v", err), "", "")
				return
			}

			// 2. Run
			if err := containerSvc.Deploy(ctx, plan, imageTag); err != nil {
				slog.Error("Deploy failed", "error", err)
				reportStatus(types.DeployCallbackStatusFailed, fmt.Sprintf("deploy failed: %v", err), "", "")
				return
			}

			// 3. Find Port
			hostPort, err := containerSvc.GetHostPort(ctx, fmt.Sprintf("app-%s", plan.ProjectID), plan.Runtime.Port)
			if err != nil {
				slog.Error("Failed to resolve port", "error", err)
				reportStatus(types.DeployCallbackStatusFailed, fmt.Sprintf("port resolution failed: %v", err), "", "")
				return
			}

			// 4. Update Router
			if err := routerSvc.UpdateRoute(plan.Router.Domain, hostPort); err != nil {
				slog.Error("Router update failed", "error", err)
				reportStatus(types.DeployCallbackStatusFailed, fmt.Sprintf("router update failed: %v", err), plan.Router.Domain, hostPort)
				return
			}

			slog.Info("Deploy Success!", "domain", plan.Router.Domain, "host_port", hostPort)
			reportStatus(types.DeployCallbackStatusSucceeded, "", plan.Router.Domain, hostPort)
		}()
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
			// Resolve project_id to container name
			containerID = fmt.Sprintf("app-%s", projectID)
		}

		// Set headers for streaming
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		if err := logSvc.Stream(r.Context(), containerID, w); err != nil {
			slog.Error("Log stream ending", "error", err)
		}
	}))

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	slog.Info("Agent listening", "addr", listenAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("Agent server failed", "error", err)
		os.Exit(1)
	}
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
		_ = fallbackClient.Close()
		if baseErr != nil {
			return nil, fmt.Errorf("default docker client init failed: %v; fallback ping failed: %w", baseErr, err)
		}
		return nil, fmt.Errorf("fallback docker ping failed: %w", err)
	}

	slog.Info("Using fallback docker host", "host", fallbackHost)
	return fallbackClient, nil
}

func pingDocker(dockerCli *client.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := dockerCli.Ping(ctx)
	return err
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
