package router

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
)

type CaddyRouter struct {
	ConfigPath string
	statePath  string
	mu         sync.Mutex
}

func NewCaddyRouter() *CaddyRouter {
	configPath := "Caddyfile"
	return &CaddyRouter{
		ConfigPath: configPath,
		statePath:  filepath.Join(filepath.Dir(configPath), "routes.json"),
	}
}

// Simple Caddyfile template
const caddyTemplate = `
{
	admin off
}

{{range .Routes}}
{{.Domain}} {
	reverse_proxy localhost:{{.Port}}
}
{{end}}
`

type Route struct {
	Domain string
	Port   string
}

type RouterConfig struct {
	Routes []Route
}

func (r *CaddyRouter) UpdateRoute(domain string, targetPort string) error {
	domain = strings.ToLower(strings.TrimSpace(domain))
	targetPort = strings.TrimSpace(targetPort)

	if !isValidDomain(domain) {
		return fmt.Errorf("invalid domain: %s", domain)
	}
	if !isValidPort(targetPort) {
		return fmt.Errorf("invalid target port: %s", targetPort)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	slog.Info("Updating router config", "domain", domain, "target", targetPort)

	routes, err := r.loadRoutes()
	if err != nil {
		return err
	}
	routes[domain] = targetPort

	if err := r.saveRoutes(routes); err != nil {
		return err
	}
	if err := r.writeConfig(routes); err != nil {
		return err
	}

	return r.Reload()
}

func (r *CaddyRouter) Reload() error {
	slog.Info("Reloading Caddy...")

	cmd := exec.Command("caddy", "reload", "--config", r.ConfigPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		slog.Error("Caddy reload failed", "output", string(output))
		return fmt.Errorf("caddy reload failed: %w", err)
	}
	return nil
}

func (r *CaddyRouter) loadRoutes() (map[string]string, error) {
	content, err := os.ReadFile(r.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]string), nil
		}
		return nil, err
	}
	if len(content) == 0 {
		return make(map[string]string), nil
	}

	var routes map[string]string
	if err := json.Unmarshal(content, &routes); err != nil {
		return nil, fmt.Errorf("failed to parse route state: %w", err)
	}
	if routes == nil {
		routes = make(map[string]string)
	}
	return routes, nil
}

func (r *CaddyRouter) saveRoutes(routes map[string]string) error {
	payload, err := json.MarshalIndent(routes, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.statePath, payload, 0o600)
}

func (r *CaddyRouter) writeConfig(routes map[string]string) error {
	domains := make([]string, 0, len(routes))
	for domain := range routes {
		domains = append(domains, domain)
	}
	sort.Strings(domains)

	ordered := make([]Route, 0, len(domains))
	for _, domain := range domains {
		ordered = append(ordered, Route{
			Domain: domain,
			Port:   routes[domain],
		})
	}

	config := RouterConfig{Routes: ordered}
	tmpl, err := template.New("caddy").Parse(caddyTemplate)
	if err != nil {
		return err
	}

	configFile, err := os.CreateTemp(filepath.Dir(r.ConfigPath), ".caddyfile-*")
	if err != nil {
		return err
	}
	defer os.Remove(configFile.Name())

	if err := tmpl.Execute(configFile, config); err != nil {
		configFile.Close()
		return err
	}

	if err := configFile.Chmod(0o644); err != nil {
		configFile.Close()
		return err
	}
	if err := configFile.Close(); err != nil {
		return err
	}

	return os.Rename(configFile.Name(), r.ConfigPath)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tempFile, err := os.CreateTemp(filepath.Dir(path), ".routes-*")
	if err != nil {
		return err
	}
	defer os.Remove(tempFile.Name())

	if _, err := tempFile.Write(data); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Chmod(mode); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}

	return os.Rename(tempFile.Name(), path)
}

var domainLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func isValidDomain(domain string) bool {
	if len(domain) == 0 || len(domain) > 253 {
		return false
	}

	labels := strings.Split(domain, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if !domainLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func isValidPort(port string) bool {
	number, err := strconv.Atoi(port)
	if err != nil {
		return false
	}
	return number > 0 && number < 65536
}
