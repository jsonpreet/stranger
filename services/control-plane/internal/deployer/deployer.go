package deployer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/stranger/control-plane/types"
)

var (
	projectLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	templateSpecs = map[string]struct {
		dockerfile  string
		port        int
		buildMode   string // "" = docker build, "nixpacks" = nixpacks CLI
		dockerImage string // Docker Hub image — skip build entirely
	}{
		// ── Docker-build (repo has Dockerfile or auto-generated) ──────────────────
		"nextjs":            {dockerfile: "Dockerfile", port: 3000},
		"nextjs-standalone": {dockerfile: "Dockerfile", port: 3000},
		"nuxt":              {dockerfile: "Dockerfile", port: 3000},
		"t3":                {dockerfile: "Dockerfile", port: 3000},
		"go":                {dockerfile: "Dockerfile", port: 3000},
		"node":              {dockerfile: "Dockerfile", port: 8080},
		"nodejs":            {dockerfile: "Dockerfile", port: 3000},
		"nestjs":            {dockerfile: "Dockerfile", port: 3000},
		"bun":               {dockerfile: "Dockerfile", port: 3000},
		"adonisjs":          {dockerfile: "Dockerfile", port: 8080},
		"rails":             {dockerfile: "Dockerfile", port: 3000},
		"remix":             {dockerfile: "Dockerfile", port: 3000},
		"rust":              {dockerfile: "Dockerfile", port: 3000},
		"shopware6":         {dockerfile: "Dockerfile", port: 8000},
		"static":            {dockerfile: "Dockerfile", port: 80},
		"strapi":            {dockerfile: "Dockerfile", port: 1337},
		"vite":              {dockerfile: "Dockerfile", port: 80},
		"vue":               {dockerfile: "Dockerfile", port: 80},
		"astro":             {dockerfile: "Dockerfile", port: 80},
		"elixir":            {dockerfile: "Dockerfile", port: 4000},
		// ── Nixpacks (no Dockerfile — auto-detected) ───────────────────────────
		"laravel":           {buildMode: "nixpacks", port: 80},
		"laravel-inertia":   {buildMode: "nixpacks", port: 80},
		"symfony":           {buildMode: "nixpacks", port: 80},
		"flask":             {buildMode: "nixpacks", port: 5000},
		// ── Docker Hub images (pulled directly, no build) ────────────────────
		"n8n":         {dockerImage: "n8nio/n8n:latest",                     port: 5678},
		"pocketbase":  {dockerImage: "ghcr.io/muchobien/pocketbase:latest",  port: 8090},
		"uptime-kuma": {dockerImage: "louislam/uptime-kuma:latest",         port: 3001},
		"vaultwarden": {dockerImage: "vaultwarden/server:latest",           port: 80},
		"filebrowser": {dockerImage: "filebrowser/filebrowser:latest",      port: 80},
		"nocodb":      {dockerImage: "nocodb/nocodb:latest",                port: 8080},
		"metabase":    {dockerImage: "metabase/metabase:latest",            port: 3000},
		"minio":       {dockerImage: "minio/minio:latest",                  port: 9000},
		"portainer":   {dockerImage: "portainer/portainer-ce:latest",       port: 9000},
		"gitea":       {dockerImage: "gitea/gitea:latest",                  port: 3000},
		// ── Stack templates (no single-container spec — use GenerateStackPlan) ────
		"wordpress":   {port: 80},
		"ghost":       {port: 2368},
		"drupal":      {port: 80},
		"directus":    {port: 8055},
	}
)

func IsSupportedTemplate(template string) bool {
	_, ok := templateSpecs[strings.ToLower(strings.TrimSpace(template))]
	return ok
}

// GeneratePlan creates a single-container DeployPlan from a Project.
func GeneratePlan(project types.Project) (types.DeployPlan, error) {
	template := strings.ToLower(strings.TrimSpace(project.Template))
	spec, ok := templateSpecs[template]
	if !ok {
		return types.DeployPlan{}, fmt.Errorf("unknown template: %s", project.Template)
	}

	label, err := sanitizeProjectLabel(project.Name)
	if err != nil {
		return types.DeployPlan{}, err
	}

	dockerfilePath := normalizedDockerfileLocation(project.DockerfileLocation, spec.dockerfile)
	baseDirectory := normalizedBaseDirectory(project.BaseDirectory)

	buildMode := spec.buildMode
	gitSHA := getGitHeadSHA(project.RepoURL)
	cacheKey := buildCacheKey(template, project.RepoURL, gitSHA, dockerfilePath, baseDirectory)

	domain := fmt.Sprintf("%s.localhost", label)
	if cd := strings.TrimSpace(project.CustomDomain); cd != "" {
		domain = cd
	}

	plan := types.DeployPlan{
		ID:        uuid.New().String(),
		ProjectID: project.ID,
		Template:  template,
		Build: types.BuildSpec{
			RepoURL:       project.RepoURL,
			Dockerfile:    dockerfilePath,
			BaseDirectory: baseDirectory,
			CacheKey:      cacheKey,
			BuildMode:     buildMode,
			DockerImage:   spec.dockerImage,
		},
		Runtime: types.RuntimeSpec{
			Port:        firstNonZero(project.Port, spec.port),
			Env:         make(map[string]string),
			MemoryLimit: project.MemoryLimit,
			CPULimit:    project.CPULimit,
		},
		Router: types.RouterSpec{
			Domain: domain,
		},
	}

	return plan, nil
}

// stackCatalogEntry is the JSON shape stored in Project.StackConfig and the marketplace catalog.
type stackCatalogEntry struct {
	Services []stackServiceEntry `json:"services"`
}

type stackServiceEntry struct {
	Key         string            `json:"key"`
	DockerImage string            `json:"docker_image"`
	Port        int               `json:"port"`
	Internal    bool              `json:"internal"`
	DependsOn   []string          `json:"depends_on"`
	Env         map[string]string `json:"env"`
	Volume      string            `json:"volume"`
}

// GenerateStackPlan creates a StackDeployPlan for multi-service (pipeline) projects.
// It resolves ${generate:password} by generating a random hex password, and
// ${service:key:host} / ${service:key:env:X} by referring to already-resolved values.
func GenerateStackPlan(project types.Project) (types.StackDeployPlan, map[string]string, error) {
	var catalog stackCatalogEntry
	if err := json.Unmarshal([]byte(project.StackConfig), &catalog); err != nil {
		return types.StackDeployPlan{}, nil, fmt.Errorf("invalid stack_config JSON: %w", err)
	}

	label, err := sanitizeProjectLabel(project.Name)
	if err != nil {
		return types.StackDeployPlan{}, nil, err
	}

	domain := fmt.Sprintf("%s.localhost", label)
	if cd := strings.TrimSpace(project.CustomDomain); cd != "" {
		domain = cd
	}

	networkName := fmt.Sprintf("stranger-stack-%s", project.ID)

	// First pass: resolve ${generate:password} and build per-service env maps.
	// We track generated passwords so they're consistent within one plan.
	generatedPasswords := map[string]string{} // key -> generated value
	resolvedServiceEnvs := map[string]map[string]string{} // serviceKey -> resolved env
	containerNames := map[string]string{} // serviceKey -> container name

	for _, svc := range catalog.Services {
		cName := fmt.Sprintf("stack-%s-%s", project.ID, svc.Key)
		containerNames[svc.Key] = cName
		resolvedServiceEnvs[svc.Key] = map[string]string{}
	}

	// Generated values the caller should surface to the user (secrets).
	exposedSecrets := map[string]string{}

	var services []types.StackService
	for _, svc := range catalog.Services {
		env := make(map[string]string, len(svc.Env))
		for k, v := range svc.Env {
			env[k] = resolveEnvValue(v, svc.Key, k, generatedPasswords, resolvedServiceEnvs, containerNames, exposedSecrets)
		}
		resolvedServiceEnvs[svc.Key] = env

		volumeName := ""
		if svc.Volume != "" {
			volumeName = fmt.Sprintf("stranger-vol-%s-%s", project.ID, svc.Key)
		}

		services = append(services, types.StackService{
			Key:           svc.Key,
			ContainerName: containerNames[svc.Key],
			DockerImage:   svc.DockerImage,
			Port:          svc.Port,
			Internal:      svc.Internal,
			DependsOn:     svc.DependsOn,
			Env:           env,
			Volume:        volumeName,
		})
	}

	plan := types.StackDeployPlan{
		ID:        uuid.New().String(),
		ProjectID: project.ID,
		Network:   networkName,
		Services:  services,
		Router: types.RouterSpec{
			Domain: domain,
		},
	}

	return plan, exposedSecrets, nil
}

// resolveEnvValue replaces template placeholders in an env value string.
//
//	${generate:password}         → 24-char random hex (stable per project deploy)
//	${service:<key>:host}        → Docker container name of that service
//	${service:<key>:env:<NAME>}  → already-resolved env var value of that service
func resolveEnvValue(
	raw, _ /* serviceKey */, envKey string,
	passwordCache map[string]string,
	resolvedEnvs map[string]map[string]string,
	containerNames map[string]string,
	exposed map[string]string,
) string {
	result := raw

	// ${generate:password}
	result = strings.ReplaceAll(result, "${generate:password}", func() string {
		if v, ok := passwordCache[envKey]; ok {
			return v
		}
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		v := hex.EncodeToString(b)[:24]
		passwordCache[envKey] = v
		exposed[envKey] = v
		return v
	}())

	// ${service:<key>:host} and ${service:<key>:env:<NAME>}
	re := regexp.MustCompile(`\$\{service:([^:}]+):([^}]+)\}`)
	result = re.ReplaceAllStringFunc(result, func(match string) string {
		parts := re.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		svcKey := parts[1]
		ref := parts[2]

		if ref == "host" {
			if name, ok := containerNames[svcKey]; ok {
				return name
			}
		}

		if strings.HasPrefix(ref, "env:") {
			envName := strings.TrimPrefix(ref, "env:")
			if svcEnv, ok := resolvedEnvs[svcKey]; ok {
				if v, ok2 := svcEnv[envName]; ok2 {
					return v
				}
			}
		}
		return match // unresolved — leave as-is
	})

	return result
}

// isStackTemplate returns true when the template is a multi-service stack.
func IsStackTemplate(template string) bool {
	switch strings.ToLower(strings.TrimSpace(template)) {
	case "wordpress", "ghost", "drupal", "directus":
		return true
	}
	return false
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

// getGitHeadSHA queries the remote HEAD SHA using git ls-remote.
// Returns empty string on any error (caching is skipped but deploy proceeds normally).
func getGitHeadSHA(repoURL string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "git", "ls-remote", "--quiet", "--exit-code", repoURL, "HEAD").Output()
	if err != nil {
		slog.Warn("Build cache disabled: git ls-remote failed (private repo or network issue)", "repo", repoURL, "error", err)
		return ""
	}

	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// DetectTemplate performs a shallow no-checkout clone and inspects root-level files
// to determine the best-matching template. Returns "" if detection fails or is inconclusive.
func DetectTemplate(repoURL string) string {
	tmpDir, err := os.MkdirTemp("", "stranger-detect-*")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(tmpDir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Shallow no-checkout clone — only fetches HEAD commit metadata + tree
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--no-checkout", repoURL, tmpDir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err != nil {
		slog.Debug("DetectTemplate: clone failed", "repo", repoURL, "error", err)
		return ""
	}

	// List root-level files from the HEAD tree
	out, err := exec.CommandContext(ctx, "git", "-C", tmpDir, "ls-tree", "--name-only", "HEAD").Output()
	if err != nil {
		return ""
	}

	files := make(map[string]bool)
	for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		files[strings.TrimSpace(f)] = true
	}

	// Detection order: Go > Next.js > Node > Static
	if files["go.mod"] {
		return "go"
	}
	if files["package.json"] {
		pkgData, err := exec.CommandContext(ctx, "git", "-C", tmpDir, "show", "HEAD:package.json").Output()
		if err == nil && strings.Contains(string(pkgData), `"next"`) {
			return "nextjs"
		}
		return "node"
	}
	if files["index.html"] || files["_site"] {
		return "static"
	}
	// Dockerfile present → likely node or go, default to node
	if files["Dockerfile"] {
		return "node"
	}

	return ""
}

// buildCacheKey computes a short SHA-256 fingerprint of the build inputs.
// Returns empty string if gitSHA is unavailable (disables caching).
func buildCacheKey(template, repoURL, gitSHA, dockerfilePath, baseDirectory string) string {
	if gitSHA == "" {
		return ""
	}
	input := template + "|" + repoURL + "|" + gitSHA + "|" + dockerfilePath + "|" + baseDirectory
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:16]
}

func normalizedDockerfileLocation(value, fallback string) string {
	clean := strings.TrimSpace(value)
	if clean == "" {
		return fallback
	}
	return strings.TrimPrefix(clean, "./")
}

func normalizedBaseDirectory(value string) string {
	clean := strings.TrimSpace(value)
	if clean == "" {
		return "."
	}
	return clean
}

func sanitizeProjectLabel(name string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	if normalized == "" {
		return "", fmt.Errorf("project name cannot be empty")
	}

	replacer := strings.NewReplacer(" ", "-", "_", "-", ".", "-")
	normalized = replacer.Replace(normalized)
	normalized = strings.Trim(normalized, "-")
	if len(normalized) > 63 {
		normalized = strings.Trim(normalized[:63], "-")
	}

	if normalized == "" || !projectLabelPattern.MatchString(normalized) {
		return "", fmt.Errorf("project name must resolve to a valid DNS label")
	}

	return normalized, nil
}
