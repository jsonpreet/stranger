# Marketplace Expansion + Pipeline Support

Three phases ordered by complexity. Each phase is independently deployable.

---

## Phase 1 — Pre-built Docker Hub Images (~1 hour, 3 files)

Many great apps (n8n, Pocketbase, Uptime Kuma) are published on Docker Hub and don't need to be built from source. Right now the agent only handles `PrebuiltImageTag` for rollbacks (requires image to already exist locally). We need a **pull-from-registry** path.

### Proposed Changes

---

#### [MODIFY] [types/types.go (agent)](file:///Users/harpreet/Github/stranger/services/agent/types/types.go) + [types/types.go (control-plane)](file:///Users/harpreet/Github/stranger/services/control-plane/types/types.go)

Add `DockerImage` to `BuildSpec` in both packages:

```go
type BuildSpec struct {
    // ... existing fields ...
    // DockerImage: when set, skip all build steps and pull this image from Docker Hub.
    DockerImage string `json:"docker_image,omitempty"` // e.g. "wordpress:latest"
}
```

---

#### [MODIFY] [deployer.go](file:///Users/harpreet/Github/stranger/services/control-plane/internal/deployer/deployer.go)

Add `dockerImage` field to `templateSpecs`. When set, populate `BuildSpec.DockerImage` and leave `Dockerfile` empty:

```go
templateSpecs = map[string]struct {
    dockerfile  string
    port        int
    buildMode   string
    dockerImage string // hub image — skips build entirely
}{
    // ...existing docker/nixpacks entries...

    // Pre-built Docker Hub images
    "n8n":          {dockerImage: "n8nio/n8n:latest",                    port: 5678},
    "pocketbase":   {dockerImage: "ghcr.io/muchobien/pocketbase:latest", port: 8090},
    "uptime-kuma":  {dockerImage: "louislam/uptime-kuma:latest",         port: 3001},
    "vaultwarden":  {dockerImage: "vaultwarden/server:latest",           port: 80},
    "filebrowser":  {dockerImage: "filebrowser/filebrowser:latest",      port: 80},
    "nocodb":       {dockerImage: "nocodb/nocodb:latest",                port: 8080},
    "metabase":     {dockerImage: "metabase/metabase:latest",            port: 3000},
    "minio":        {dockerImage: "minio/minio:latest",                  port: 9000},
    "portainer":    {dockerImage: "portainer/portainer-ce:latest",       port: 9000},
    "gitea":        {dockerImage: "gitea/gitea:latest",                  port: 3000},
}
```

Populate in `GeneratePlan`:
```go
Build: types.BuildSpec{
    DockerImage:   spec.dockerImage,
    RepoURL:       project.RepoURL,
    // ...
}
```

---

#### [MODIFY] [builder.go](file:///Users/harpreet/Github/stranger/services/agent/internal/builder/builder.go)

Add `buildFromDockerImage()` and route to it at the top of `Build()`:

```go
// Docker Hub image — pull and tag, no build needed
if plan.Build.DockerImage != "" {
    return b.buildFromDockerImage(ctx, plan.Build.DockerImage, planImageTag, tee)
}

func (b *Builder) buildFromDockerImage(ctx, imageRef, planTag, out) (string, error) {
    // ImagePull streams progress JSON, pipe it to out
    // Then ImageTag as planTag
}
```

---

## Phase 2 — New Single-Container Marketplace Entries (~30 min, 1 file)

#### [MODIFY] [marketplace-catalog.js](file:///Users/harpreet/Github/stranger/apps/dashboard/lib/marketplace-catalog.js)

Add 10 new entries using `docker_image` (no `repo_url` or `template` needed):

| App | Image | Port | Category |
|---|---|---|---|
| n8n | `n8nio/n8n:latest` | 5678 | Automation |
| Pocketbase | `ghcr.io/muchobien/pocketbase:latest` | 8090 | Backend |
| Uptime Kuma | `louislam/uptime-kuma:latest` | 3001 | Monitoring |
| Vaultwarden | `vaultwarden/server:latest` | 80 | Security |
| FileBrowser | `filebrowser/filebrowser:latest` | 80 | Tools |
| Nocodb | `nocodb/nocodb:latest` | 8080 | Databases |
| Metabase | `metabase/metabase:latest` | 3000 | Analytics |
| Minio | `minio/minio:latest` | 9000 | Storage |
| Portainer | `portainer/portainer-ce:latest` | 9000 | DevOps |
| Gitea | `gitea/gitea:latest` | 3000 | DevOps |

---

## Phase 3 — Stack / Pipeline System (~2 days, major feature)

Enables WordPress, Ghost, Drupal, Directus — apps that require a database first.

### How it works

```
User installs "WordPress"
        ↓
Control plane generates StackDeployPlan with 2 services in order:
  [0] db  → mysql:8.0    (internal, no Caddy route)
  [1] app → wordpress:latest (depends_on: db)
        ↓
Agent creates a Docker bridge network: stack-{projectID}
Agent deploys db container first, waits for TCP health check on 3306
Agent injects db's hostname/env into app's env vars
Agent deploys app container, waits for TCP health check on 80
Agent creates Caddy route for app container only
        ↓
User gets one URL → WordPress is live and connected to MySQL
```

### New apps unlocked by stacks

| Stack | Services | Notes |
|---|---|---|
| WordPress | MySQL 8 + wordpress:latest | Most popular CMS |
| Ghost | MySQL 8 + ghost:latest | Modern blogging |
| Drupal | PostgreSQL 16 + drupal:latest | Enterprise CMS |
| Directus | PostgreSQL + Redis + directus:latest | Headless CMS |
| Umami | PostgreSQL + ghcr.io/umami-software/umami | Analytics |
| Gitea (full) | PostgreSQL + gitea:latest | Self-hosted GitHub |
| Outline | PostgreSQL + Redis + outlinewiki/outline | Team wiki |

### Proposed Data Shape (marketplace-catalog.js)

```js
{
  id: "wordpress",
  name: "WordPress",
  type: "stack",
  category: "CMS",
  description: "WordPress — the world's most popular CMS.",
  services: [
    {
      key: "db",
      docker_image: "mysql:8.0",
      port: 3306,
      internal: true,
      env: {
        MYSQL_DATABASE:      "wordpress",
        MYSQL_USER:          "wp",
        MYSQL_PASSWORD:      "${generate:password}",
        MYSQL_ROOT_PASSWORD: "${generate:password}",
      }
    },
    {
      key: "app",
      docker_image: "wordpress:latest",
      port: 80,
      depends_on: ["db"],
      env: {
        WORDPRESS_DB_HOST:     "${service:db:host}",
        WORDPRESS_DB_USER:     "${service:db:env:MYSQL_USER}",
        WORDPRESS_DB_PASSWORD: "${service:db:env:MYSQL_PASSWORD}",
        WORDPRESS_DB_NAME:     "${service:db:env:MYSQL_DATABASE}",
      }
    }
  ]
}
```

### Files touched in Phase 3

| File | Change |
|---|---|
| `control-plane/types/types.go` | Add `StackDeployPlan`, `StackService` types |
| `control-plane/internal/store/sqlite.go` | New `stack_services` table |
| `control-plane/internal/deployer/deployer.go` | `GenerateStackPlan()` with password generation + env var resolution |
| `control-plane/internal/queue/dispatcher.go` | Route stack projects to `/deploy/stack` on agent |
| `agent/types/types.go` | Mirror `StackDeployPlan` type |
| `agent/internal/container/manager.go` | Docker network creation, multi-service deploy |
| `agent/main.go` | New `/deploy/stack` handler |
| `apps/dashboard/lib/marketplace-catalog.js` | Stack entries for WordPress, Ghost, Drupal, etc. |
| `apps/dashboard/components/marketplace-client.jsx` | Show "requires: MySQL" badge on stack apps |
| `apps/dashboard/components/project-details-client.jsx` | Show all services + their individual status |

---

## Delivery Order

> [!IMPORTANT]
> **Recommended: Ship Phase 1 → Phase 2 → Phase 3 separately.**
> Phase 1+2 are low-risk. Phase 3 is a significant architectural change.

| Phase | Effort | Risk | Value |
|---|---|---|---|
| 1 — Docker Hub pull | ~1 hour | Low | Enables all single-container apps |
| 2 — 10 new apps | ~30 min | None | Bigger marketplace immediately |
| 3 — Stack/pipeline | ~2 days | Medium | WordPress, Ghost, Drupal, etc. |
