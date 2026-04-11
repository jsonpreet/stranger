# Stack App Install Settings + Welcome Email

## Goal
When installing a stack app from the marketplace (WordPress, Ghost, Drupal, Directus):
1. Show a **Site Settings** section with app-specific fields (admin email, username, password, site title) instead of generic env variables
2. Show an optional **Database Settings** section (DB name, user, password, port)
3. After a successful deploy, **send a welcome email** to the admin email with credentials and the site URL

---

## Part 1 — Site Settings & DB Settings in the Install Form

### How it works

The marketplace catalog declares what fields each stack app needs. The install dialog reads this schema and renders the appropriate form. Values override the defaults in `stack_config`.

```
Catalog declares site_settings schema
         ↓
User opens marketplace → clicks "Install" on WordPress
         ↓
Install dialog shows:
  [Site Settings]   Admin Email, Admin User, Admin Password, Site Title
  [DB Settings]     DB Name, DB User, DB Password, DB Port  (collapsed by default)
         ↓
Dashboard merges user values into stack_config env fields before submitting
         ↓
POST /projects with merged stack_config
```

---

### Proposed Changes

---

#### [MODIFY] [marketplace-catalog.js](file:///Users/harpreet/Github/stranger/apps/dashboard/lib/marketplace-catalog.js)

Add `site_settings` and `db_settings` arrays to each stack entry. These declare the form fields the install dialog should show:

```js
{
  id: "wordpress",
  ...
  type: "stack",
  site_settings: [
    { key: "ADMIN_EMAIL",     label: "Admin Email",     type: "email",    required: true,  env_target: "app",    default: "" },
    { key: "ADMIN_USER",      label: "Admin Username",  type: "text",     required: false, env_target: "app",    default: "admin" },
    { key: "ADMIN_PASSWORD",  label: "Admin Password",  type: "password", required: false, env_target: "app",    default: "${generate:password}" },
    { key: "SITE_TITLE",      label: "Site Title",      type: "text",     required: false, env_target: "app",    default: "My WordPress Site" },
  ],
  db_settings: [
    { key: "MYSQL_DATABASE",  label: "Database Name",   type: "text",     required: false, env_target: "db",     default: "wordpress" },
    { key: "MYSQL_USER",      label: "DB Username",     type: "text",     required: false, env_target: "db",     default: "wp" },
    { key: "MYSQL_PASSWORD",  label: "DB Password",     type: "password", required: false, env_target: "db",     default: "${generate:password}" },
    { key: "MYSQL_PORT",      label: "DB Port",         type: "number",   required: false, env_target: "db",     default: "3306" },
  ],
  // admin_email_key tells the backend which env var holds the admin email for the welcome mail
  admin_email_key: "ADMIN_EMAIL",
}
```

The `env_target` field tells the dashboard which service's env block (`"app"` or `"db"`) to patch when merging user values.

---

#### [NEW] install-dialog component

> **Location:** `apps/dashboard/components/stack-install-dialog.jsx`

A focused install dialog that appears when the user clicks "Install" on a stack app:

```
┌──────────────────────────────────────────────────────────┐
│  Install WordPress                                        │
│  ─────────────────────────────────────────────────────── │
│  Project Name  [my-blog                               ]  │
│                                                           │
│  ▼ Site Settings                                          │
│    Admin Email     [admin@example.com               ]    │
│    Admin Username  [admin                           ]    │
│    Admin Password  [••••••••••  👁                  ]    │
│    Site Title      [My WordPress Site               ]    │
│                                                           │
│  ▶ Database Settings  (optional, auto-configured)        │
│                                                           │
│              [ Cancel ]  [ Install WordPress  →  ]       │
└──────────────────────────────────────────────────────────┘
```

**Logic:**
1. Reads `site_settings` + `db_settings` from the catalog entry
2. For `${generate:password}` defaults, generates a random password client-side and pre-fills the field
3. On submit, deep-clones the catalog's `stack_config`, walks the services array, and patches env vars using `env_target` to find the right service
4. Calls `POST /api/projects` with the patched `stack_config` + `POST /api/projects/{id}/deploy`

---

#### [MODIFY] [marketplace-client.jsx](file:///Users/harpreet/Github/stranger/apps/dashboard/components/marketplace-client.jsx)

Replace/extend the existing install handler to:
- Check if `app.type === "stack"` and `app.site_settings`
- If yes → open `<StackInstallDialog>` instead of the quick-install flow
- Pass the catalog entry to the dialog

---

#### [MODIFY] [control-plane/main.go](file:///Users/harpreet/Github/stranger/services/control-plane/main.go)

**Current problem:** `validateCreateProjectRequest` requires a non-empty `repo_url`. Stack apps have no repo to clone — we need to relax this:

```go
func validateCreateProjectRequest(req types.CreateProjectRequest) error {
    // Stack apps have no repo_url
    isStack := strings.TrimSpace(req.StackConfig) != ""
    if !isStack {
        if err := validateRepoURL(repo); err != nil {
            return err
        }
    }
    // ...
}
```

Also need to store `admin_email` from site_settings as a project secret on creation so the welcome email can find it at deploy time. The dashboard sends it in a `InitialSecrets` map on `CreateProjectRequest`:

```go
type CreateProjectRequest struct {
    // ...existing fields...
    // InitialSecrets are stored as encrypted project secrets immediately on project creation.
    // Used for stack apps to persist admin credentials before first deploy.
    InitialSecrets map[string]string `json:"initial_secrets,omitempty"`
}
```

On create, iterate `InitialSecrets` and call `db.UpsertProjectSecret` for each.

---

## Part 2 — Post-Deploy Welcome Email

### How it works

```
Deploy succeeds (callback: status = "succeeded")
         ↓
Control plane checks: is this a stack project?
         ↓
Load project secrets → find ADMIN_EMAIL, ADMIN_USER, ADMIN_PASSWORD
         ↓
Send welcome email via SMTP
         ↓
(if SMTP not configured → skip silently, log warning)
```

---

### Proposed Changes

---

#### [NEW] [services/control-plane/internal/mailer/mailer.go](file:///Users/harpreet/Github/stranger/services/control-plane/internal/mailer/mailer.go)

A thin, zero-dependency SMTP mailer using Go's built-in `net/smtp`:

```go
type Config struct {
    Host     string // e.g. "smtp.mailgun.org"
    Port     int    // e.g. 587
    Username string
    Password string
    From     string // e.g. "Stranger <no-reply@yourdomain.com>"
}

func (m *Mailer) SendWelcome(to, appName, siteURL, username, password string) error
```

Uses `net/smtp.SendMail` — no external library needed.

#### [MODIFY] [.env / dev-start.sh](file:///Users/harpreet/Github/stranger/.env)

Add optional SMTP env vars (all optional, email is skipped if not set):

```bash
SMTP_HOST=
SMTP_PORT=587
SMTP_USER=
SMTP_PASS=
SMTP_FROM=Stranger <no-reply@yourapp.com>
```

#### [MODIFY] [services/control-plane/main.go](file:///Users/harpreet/Github/stranger/services/control-plane/main.go)

1. Initialize `mailer.New(cfg)` at startup (or `nil` if SMTP not configured)
2. In the deploy callback handler, on `status == "succeeded"`:

```go
if req.Status == types.DeployJobStatusSucceeded && mailerSvc != nil {
    go func() {
        project, _ := db.GetProject(job.ProjectID)
        if project.StackConfig == "" {
            return // not a stack app
        }
        secrets, _ := d.loadProjectSecrets(project.ID)
        adminEmail := secrets["ADMIN_EMAIL"]
        if adminEmail == "" {
            return // no email configured
        }
        siteURL := "https://" + req.Domain
        _ = mailerSvc.SendWelcome(adminEmail, project.Name, siteURL,
            secrets["ADMIN_USER"], secrets["ADMIN_PASSWORD"])
    }()
}
```

---

## Files changed summary

| File | Change |
|---|---|
| `apps/dashboard/lib/marketplace-catalog.js` | Add `site_settings`, `db_settings`, `admin_email_key` to stack entries |
| `apps/dashboard/components/stack-install-dialog.jsx` | **NEW** — full install form with site/db settings |
| `apps/dashboard/components/marketplace-client.jsx` | Route stack apps to new dialog |
| `services/control-plane/types/types.go` | Add `InitialSecrets` to `CreateProjectRequest` |
| `services/control-plane/main.go` | Relax `repo_url` validation for stacks; store `InitialSecrets` on create; trigger email on success |
| `services/control-plane/internal/mailer/mailer.go` | **NEW** — SMTP welcome email sender |

---

## Open Questions / Decisions

> [!IMPORTANT]
> **Email prerequisite:** The welcome email only works if SMTP is configured in `.env`. Without it, the deploy succeeds normally but no email is sent. We should show a setup note in the README. Do you have an SMTP provider in mind (Mailgun, Resend, Gmail, etc.)?

> [!NOTE]
> **WordPress admin setup:** The official `wordpress:latest` Docker image reads `WORDPRESS_ADMIN_USER`, `WORDPRESS_ADMIN_PASSWORD`, and `WORDPRESS_ADMIN_EMAIL` env vars **only on first run** if those vars are set. So the admin credentials the user enters in the form flow directly into the container's first-run setup — no extra scripting needed.

> [!NOTE]
> **Ghost admin setup:** Ghost doesn't auto-create an admin from env vars. After deployment, the user must visit `/ghost` to set up their account. The welcome email can still include the site URL with instructions.

> [!NOTE]
> **Drupal/Directus:** Directus does read `ADMIN_EMAIL` and `ADMIN_PASSWORD` from env — they work the same as WordPress. Drupal requires manual setup via `/install.php` on first visit.

> [!IMPORTANT]
> **Password visibility in Secrets tab:** The generated/user-supplied admin password is stored as an encrypted project secret (`ADMIN_PASSWORD`). Users can view it in the project's Secrets tab if they need it later. Should we also show a "Credentials" card in the project dashboard directly?
