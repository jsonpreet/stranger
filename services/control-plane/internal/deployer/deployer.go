package deployer

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/stranger/control-plane/types"
)

var (
	projectLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	templateSpecs       = map[string]struct {
		dockerfile string
		port       int
	}{
		"nextjs": {dockerfile: "Dockerfile", port: 3000},
		"go":     {dockerfile: "Dockerfile", port: 3000},
		"node":   {dockerfile: "Dockerfile", port: 8080},
		"static": {dockerfile: "Dockerfile", port: 80},
	}
)

func IsSupportedTemplate(template string) bool {
	_, ok := templateSpecs[strings.ToLower(strings.TrimSpace(template))]
	return ok
}

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

	plan := types.DeployPlan{
		ID:        uuid.New().String(),
		ProjectID: project.ID,
		Template:  template,
		Build: types.BuildSpec{
			RepoURL:       project.RepoURL,
			Dockerfile:    normalizedDockerfileLocation(project.DockerfileLocation, spec.dockerfile),
			BaseDirectory: normalizedBaseDirectory(project.BaseDirectory),
		},
		Runtime: types.RuntimeSpec{
			Port: spec.port,
			Env:  make(map[string]string),
		},
		Router: types.RouterSpec{
			Domain: fmt.Sprintf("%s.localhost", label),
		},
	}

	return plan, nil
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
