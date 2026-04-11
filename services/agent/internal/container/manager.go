package container

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/stranger/agent/types"
)

type Manager struct {
	docker *client.Client
}

func NewManager(docker *client.Client) *Manager {
	return &Manager{docker: docker}
}

// Deploy stops old container, starts new one
func (m *Manager) Deploy(ctx context.Context, plan types.DeployPlan, imageTag string) error {
	// MVP: Use ProjectID to ensure only one version runs at a time.
	// This simplifies log streaming (just tail matches app-<projectID>)
	containerName := fmt.Sprintf("app-%s", plan.ProjectID)

	// 1. Check/Stop existing
	slog.Info("Checking for existing container", "name", containerName)
	containers, err := m.docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return err
	}

	for _, c := range containers {
		for _, name := range c.Names {
			// Docker names start with /
			if name == "/"+containerName {
				slog.Info("Removing old container", "id", c.ID)
				// Force remove (kills if running)
				err := m.docker.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
				if err != nil {
					return fmt.Errorf("failed to remove old container: %w", err)
				}
				break
			}
		}
	}

	// 2. Create New
	slog.Info("Creating new container", "image", imageTag)

	containerPort := nat.Port(fmt.Sprintf("%d/tcp", plan.Runtime.Port))

	config := &container.Config{
		Image: imageTag,
		Env:   flattenEnv(plan.Runtime.Env),
		ExposedPorts: nat.PortSet{
			containerPort: struct{}{},
		},
		Labels: map[string]string{
			"com.stranger.project": plan.ProjectID,
			"com.stranger.plan":    plan.ID,
		},
	}

	hostConfig := &container.HostConfig{
		PortBindings: nat.PortMap{
			containerPort: []nat.PortBinding{
				{
					HostIP:   "0.0.0.0",
					HostPort: "0", // Random port
				},
			},
		},
		RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
	}

	if mem, err := parseMemoryBytes(plan.Runtime.MemoryLimit); err == nil && mem > 0 {
		hostConfig.Memory = mem
	}
	if quota, err := parseCPUQuota(plan.Runtime.CPULimit); err == nil && quota > 0 {
		hostConfig.CPUQuota = quota
	}

	resp, err := m.docker.ContainerCreate(ctx, config, hostConfig, nil, nil, containerName)
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	// 3. Start
	if err := m.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}

	slog.Info("Container started", "id", resp.ID)
	return nil
}

// ─── Stack / Pipeline deploy ────────────────────────────────────────────────

// Builder is the minimal interface the manager needs for stack image pulls.
type Builder interface {
	PullImage(ctx context.Context, imageRef, planTag string, out io.Writer) error
}

// DeployStack deploys a multi-service stack in dependency order on a shared Docker network.
// Returns the host port of the first non-internal service.
func (m *Manager) DeployStack(ctx context.Context, plan types.StackDeployPlan, out io.Writer, b Builder) (string, error) {
	// 1. Ensure network exists
	if err := m.ensureNetwork(ctx, plan.Network); err != nil {
		return "", fmt.Errorf("network creation failed: %w", err)
	}
	fmt.Fprintf(out, "[stack] Network ready: %s\n", plan.Network)

	// 2. Deploy services in order (catalog already sorted by depends_on)
	var publicService *types.StackService
	for i := range plan.Services {
		svc := &plan.Services[i]
		fmt.Fprintf(out, "[stack] Deploying service: %s (%s)\n", svc.Key, svc.DockerImage)

		// Pull image
		pullTag := fmt.Sprintf("stranger-stack-%s-%s:latest", plan.ProjectID, svc.Key)
		if err := b.PullImage(ctx, svc.DockerImage, pullTag, out); err != nil {
			return "", fmt.Errorf("service %s: image pull failed: %w", svc.Key, err)
		}

		// Ensure named volume if requested
		if svc.Volume != "" {
			if err := m.ensureVolume(ctx, svc.Volume); err != nil {
				return "", fmt.Errorf("service %s: volume creation failed: %w", svc.Key, err)
			}
		}

		// Remove existing container with same name
		_ = m.docker.ContainerRemove(ctx, svc.ContainerName, container.RemoveOptions{Force: true})

		// Port binding: only for non-internal (the public app service)
		exposedPorts := nat.PortSet{}
		portBindings := nat.PortMap{}
		if !svc.Internal {
			cp := nat.Port(fmt.Sprintf("%d/tcp", svc.Port))
			exposedPorts[cp] = struct{}{}
			portBindings[cp] = []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "0"}}
		}

		containerCfg := &container.Config{
			Image:        pullTag,
			Env:          flattenEnv(svc.Env),
			ExposedPorts: exposedPorts,
			Labels: map[string]string{
				"com.stranger.project": plan.ProjectID,
				"com.stranger.stack":   plan.ID,
				"com.stranger.service": svc.Key,
			},
		}

		binds := []string{}
		if svc.Volume != "" {
			binds = append(binds, fmt.Sprintf("%s:/var/lib/data", svc.Volume))
		}

		hostCfg := &container.HostConfig{
			PortBindings:  portBindings,
			RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
			Binds:         binds,
		}

		netCfg := &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				plan.Network: {NetworkID: plan.Network},
			},
		}

		resp, err := m.docker.ContainerCreate(ctx, containerCfg, hostCfg, netCfg, nil, svc.ContainerName)
		if err != nil {
			return "", fmt.Errorf("service %s: container create failed: %w", svc.Key, err)
		}

		if err := m.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
			return "", fmt.Errorf("service %s: container start failed: %w", svc.Key, err)
		}

		// Health check: internal services get more time (DB startup), public ones less
		healthTimeout := 60 * time.Second
		if !svc.Internal {
			healthTimeout = 90 * time.Second
		}

		if svc.Internal {
			// For internal services (DB), we can't reach them directly from host
			// Just wait a flat period for the DB to be ready
			fmt.Fprintf(out, "[stack] Waiting for %s to be ready...\n", svc.Key)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(10 * time.Second):
			}
		} else {
			hostPort, err := m.GetHostPort(ctx, svc.ContainerName, svc.Port)
			if err != nil {
				return "", fmt.Errorf("service %s: port resolution failed: %w", svc.Key, err)
			}
			if err := m.WaitReady(ctx, hostPort, healthTimeout); err != nil {
				return "", fmt.Errorf("service %s: health check timed out: %w", svc.Key, err)
			}
			if publicService == nil {
				publicService = svc
			}
		}

		slog.Info("Stack service deployed", "service", svc.Key, "container", svc.ContainerName)
		fmt.Fprintf(out, "[stack] Service ready: %s\n", svc.Key)
	}

	if publicService == nil {
		return "", fmt.Errorf("no public (non-internal) service found in stack")
	}

	hostPort, err := m.GetHostPort(ctx, publicService.ContainerName, publicService.Port)
	if err != nil {
		return "", fmt.Errorf("public service port resolution failed: %w", err)
	}
	return hostPort, nil
}

func (m *Manager) ensureNetwork(ctx context.Context, networkName string) error {
	// Check if already exists
	res, err := m.docker.NetworkList(ctx, dockertypes.NetworkListOptions{
		Filters: filters.NewArgs(filters.Arg("name", networkName)),
	})
	if err != nil {
		return err
	}
	for _, n := range res {
		if n.Name == networkName {
			return nil // already exists
		}
	}
	_, err = m.docker.NetworkCreate(ctx, networkName, dockertypes.NetworkCreate{
		Driver: "bridge",
		Labels: map[string]string{"com.stranger.managed": "true"},
	})
	return err
}

func (m *Manager) ensureVolume(ctx context.Context, volumeName string) error {
	_, err := m.docker.VolumeCreate(ctx, volume.CreateOptions{
		Name:   volumeName,
		Driver: "local",
		Labels: map[string]string{"com.stranger.managed": "true"},
	})
	return err
}

// GetHostPort finds the actual port mapped on the host
func (m *Manager) GetHostPort(ctx context.Context, containerName string, internalPort int) (string, error) {
	json, err := m.docker.ContainerInspect(ctx, containerName)
	if err != nil {
		return "", err
	}

	portProto := nat.Port(fmt.Sprintf("%d/tcp", internalPort))
	bindings, ok := json.NetworkSettings.Ports[portProto]
	if !ok || len(bindings) == 0 {
		return "", fmt.Errorf("no port binding found for %d", internalPort)
	}

	return bindings[0].HostPort, nil
}

// WaitReady polls localhost:{hostPort} via TCP until the app is accepting connections
// or timeout is reached.
func (m *Manager) WaitReady(ctx context.Context, hostPort string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort("localhost", hostPort)
	var lastErr error
	for {
		conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("health check timed out after %s: %w", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Remove force-removes a container by name.
func (m *Manager) Remove(ctx context.Context, containerName string) error {
	slog.Info("Removing container", "name", containerName)
	if err := m.docker.ContainerRemove(ctx, containerName, container.RemoveOptions{Force: true}); err != nil {
		slog.Error("Failed to remove container", "name", containerName, "error", err)
		return fmt.Errorf("failed to remove container %s: %w", containerName, err)
	}
	return nil
}

// PruneProjectImages removes old Docker images for a project, keeping the most recent keepCount.
func (m *Manager) PruneProjectImages(ctx context.Context, projectID string, keepCount int) error {
	filterArgs := filters.NewArgs()
	filterArgs.Add("label", "com.stranger.project="+projectID)

	images, err := m.docker.ImageList(ctx, image.ListOptions{Filters: filterArgs})
	if err != nil {
		return fmt.Errorf("failed to list images: %w", err)
	}

	if len(images) <= keepCount {
		return nil
	}

	// Sort by created time descending (newest first)
	sort.Slice(images, func(i, j int) bool {
		return images[i].Created > images[j].Created
	})

	for _, img := range images[keepCount:] {
		if _, err := m.docker.ImageRemove(ctx, img.ID, image.RemoveOptions{Force: true}); err != nil {
			slog.Warn("Failed to remove old image", "id", img.ID, "tags", img.RepoTags, "error", err)
		} else {
			slog.Info("Pruned old image", "id", img.ID, "tags", img.RepoTags)
		}
	}
	return nil
}

// PruneStoppedContainers removes any exited/dead containers for a project.
func (m *Manager) PruneStoppedContainers(ctx context.Context, projectID string) {
	filterArgs := filters.NewArgs()
	filterArgs.Add("label", "com.stranger.project="+projectID)
	filterArgs.Add("status", "exited")

	containers, err := m.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: filterArgs})
	if err != nil {
		slog.Warn("PruneStoppedContainers: list failed", "project_id", projectID, "error", err)
		return
	}
	for _, c := range containers {
		if err := m.docker.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
			slog.Warn("PruneStoppedContainers: remove failed", "id", c.ID, "error", err)
		} else {
			slog.Info("PruneStoppedContainers: removed", "id", c.ID)
		}
	}
}

// ContainerAction performs start, stop, or restart on the container for a project.
// projectID is the Stranger project ID; the container name is derived as "app-<projectID>".
// If the project has a stack, all containers matching the project label are acted upon.
func (m *Manager) ContainerAction(ctx context.Context, projectID, action string) error {
	containerName := fmt.Sprintf("app-%s", projectID)

	// List all containers (including stopped) to find matching ones.
	all, err := m.docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	var targets []string
	for _, c := range all {
		for _, n := range c.Names {
			// Single-container projects: exact name match ("app-<projectID>")
			// Stack projects: names like "app-<projectID>-app", "app-<projectID>-db"
			clean := strings.TrimPrefix(n, "/")
			if clean == containerName || strings.HasPrefix(clean, containerName+"-") {
				targets = append(targets, c.ID)
				break
			}
		}
	}

	if len(targets) == 0 {
		return fmt.Errorf("no container found for project %q — has it been deployed yet?", projectID)
	}

	timeout := 10 // seconds for stop/restart
	var lastErr error
	for _, id := range targets {
		var opErr error
		switch action {
		case "start":
			opErr = m.docker.ContainerStart(ctx, id, container.StartOptions{})
		case "stop":
			opErr = m.docker.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout})
		case "restart":
			opErr = m.docker.ContainerRestart(ctx, id, container.StopOptions{Timeout: &timeout})
		default:
			return fmt.Errorf("unknown action %q — must be start, stop, or restart", action)
		}
		if opErr != nil {
			slog.Warn("ContainerAction failed", "action", action, "id", id, "error", opErr)
			lastErr = opErr
		} else {
			slog.Info("ContainerAction succeeded", "action", action, "id", id, "project_id", projectID)
		}
	}
	return lastErr
}

// GetContainerStatus returns rich details about all containers for a project.
func (m *Manager) GetContainerStatus(ctx context.Context, projectID string) (map[string]any, error) {
	containerName := fmt.Sprintf("app-%s", projectID)
	all, err := m.docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}

	var matchedIDs []string
	for _, c := range all {
		for _, n := range c.Names {
			clean := strings.TrimPrefix(n, "/")
			if clean == containerName || strings.HasPrefix(clean, containerName+"-") {
				matchedIDs = append(matchedIDs, c.ID)
				break
			}
		}
	}

	if len(matchedIDs) == 0 {
		return map[string]any{
			"project_id": projectID,
			"running":    false,
			"containers": []any{},
			"summary":    "not_deployed",
		}, nil
	}

	var results []map[string]any
	runningCount := 0

	for _, id := range matchedIDs {
		info, err := m.docker.ContainerInspect(ctx, id)
		if err != nil {
			continue
		}

		// Collect exposed + bound ports
		var ports []map[string]any
		for portProto, bindings := range info.NetworkSettings.Ports {
			for _, b := range bindings {
				ports = append(ports, map[string]any{
					"container": string(portProto),
					"host_ip":   b.HostIP,
					"host_port": b.HostPort,
				})
			}
		}

		// Friendly name (trim leading /)
		name := strings.TrimPrefix(info.Name, "/")

		state := info.State
		if state != nil && strings.EqualFold(state.Status, "running") {
			runningCount++
		}

		result := map[string]any{
			"id":            info.ID[:12],
			"id_full":       info.ID,
			"name":          name,
			"image":         info.Config.Image,
			"created":       info.Created,
			"state":         "",
			"status_text":   "",
			"started_at":    "",
			"finished_at":   "",
			"restart_count": 0,
			"exit_code":     0,
			"ports":         ports,
			"labels":        info.Config.Labels,
		}

		if state != nil {
			result["state"] = state.Status
			result["started_at"] = state.StartedAt
			result["finished_at"] = state.FinishedAt
			result["restart_count"] = info.RestartCount
			result["exit_code"] = state.ExitCode
			result["oom_killed"] = state.OOMKilled
			result["paused"] = state.Paused
			result["error"] = state.Error

			// Build human-readable status_text
			switch strings.ToLower(state.Status) {
			case "running":
				result["status_text"] = "Running"
			case "exited":
				result["status_text"] = fmt.Sprintf("Exited (code %d)", state.ExitCode)
			case "paused":
				result["status_text"] = "Paused"
			case "restarting":
				result["status_text"] = "Restarting"
			default:
				result["status_text"] = state.Status
			}
		}

		results = append(results, result)
	}

	summary := "exited"
	if runningCount == len(results) {
		summary = "running"
	} else if runningCount > 0 {
		summary = "partial"
	}

	return map[string]any{
		"project_id": projectID,
		"running":    runningCount > 0,
		"summary":    summary,
		"containers": results,
	}, nil
}

func flattenEnv(env map[string]string) []string {
	var out []string
	for k, v := range env {
		out = append(out, fmt.Sprintf("%s=%s", k, v))
	}
	return out
}

// parseMemoryBytes converts a human-readable size string to bytes.
// Examples: "512m" → 536870912, "1g" → 1073741824, "256" → 256.
func parseMemoryBytes(limit string) (int64, error) {
	limit = strings.TrimSpace(strings.ToLower(limit))
	if limit == "" {
		return 0, nil
	}
	multipliers := map[string]int64{
		"k": 1024,
		"m": 1024 * 1024,
		"g": 1024 * 1024 * 1024,
	}
	for suffix, mult := range multipliers {
		if strings.HasSuffix(limit, suffix) {
			n, err := strconv.ParseInt(strings.TrimSuffix(limit, suffix), 10, 64)
			if err != nil {
				return 0, err
			}
			return n * mult, nil
		}
	}
	return strconv.ParseInt(limit, 10, 64)
}

// parseCPUQuota converts a float CPU fraction string to Docker CPUQuota microseconds.
// CPUPeriod defaults to 100000 us. "0.5" → 50000, "1" → 100000, "2" → 200000.
func parseCPUQuota(limit string) (int64, error) {
	limit = strings.TrimSpace(limit)
	if limit == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(limit, 64)
	if err != nil {
		return 0, err
	}
	return int64(f * 100000), nil
}
