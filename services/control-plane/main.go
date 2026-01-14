package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/stranger/control-plane/internal/store"
	"github.com/stranger/control-plane/types"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// Init DB
	db, err := store.NewStore("stranger.db")
	if err != nil {
		slog.Error("Failed to init db", "error", err)
		os.Exit(1)
	}

	// Handlers
	http.HandleFunc("/projects", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			projects, err := db.ListProjects()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(projects)
			return
		}

		if r.Method == http.MethodPost {
			var p types.Project
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				http.Error(w, "Invalid body", http.StatusBadRequest)
				return
			}
			p.ID = uuid.New().String()
			p.CreatedAt = time.Now()

			if err := db.CreateProject(p); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(p)
			return
		}
	})

	http.HandleFunc("/deploy", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		
		// For MVP, simply forward a fixed plan to the Agent
		// In real system: Fetch project, generate plan, find node, send to agent.
		
		agentURL := "http://localhost:3000/deploy"
		
		// Mock Plan
		plan := map[string]interface{}{
			"id": "test-" + uuid.New().String()[:8],
			"template": "nextjs",
			"build": map[string]string{
				"repo_url": "https://github.com/vercel/next.js/tree/canary/examples/hello-world", // Just an example
				"dockerfile": "./Dockerfile",
			},
			"runtime": map[string]interface{}{
				"port": 3000,
				"env": map[string]string{"NODE_ENV": "production"},
			},
			"router": map[string]string{
				"domain": "hello.local",
			},
		}
		
		body, _ := json.Marshal(plan)
		resp, err := http.Post(agentURL, "application/json", bytes.NewBuffer(body))
		if err != nil {
			http.Error(w, "Agent unreachable: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "deploy_dispatched"})
	})

	slog.Info("Control Plane starting on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		slog.Error("Server failed", "error", err)
		os.Exit(1)
	}
}
