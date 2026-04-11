package builder

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/image"
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

// Build handles cloning and building the docker image.
// out receives human-readable progress lines (git clone + docker build).
// Returns the image tag that was built or resolved.
func (b *Builder) Build(ctx context.Context, plan types.DeployPlan, out io.Writer) (string, error) {
	if out == nil {
		out = os.Stdout
	}
	tee := io.MultiWriter(os.Stdout, out)
	planImageTag := fmt.Sprintf("stranger-app-%s:latest", plan.ID)

	// 1. Prebuilt image (rollback): skip build entirely
	if plan.Build.PrebuiltImageTag != "" {
		fmt.Fprintf(tee, "[build] Using prebuilt image: %s\n", plan.Build.PrebuiltImageTag)
		_, _, err := b.docker.ImageInspectWithRaw(ctx, plan.Build.PrebuiltImageTag)
		if err != nil {
			return "", fmt.Errorf("rollback image not available: %s not found locally; try a fresh deploy", plan.Build.PrebuiltImageTag)
		}
		if err := b.docker.ImageTag(ctx, plan.Build.PrebuiltImageTag, planImageTag); err != nil {
			return "", fmt.Errorf("failed to tag prebuilt image as %s: %w", planImageTag, err)
		}
		fmt.Fprintf(tee, "[build] Prebuilt image ready: %s\n", planImageTag)
		return planImageTag, nil
	}

	// 2. Docker Hub image: pull and tag, no build needed
	if plan.Build.DockerImage != "" {
		if err := b.pullDockerImage(ctx, plan.Build.DockerImage, planImageTag, tee); err != nil {
			return "", err
		}
		return planImageTag, nil
	}

	// 2. Cache hit check
	if plan.Build.CacheKey != "" {
		cacheTag := fmt.Sprintf("stranger-cache-%s:latest", plan.Build.CacheKey)
		_, _, err := b.docker.ImageInspectWithRaw(ctx, cacheTag)
		if err == nil {
			fmt.Fprintf(tee, "[build] Cache hit: %s\n", cacheTag)
			if tagErr := b.docker.ImageTag(ctx, cacheTag, planImageTag); tagErr != nil {
				fmt.Fprintf(tee, "[build] Cache tag failed, proceeding with fresh build: %v\n", tagErr)
			} else {
				fmt.Fprintf(tee, "[build] Tagged cache image as %s\n", planImageTag)
				return planImageTag, nil
			}
		}
	}

	// 3. Clone repository
	workDir, err := os.MkdirTemp("", "build-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	fmt.Fprintf(tee, "[git] Cloning %s ...\n", plan.Build.RepoURL)
	slog.Info("Cloning repo", "url", plan.Build.RepoURL, "dir", workDir)

	_, err = git.PlainClone(workDir, false, &git.CloneOptions{
		URL:      plan.Build.RepoURL,
		Progress: tee,
		Depth:    1,
	})
	if err != nil {
		return "", fmt.Errorf("git clone failed: %w", err)
	}
	fmt.Fprintf(tee, "[git] Clone complete\n")

	buildContextDir, err := resolveBuildContextDir(workDir, plan.Build.BaseDirectory)
	if err != nil {
		return "", err
	}

	// 4. Build — route to nixpacks or standard docker build
	if plan.Build.BuildMode == "nixpacks" {
		if err := b.buildWithNixpacks(ctx, buildContextDir, planImageTag, plan, tee); err != nil {
			return "", err
		}
	} else {
		if err := generateDockerfileIfMissing(buildContextDir, plan.Build.Dockerfile, plan.Template, tee); err != nil {
			return "", err
		}

		dockerfilePath, err := resolveDockerfilePath(buildContextDir, plan.Build.Dockerfile)
		if err != nil {
			return "", err
		}

		if err := patchNodeAlpineDockerfileForARM64(buildContextDir, dockerfilePath, tee); err != nil {
			return "", err
		}
		if err := ensureNextStandaloneOutput(buildContextDir, dockerfilePath, plan.Template, tee); err != nil {
			return "", err
		}

		tar, err := archive.TarWithOptions(buildContextDir, &archive.TarOptions{})
		if err != nil {
			return "", fmt.Errorf("failed to tar build context: %w", err)
		}

		fmt.Fprintf(tee, "[docker] Building image %s (context: %s, dockerfile: %s) ...\n",
			planImageTag, buildContextDir, dockerfilePath)
		slog.Info("Building docker image", "tag", planImageTag, "context", buildContextDir, "dockerfile", dockerfilePath)

		res, err := b.docker.ImageBuild(ctx, tar, dockertypes.ImageBuildOptions{
			Tags:       []string{planImageTag},
			Dockerfile: dockerfilePath,
			Remove:     true,
			Labels: map[string]string{
				"com.stranger.project": plan.ProjectID,
			},
		})
		if err != nil {
			return "", fmt.Errorf("docker build failed: %w", err)
		}
		defer res.Body.Close()

		// Parse the JSON build stream and surface human-readable output.
		// ImageBuild returns 200 OK even on failure — errors are in the stream body.
		if err := processBuildResponse(res.Body, tee); err != nil {
			return "", fmt.Errorf("docker build failed: %w", err)
		}

		fmt.Fprintf(tee, "[docker] Image built successfully: %s\n", planImageTag)
	}

	// 5. Tag with cache key so future deploys can skip the build
	if plan.Build.CacheKey != "" {
		cacheTag := fmt.Sprintf("stranger-cache-%s:latest", plan.Build.CacheKey)
		if tagErr := b.docker.ImageTag(ctx, planImageTag, cacheTag); tagErr != nil {
			slog.Warn("Failed to tag image with cache key", "cache_tag", cacheTag, "error", tagErr)
		} else {
			fmt.Fprintf(tee, "[build] Cached as %s\n", cacheTag)
		}
	}

	return planImageTag, nil
}

// buildWithNixpacks uses the nixpacks CLI to auto-detect the language/framework
// and produce a Docker image without a pre-existing Dockerfile.
// Nixpacks must be installed globally on the agent host (dev-start.sh handles this).
func (b *Builder) buildWithNixpacks(ctx context.Context, contextDir, imageTag string, plan types.DeployPlan, out io.Writer) error {
	if _, err := exec.LookPath("nixpacks"); err != nil {
		return fmt.Errorf("nixpacks not installed on this agent. Run ./dev-start.sh restart to install it, or see https://nixpacks.com/docs/install")
	}

	fmt.Fprintf(out, "[nixpacks] Building %s from %s ...\n", imageTag, contextDir)
	slog.Info("nixpacks build", "tag", imageTag, "context", contextDir, "template", plan.Template)

	cmd := exec.CommandContext(ctx, "nixpacks", "build", contextDir,
		"--name", imageTag,
	)
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nixpacks build failed: %w", err)
	}

	fmt.Fprintf(out, "[nixpacks] Image built successfully: %s\n", imageTag)
	return nil
}

// PullImage is a public wrapper around pullDockerImage used by the stack deployer.
func (b *Builder) PullImage(ctx context.Context, imageRef, planTag string, out io.Writer) error {
	return b.pullDockerImage(ctx, imageRef, planTag, out)
}

// pullDockerImage pulls imageRef from Docker Hub (or any registry) and tags it as planTag.
// Pull progress is streamed to out.
func (b *Builder) pullDockerImage(ctx context.Context, imageRef, planTag string, out io.Writer) error {
	fmt.Fprintf(out, "[docker] Pulling %s ...\n", imageRef)
	slog.Info("Pulling Docker Hub image", "image", imageRef, "tag", planTag)

	rc, err := b.docker.ImagePull(ctx, imageRef, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("docker pull failed for %s: %w", imageRef, err)
	}
	defer rc.Close()

	// Stream pull status lines (JSON per line)
	scanner := bufio.NewScanner(rc)
	for scanner.Scan() {
		line := scanner.Text()
		var msg struct {
			Status string `json:"status"`
			ID     string `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err == nil && msg.Status != "" {
			if msg.ID != "" {
				fmt.Fprintf(out, "[docker] %s %s\n", msg.ID, msg.Status)
			} else {
				fmt.Fprintf(out, "[docker] %s\n", msg.Status)
			}
		}
	}

	if err := b.docker.ImageTag(ctx, imageRef, planTag); err != nil {
		return fmt.Errorf("failed to tag %s as %s: %w", imageRef, planTag, err)
	}

	fmt.Fprintf(out, "[docker] Pull complete: %s\n", planTag)
	return nil
}

// buildMsg is a single frame from the Docker image build JSON stream.
type buildMsg struct {
	Stream      string `json:"stream"`
	Error       string `json:"error"`
	ErrorDetail struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

// processBuildResponse reads the Docker image build response stream, writes
// human-readable output to out, and returns an error if the build failed.
// Docker returns 200 OK for both success and failure; errors live inside the stream.
func processBuildResponse(body io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 512*1024), 512*1024)

	var buildErr string
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg buildMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			// Not valid JSON — pass through as-is (shouldn't happen normally)
			_, _ = out.Write(line)
			_, _ = out.Write([]byte("\n"))
			continue
		}

		if msg.Stream != "" {
			_, _ = out.Write([]byte(msg.Stream))
		}

		if msg.Error != "" {
			errText := msg.Error
			if msg.ErrorDetail.Message != "" && msg.ErrorDetail.Message != msg.Error {
				errText = msg.ErrorDetail.Message
			}
			fmt.Fprintf(out, "[docker] ERROR: %s\n", errText)
			buildErr = errText
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading build response: %w", err)
	}
	if buildErr != "" {
		return fmt.Errorf("%s", buildErr)
	}
	return nil
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

// generateDockerfileIfMissing writes a template-specific Dockerfile into the
// build context when no Dockerfile is present. It is a no-op when:
//   - the user provided a custom dockerfile_location, or
//   - a Dockerfile already exists in the build context.
func generateDockerfileIfMissing(contextDir, requestedDockerfile, template string, out io.Writer) error {
	// Only auto-generate when the user left dockerfile_location at its default.
	userCustomized := strings.TrimSpace(requestedDockerfile) != "" &&
		requestedDockerfile != "Dockerfile" &&
		requestedDockerfile != "./Dockerfile"
	if userCustomized {
		return nil
	}

	dest := filepath.Join(contextDir, "Dockerfile")
	if _, err := os.Stat(dest); err == nil {
		return nil // already exists
	}

	content, ok := templateDockerfiles[strings.ToLower(strings.TrimSpace(template))]
	if !ok {
		return nil // unknown template — let resolveDockerfilePath produce the error
	}

	if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
		return fmt.Errorf("failed to write generated Dockerfile: %w", err)
	}
	fmt.Fprintf(out, "[build] No Dockerfile found — generated one for template %q\n", template)
	return nil
}

// patchNodeAlpineDockerfileForARM64 auto-heals a common Next.js-on-ARM64 issue.
// Many example Dockerfiles use `FROM node:XX-alpine` and add `libc6-compat`
// only in one stage (often `deps`). On ARM64, `next build` runs in the
// separate `builder` stage and fails with:
//   "Error relocating ... __res_init: symbol not found"
// To keep third-party examples working, we patch the temporary cloned
// Dockerfile by adding `RUN apk add --no-cache libc6-compat` to each
// Alpine-based Node stage that does not already have it.
func patchNodeAlpineDockerfileForARM64(contextDir, dockerfilePath string, out io.Writer) error {
	if runtime.GOARCH != "arm64" {
		return nil
	}
	absPath := filepath.Join(contextDir, dockerfilePath)
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil
	}
	content := string(data)
	lowerContent := strings.ToLower(content)
	if !strings.Contains(lowerContent, "alpine") || !strings.Contains(lowerContent, "node") {
		return nil
	}

	lines := strings.Split(content, "\n")
	type stage struct {
		fromIndex int
		endIndex  int
	}

	var stages []stage
	for idx, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(strings.ToUpper(line), "FROM ") {
			if len(stages) > 0 {
				stages[len(stages)-1].endIndex = idx
			}
			stages = append(stages, stage{fromIndex: idx, endIndex: len(lines)})
		}
	}
	if len(stages) == 0 {
		return nil
	}

	patchedStages := 0
	insertOffset := 0
	for _, st := range stages {
		fromIndex := st.fromIndex + insertOffset
		endIndex := st.endIndex + insertOffset
		if fromIndex >= len(lines) {
			continue
		}
		fromLine := strings.TrimSpace(strings.ToLower(lines[fromIndex]))
		if !strings.Contains(fromLine, "node:") || !strings.Contains(fromLine, "alpine") {
			continue
		}

		stageHasCompat := false
		for idx := fromIndex + 1; idx < endIndex && idx < len(lines); idx++ {
			stageLine := strings.ToLower(strings.TrimSpace(lines[idx]))
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(lines[idx])), "FROM ") {
				break
			}
			if strings.Contains(stageLine, "libc6-compat") {
				stageHasCompat = true
				break
			}
		}
		if stageHasCompat {
			continue
		}

		insertAt := fromIndex + 1
		injected := "RUN apk add --no-cache libc6-compat"
		lines = append(lines[:insertAt], append([]string{injected}, lines[insertAt:]...)...)
		insertOffset++
		patchedStages++
	}

	if patchedStages == 0 {
		return nil
	}

	if err := os.WriteFile(absPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return fmt.Errorf("failed to patch Dockerfile for ARM64 compatibility: %w", err)
	}

	fmt.Fprintf(out, "[build] Patched Dockerfile for ARM64 compatibility: added libc6-compat to %d Alpine Node stage(s)\n", patchedStages)
	return nil
}

// ensureNextStandaloneOutput makes common community Next.js examples compatible
// with Dockerfiles that expect `.next/standalone` to exist. If the Dockerfile
// copies standalone output but the repository has no `output: "standalone"`
// config, Docker build fails late with:
//   COPY failed: stat app/.next/standalone: file does not exist
// We patch the cloned temp repo only, never the source repository.
func ensureNextStandaloneOutput(contextDir, dockerfilePath, template string, out io.Writer) error {
	normalizedTemplate := strings.ToLower(strings.TrimSpace(template))
	if normalizedTemplate != "nextjs" && normalizedTemplate != "nextjs-standalone" {
		return nil
	}

	dockerfileBytes, err := os.ReadFile(filepath.Join(contextDir, dockerfilePath))
	if err != nil {
		return nil
	}
	dockerfileContent := string(dockerfileBytes)
	if !strings.Contains(dockerfileContent, ".next/standalone") {
		return nil
	}

	configNames := []string{
		"next.config.mjs",
		"next.config.js",
		"next.config.cjs",
		"next.config.ts",
		"next.config.mts",
	}

	for _, name := range configNames {
		absPath := filepath.Join(contextDir, name)
		data, readErr := os.ReadFile(absPath)
		if readErr != nil {
			continue
		}

		content := string(data)
		lower := strings.ToLower(content)
		if strings.Contains(lower, `output: "standalone"`) || strings.Contains(lower, `output:'standalone'`) ||
			strings.Contains(lower, `output: 'standalone'`) || strings.Contains(lower, `output:"standalone"`) {
			return nil
		}

		patched := injectStandaloneIntoNextConfig(content)
		if patched == "" || patched == content {
			continue
		}
		if err := os.WriteFile(absPath, []byte(patched), 0o644); err != nil {
			return fmt.Errorf("failed to patch Next.js config for standalone output: %w", err)
		}
		fmt.Fprintf(out, "[build] Patched %s to enable Next.js standalone output\n", name)
		return nil
	}

	absPath := filepath.Join(contextDir, "next.config.mjs")
	content := "/** Auto-generated by Stranger for Docker standalone builds */\nexport default {\n  output: \"standalone\",\n};\n"
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("failed to create Next.js standalone config: %w", err)
	}
	fmt.Fprintf(out, "[build] Added next.config.mjs with output=standalone for Docker compatibility\n")
	return nil
}

func injectStandaloneIntoNextConfig(content string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?m)(const\s+nextConfig\s*=\s*\{)`),
		regexp.MustCompile(`(?m)(module\.exports\s*=\s*\{)`),
		regexp.MustCompile(`(?m)(export\s+default\s*\{)`),
		regexp.MustCompile(`(?m)(defineConfig\s*\(\s*\{)`),
	}

	for _, pattern := range patterns {
		if pattern.MatchString(content) {
			return pattern.ReplaceAllString(content, `${1}`+"\n  output: \"standalone\",")
		}
	}
	return ""
}

// templateDockerfiles contains auto-generated Dockerfiles for each supported
// template. All Node.js images use node:20-slim (Debian) instead of Alpine to
// avoid the ARM64 SWC binary issue (@next/swc __res_init symbol not found).
var templateDockerfiles = map[string]string{
	"nextjs": `FROM node:20-slim AS builder
WORKDIR /app
COPY . .
RUN if [ -f yarn.lock ]; then \
      yarn install --frozen-lockfile && yarn build; \
    elif [ -f pnpm-lock.yaml ]; then \
      corepack enable pnpm && pnpm install --frozen-lockfile && pnpm run build; \
    elif [ -f package-lock.json ]; then \
      npm ci && npm run build; \
    else \
      npm install && npm run build; \
    fi

FROM node:20-slim AS runner
WORKDIR /app
ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1
COPY --from=builder /app/package.json ./package.json
COPY --from=builder /app/node_modules ./node_modules
COPY --from=builder /app/.next ./.next
COPY --from=builder /app/public ./public
EXPOSE 3000
CMD ["npm", "start"]
`,
	"node": `FROM node:20-slim AS builder
WORKDIR /app
COPY . .
RUN if [ -f yarn.lock ]; then \
      yarn install --frozen-lockfile; \
    elif [ -f pnpm-lock.yaml ]; then \
      corepack enable pnpm && pnpm install --frozen-lockfile; \
    elif [ -f package-lock.json ]; then \
      npm ci; \
    else \
      npm install; \
    fi

FROM node:20-slim AS runner
WORKDIR /app
ENV NODE_ENV=production
COPY --from=builder /app/package.json ./package.json
COPY --from=builder /app/node_modules ./node_modules
COPY --from=builder /app/. .
EXPOSE 8080
CMD ["npm", "start"]
`,
	"go": `FROM golang:1.23-bookworm AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /app/server .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/server ./server
EXPOSE 3000
CMD ["./server"]
`,
	"static": `FROM node:20-slim AS builder
WORKDIR /app
COPY . .
RUN if [ -f yarn.lock ]; then \
      yarn install --frozen-lockfile && yarn build; \
    elif [ -f pnpm-lock.yaml ]; then \
      corepack enable pnpm && pnpm install --frozen-lockfile && pnpm run build; \
    elif [ -f package-lock.json ]; then \
      npm ci && npm run build; \
    else \
      npm install && npm run build; \
    fi

FROM nginx:1.27-alpine
COPY --from=builder /app/out /usr/share/nginx/html
EXPOSE 80
CMD ["nginx", "-g", "daemon off;"]
`,
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
