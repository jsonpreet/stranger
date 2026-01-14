package container

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
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
	containerName := fmt.Sprintf("app-%s", plan.ID)

	// 1. Check/Stop existing
	slog.Info("Checking for existing container", "name", containerName)
	containers, err := m.docker.ContainerList(ctx, types.ContainerListOptions{All: true})
	if err != nil {
		return err
	}

	for _, c := range containers {
		for _, name := range c.Names {
			// Docker names start with /
			if name == "/"+containerName {
				slog.Info("Removing old container", "id", c.ID)
				// Force remove (kills if running)
				err := m.docker.ContainerRemove(ctx, c.ID, types.ContainerRemoveOptions{Force: true})
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

	resp, err := m.docker.ContainerCreate(ctx, config, hostConfig, nil, nil, containerName)
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	// 3. Start
	if err := m.docker.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}

	slog.Info("Container started", "id", resp.ID)
	return nil
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

func flattenEnv(env map[string]string) []string {
	var out []string
	for k, v := range env {
		out = append(out, fmt.Sprintf("%s=%s", k, v))
	}
	return out
}
