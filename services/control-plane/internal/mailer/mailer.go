// Package mailer sends transactional emails via SMTP using only the Go standard library.
// It is intentionally simple: one function per email type, one SMTP connection per send.
//
// If SMTP is not configured (empty Host), all send methods are no-ops — stack deploys
// succeed normally and a warning is logged.
package mailer

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/smtp"
	"strings"
	"time"
)

// Config holds SMTP connection parameters.
// All fields except Host are optional.
type Config struct {
	Host     string // SMTP server hostname, e.g. "smtp.mailgun.org"
	Port     int    // SMTP port; defaults to 587 if zero
	Username string // SMTP auth username
	Password string // SMTP auth password
	From     string // Sender address, e.g. "Stranger <no-reply@yourapp.com>"
}

// Mailer sends transactional emails.
// The zero value (nil config) is safe to use — sends are silently skipped.
type Mailer struct {
	cfg Config
}

// New returns a Mailer.
// If cfg.Host is empty, all sends are no-ops.
func New(cfg Config) *Mailer {
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if cfg.From == "" {
		cfg.From = "Stranger <no-reply@stranger.local>"
	}
	return &Mailer{cfg: cfg}
}

// Enabled reports whether SMTP is configured.
func (m *Mailer) Enabled() bool {
	return m != nil && strings.TrimSpace(m.cfg.Host) != ""
}

// SendStackWelcome sends a welcome email after a successful stack deploy.
// Parameters:
//   - to:       admin email address
//   - appName:  human-readable project name, e.g. "my-wordpress"
//   - appType:  template name, e.g. "wordpress" (used to personalise message)
//   - siteURL:  full URL of the running site, e.g. "https://my-blog.localhost"
//   - username: admin username (may be empty for apps like Ghost/Drupal)
//   - password: admin password (may be empty if unknown)
func (m *Mailer) SendStackWelcome(to, appName, appType, siteURL, username, password string) error {
	if !m.Enabled() {
		slog.Debug("Mailer: SMTP not configured, skipping welcome email", "to", to)
		return nil
	}

	subject := fmt.Sprintf("Your %s site is ready 🎉", appName)
	body := buildWelcomeBody(appName, appType, siteURL, username, password)

	if err := m.send(to, subject, body); err != nil {
		slog.Warn("Mailer: failed to send welcome email", "to", to, "error", err)
		return err
	}

	slog.Info("Mailer: welcome email sent", "to", to, "app", appName)
	return nil
}

// buildWelcomeBody returns a plain-text email body tailored to the app type.
func buildWelcomeBody(appName, appType, siteURL, username, password string) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Hi there,\n\n"))
	sb.WriteString(fmt.Sprintf("Your %s site \"%s\" has been successfully deployed and is live!\n\n", displayName(appType), appName))
	sb.WriteString(fmt.Sprintf("Site URL: %s\n\n", siteURL))

	switch strings.ToLower(appType) {
	case "wordpress":
		sb.WriteString("--- WordPress Admin Credentials ---\n")
		sb.WriteString(fmt.Sprintf("Admin URL:      %s/wp-admin\n", strings.TrimRight(siteURL, "/")))
		if username != "" {
			sb.WriteString(fmt.Sprintf("Username:       %s\n", username))
		}
		if password != "" {
			sb.WriteString(fmt.Sprintf("Password:       %s\n", password))
		}
		sb.WriteString("\nPlease change your password after your first login.\n")

	case "directus":
		sb.WriteString("--- Directus Admin Credentials ---\n")
		sb.WriteString(fmt.Sprintf("Admin URL:      %s\n", strings.TrimRight(siteURL, "/")))
		if username != "" {
			sb.WriteString(fmt.Sprintf("Email:          %s\n", username))
		}
		if password != "" {
			sb.WriteString(fmt.Sprintf("Password:       %s\n", password))
		}
		sb.WriteString("\nPlease change your password after your first login.\n")

	case "ghost":
		sb.WriteString("--- Ghost Setup ---\n")
		sb.WriteString(fmt.Sprintf("Admin URL:      %s/ghost\n", strings.TrimRight(siteURL, "/")))
		sb.WriteString("Visit the admin URL to complete your Ghost setup and create your administrator account.\n")

	case "drupal":
		sb.WriteString("--- Drupal Setup ---\n")
		sb.WriteString(fmt.Sprintf("Install URL:    %s/install.php\n", strings.TrimRight(siteURL, "/")))
		sb.WriteString("Visit the install URL to complete the Drupal installation wizard.\n")

	default:
		if username != "" || password != "" {
			sb.WriteString("--- Admin Credentials ---\n")
			if username != "" {
				sb.WriteString(fmt.Sprintf("Username: %s\n", username))
			}
			if password != "" {
				sb.WriteString(fmt.Sprintf("Password: %s\n", password))
			}
			sb.WriteString("\nPlease change your password after your first login.\n")
		}
	}

	sb.WriteString("\n---\n")
	sb.WriteString("Sent by Stranger — your self-hosted deployment platform.\n")
	sb.WriteString(fmt.Sprintf("Sent at: %s\n", time.Now().UTC().Format(time.RFC1123)))
	return sb.String()
}

func displayName(appType string) string {
	names := map[string]string{
		"wordpress": "WordPress",
		"ghost":     "Ghost",
		"drupal":    "Drupal",
		"directus":  "Directus",
	}
	if n, ok := names[strings.ToLower(appType)]; ok {
		return n
	}
	return appType
}

// send delivers a plain-text email via SMTP.
func (m *Mailer) send(to, subject, body string) error {
	addr := fmt.Sprintf("%s:%d", m.cfg.Host, m.cfg.Port)

	from := m.cfg.From
	fromAddr := extractAddr(from)

	headers := fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=UTF-8\r\nMIME-Version: 1.0\r\n\r\n",
		from, to, subject,
	)
	message := []byte(headers + body)

	// Try STARTTLS on port 587, plain TLS on 465, plain on others.
	var auth smtp.Auth
	if m.cfg.Username != "" {
		auth = smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)
	}

	if m.cfg.Port == 465 {
		// Implicit TLS
		tlsCfg := &tls.Config{ServerName: m.cfg.Host, MinVersion: tls.VersionTLS12}
		conn, err := tls.Dial("tcp", addr, tlsCfg)
		if err != nil {
			return fmt.Errorf("mailer: tls dial: %w", err)
		}
		defer conn.Close()

		client, err := smtp.NewClient(conn, m.cfg.Host)
		if err != nil {
			return fmt.Errorf("mailer: smtp client: %w", err)
		}
		defer client.Quit()

		if auth != nil {
			if err := client.Auth(auth); err != nil {
				return fmt.Errorf("mailer: smtp auth: %w", err)
			}
		}
		if err := client.Mail(fromAddr); err != nil {
			return fmt.Errorf("mailer: MAIL FROM: %w", err)
		}
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("mailer: RCPT TO: %w", err)
		}
		w, err := client.Data()
		if err != nil {
			return fmt.Errorf("mailer: DATA: %w", err)
		}
		if _, err := w.Write(message); err != nil {
			return fmt.Errorf("mailer: write: %w", err)
		}
		return w.Close()
	}

	// STARTTLS (port 587) or plain (port 25)
	return smtp.SendMail(addr, auth, fromAddr, []string{to}, message)
}

// extractAddr extracts the bare email address from a "Name <addr>" string.
func extractAddr(from string) string {
	if start := strings.Index(from, "<"); start >= 0 {
		if end := strings.Index(from, ">"); end > start {
			return from[start+1 : end]
		}
	}
	return strings.TrimSpace(from)
}

// resolveSecrets is a tiny helper to look up keys with a fallback chain.
func ResolveSecret(secrets map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(secrets[k]); v != "" {
			return v
		}
	}
	return ""
}
