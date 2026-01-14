package router

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"text/template"
)

type CaddyRouter struct {
	ConfigPath string
}

func NewCaddyRouter() *CaddyRouter {
	return &CaddyRouter{
		ConfigPath: "Caddyfile", // In current dir for MVP
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
	slog.Info("Updating router config", "domain", domain, "target", targetPort)

	// NOTE: In a real system, we'd maintain state of all routes.
	// For MVP, we'll just write a single file for this one app (Overwriting previous!).
	
	config := RouterConfig{
		Routes: []Route{
			{Domain: domain, Port: targetPort},
		},
	}

	tmpl, err := template.New("caddy").Parse(caddyTemplate)
	if err != nil {
		return err
	}

	f, err := os.Create(r.ConfigPath)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := tmpl.Execute(f, config); err != nil {
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
