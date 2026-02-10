package builder

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	dockertypes "github.com/docker/docker/api/types"
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
	buildContextDir, err := resolveBuildContextDir(workDir, plan.Build.BaseDirectory)
	if err != nil {
		return "", err
	}

	tar, err := archive.TarWithOptions(buildContextDir, &archive.TarOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to tar build context: %w", err)
	}

	// 3. Docker Build
	imageTag := fmt.Sprintf("stranger-app-%s:latest", plan.ID)
	dockerfilePath, err := resolveDockerfilePath(buildContextDir, plan.Build.Dockerfile)
	if err != nil {
		return "", err
	}
	slog.Info("Building docker image", "tag", imageTag, "context", buildContextDir, "dockerfile", dockerfilePath)

	res, err := b.docker.ImageBuild(ctx, tar, dockertypes.ImageBuildOptions{
		Tags:       []string{imageTag},
		Dockerfile: dockerfilePath,
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

func resolveBuildContextDir(workDir, requested string) (string, error) {
	baseDirectory := strings.TrimSpace(requested)
	if baseDirectory == "" {
		baseDirectory = "."
	}

	cleanPath := filepath.Clean(strings.TrimPrefix(baseDirectory, "./"))
	if cleanPath == "" || cleanPath == string(filepath.Separator) {
		cleanPath = "."
	}
	if filepath.IsAbs(cleanPath) {
		return "", fmt.Errorf("base_directory must be relative: %q", requested)
	}
	if cleanPath == ".." || strings.HasPrefix(cleanPath, fmt.Sprintf("..%c", filepath.Separator)) {
		return "", fmt.Errorf("base_directory cannot escape repository root: %q", requested)
	}

	contextPath := filepath.Join(workDir, cleanPath)
	stat, err := os.Stat(contextPath)
	if err != nil {
		return "", fmt.Errorf("base directory not found in repository at %q", cleanPath)
	}
	if !stat.IsDir() {
		return "", fmt.Errorf("base directory is not a directory: %q", cleanPath)
	}

	return contextPath, nil
}

func resolveDockerfilePath(contextDir, requested string) (string, error) {
	dockerfile := strings.TrimSpace(requested)
	userProvidedDockerfile := dockerfile != "" && dockerfile != "Dockerfile" && dockerfile != "./Dockerfile"
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}

	cleanPath := filepath.Clean(strings.TrimPrefix(dockerfile, "./"))
	if cleanPath == "." || cleanPath == string(filepath.Separator) || cleanPath == "" {
		cleanPath = "Dockerfile"
	}
	if filepath.IsAbs(cleanPath) {
		return "", fmt.Errorf("dockerfile path must be relative: %q", requested)
	}
	if cleanPath == ".." || strings.HasPrefix(cleanPath, fmt.Sprintf("..%c", filepath.Separator)) {
		return "", fmt.Errorf("dockerfile path cannot escape build context: %q", requested)
	}

	absolutePath := filepath.Join(contextDir, cleanPath)
	if _, err := os.Stat(absolutePath); err != nil {
		if userProvidedDockerfile {
			return "", fmt.Errorf("dockerfile not found in repository at %q", cleanPath)
		}

		dockerfiles, discoverErr := discoverDockerfiles(contextDir)
		if discoverErr != nil {
			return "", discoverErr
		}
		if len(dockerfiles) == 0 {
			return "", fmt.Errorf("dockerfile not found in repository at %q", cleanPath)
		}
		if len(dockerfiles) == 1 {
			return dockerfiles[0], nil
		}
		if len(dockerfiles) > 5 {
			dockerfiles = dockerfiles[:5]
		}
		return "", fmt.Errorf(
			"multiple Dockerfiles found (%s); set dockerfile_location explicitly",
			strings.Join(dockerfiles, ", "),
		)
	}

	return filepath.ToSlash(cleanPath), nil
}

func discoverDockerfiles(contextDir string) ([]string, error) {
	dockerfiles := make([]string, 0, 4)

	err := filepath.WalkDir(contextDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := strings.ToLower(entry.Name())
			if name == ".git" || name == "node_modules" || strings.HasPrefix(name, ".") {
				if path == contextDir {
					return nil
				}
				return filepath.SkipDir
			}
			return nil
		}

		if strings.EqualFold(entry.Name(), "Dockerfile") {
			relative, err := filepath.Rel(contextDir, path)
			if err != nil {
				return err
			}
			dockerfiles = append(dockerfiles, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to discover Dockerfile locations: %w", err)
	}

	return dockerfiles, nil
}
