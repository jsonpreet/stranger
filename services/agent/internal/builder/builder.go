package builder

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/archive"
	"github.com/go-git/go-git/v5"
	"github.com/stranger/agent/types"
)

type Builder struct {
	docker *client.Client
}

func NewBuilder(docker *client.Client) *Builder {
	return &Builder{docker: docker}
}

// Build handles cloning and building the docker image
// Returns the image ID (tag)
func (b *Builder) Build(ctx context.Context, plan types.DeployPlan) (string, error) {
	workDir, err := os.MkdirTemp("", "build-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	slog.Info("Cloning repo", "url", plan.Build.RepoURL, "dir", workDir)
	
	// 1. Clone
	_, err = git.PlainClone(workDir, false, &git.CloneOptions{
		URL:      plan.Build.RepoURL,
		Progress: os.Stdout,
		Depth:    1,
	})
	if err != nil {
		return "", fmt.Errorf("git clone failed: %w", err)
	}

	// 2. Tar context
	tar, err := archive.TarWithOptions(workDir, &archive.TarOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to tar build context: %w", err)
	}

	// 3. Docker Build
	imageTag := fmt.Sprintf("stranger-app-%s:latest", plan.ID)
	slog.Info("Building docker image", "tag", imageTag)

	res, err := b.docker.ImageBuild(ctx, tar, types.ImageBuildOptions{
		Tags:       []string{imageTag},
		Dockerfile: plan.Build.Dockerfile,
		Remove:     true,
	})
	if err != nil {
		return "", fmt.Errorf("docker build failed: %w", err)
	}
	defer res.Body.Close()

	// Stream output to logs
	_, err = io.Copy(os.Stdout, res.Body)
	if err != nil {
		slog.Error("error reading build output", "error", err)
	}

	return imageTag, nil
}
