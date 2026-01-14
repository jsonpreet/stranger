package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/docker/docker/client"
	"github.com/stranger/agent/internal/builder"
	"github.com/stranger/agent/internal/container"
	"github.com/stranger/agent/internal/router"
	"github.com/stranger/agent/types"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// Init Docker
	dockerCli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		slog.Error("Failed to create docker client", "error", err)
		os.Exit(1)
	}

	// Init Services
	buildSvc := builder.NewBuilder(dockerCli)
	containerSvc := container.NewManager(dockerCli)
	routerSvc := router.NewCaddyRouter()

	// Deploy Handler
	http.HandleFunc("/deploy", func(w http.ResponseWriter, r *http.Request) {
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

		go func() {
			ctx := context.Background()

			// 1. Build
			imageTag, err := buildSvc.Build(ctx, plan)
			if err != nil {
				slog.Error("Build failed", "error", err)
				return
			}

			// 2. Run
			if err := containerSvc.Deploy(ctx, plan, imageTag); err != nil {
				slog.Error("Deploy failed", "error", err)
				return
			}

			// 3. Find Port
			hostPort, err := containerSvc.GetHostPort(ctx, fmt.Sprintf("app-%s", plan.ID), plan.Runtime.Port)
			if err != nil {
				slog.Error("Failed to resolve port", "error", err)
				return
			}

			// 4. Update Router
			if err := routerSvc.UpdateRoute(plan.Router.Domain, hostPort); err != nil {
				slog.Error("Router update failed", "error", err)
				return
			}

			slog.Info("Deploy Success!", "domain", plan.Router.Domain, "host_port", hostPort)
		}()

		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"status": "deploy_started", "id": plan.ID})
	})

	slog.Info("Agent listening on :3000")
	http.ListenAndServe(":3000", nil)
}
