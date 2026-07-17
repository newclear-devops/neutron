# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Neutron is a lightweight CI/CD pipeline system built on Kubernetes. It receives webhooks from code hosting platforms (GitLab, Codeup), parses pipeline definitions from `neutron.yaml` in the repository, and launches Kubernetes Jobs to execute pipeline steps. Each runner reports status back to both the platform (GitLab commit statuses; Codeup no-op) and the Neutron API server for persistence and history tracking.

## Build Commands

```bash
make api      # builds API server → bin/neutron-api (CGO_ENABLED=0, statically linked)
make gitlab   # builds GitLab runner → bin/neutron-gitlab-runner
make codeup   # builds Codeup runner → bin/neutron-codeup-runner

# Full rebuild
make clean && make api && make gitlab && make codeup

# Build Docker images and load into kind
make kind-load

# Run tests (none exist yet)
go test ./...
```

## Architecture

### Three Binaries

**API Server** (`cmd/api/main.go`) — Gin-based HTTP server with SPA frontend:
- `GET /api/config` — returns runtime config (log URL template, namespace) for SPA
- `POST /api/register` — registers a project webhook (stores in MySQL with UUID)
- `POST /webhook/:id` — receives webhooks, auto-detects platform (GitLab/Codeup) via `X-Codeup-Event` header, fetches `neutron.yaml`, creates K8s Jobs. Query params on the webhook URL are passed as env vars to the pod.
- `POST /api/trigger` — programmatic pipeline trigger by repo URL, job name, ref, and custom env vars (bypasses trigger type validation)
- `GET /api/projects` — lists all registered projects
- `GET /api/projects/:id/jobs` — lists jobs for a project (last 7 days)
- `GET /api/status/:jobName` — job/pod status (JSON, from DB for completed jobs or K8s API for active jobs)
- `POST /api/report/:jobName` — runners push status back to API server for persistence
- `POST /api/report/:jobName/link` — set a test report URL for a job (`{"report_url": "..."}`)
- `POST /api/jobs/:jobName/rerun` — rerun a webhook-created job by recreating an identical K8s Job from its persisted spec (same commit/params/trigger, reports to platform like the original). Only jobs with a stored spec are rerunnable.
- `GET /api/default-pipeline` — returns the global default pipeline (`{"content": "..."}`, empty if unset). Shared by the SPA editor and the pod-side runner fallback.
- `PUT /api/default-pipeline` — set the global default pipeline (`{"content": "..."}`). Validates non-empty content parses to a `model.Pipeline` with at least one job; empty content disables the fallback.
- SPA: `cmd/api/static/index.html` — vanilla JS with hash-based routing (#/, #/projects, #/project/:id, #/status/:name, #/snippets, #/default-pipeline)

**GitLab Runner** (`cmd/gitlab-runner/`) — runs inside K8s pods for GitLab projects:
- Reads config from environment variables (set by API server when creating the Job)
- Reads `neutron.yaml` from cloned repo, executes steps sequentially
- Uses CompositeReporter: reports to GitLab commit statuses + Neutron API (`/api/report/:jobName`)

**Codeup Runner** (`cmd/codeup-runner/`) — runs inside K8s pods for Codeup projects:
- Same execution logic as GitLab runner
- Uses CompositeReporter: NoOp reporter (Codeup has no status API) + Neutron API (`/api/report/:jobName`)

### Data Flow

1. Webhook → `/webhook/:id` (GitLab or Codeup)
2. API server auto-detects platform, parses webhook, fetches `neutron.yaml` via platform API. If the repo has no `neutron.yaml` (platform API returns 404), it falls back to the global default pipeline (see **Default Pipeline Fallback**); if no default is configured, the request fails.
3. API server creates K8s Job with platform-appropriate runner binary
4. Main container runs runner → reads `neutron.yaml` (or, if absent, fetches the global default via `GET /api/default-pipeline`) → executes steps → reports status to both the platform API and Neutron API (`/api/report/:jobName`)
5. Status queries (`/api/status/:jobName`) return from DB for completed jobs, or K8s API + persist to DB for active jobs

### Key Packages

- `internal/gitlab/` — GitLab webhook parsing (`parser.go`)
- `internal/codeup/` — Codeup webhook parsing (`parser.go`)
- `internal/parser/` — shared parsing logic: `base.go` (fetch neutron.yaml), `path.go` (repo URL → API path conversion for GitLab `%2F` and Codeup `%252F`)
- `internal/launcher/` — shared K8s Job creation (platform-agnostic)
- `internal/model/` — domain models: `Config`, `Pipeline`, `Job`, `Step`, `RunnerConfig` + interfaces: `Reporter`, `PipelineParser`
- `internal/service/` — `Runner` (step execution, supports `SkipTriggerCheck`)
- `internal/repo.go` — MySQL data access (Repository pattern)
- `internal/notify/` — IM notification client (enterprise messaging, attachment format)
- `internal/ccwork/` — CCWork robot webhook client (group notifications, attachment format)
- `internal/reporter/` — Composite reporter (fan-out to multiple backends)
- `cmd/api/` — API server with embedded SPA (static/index.html)
- `cmd/gitlab-runner/` — GitLab runner binary + CompositeReporter (GitLab + Neutron)
- `cmd/codeup-runner/` — Codeup runner binary + CompositeReporter (NoOp + Neutron)

### Database (MySQL)

Tables auto-migrated by GORM:
- `neutron_project` (id, webhook_type, repo_url)
- `neutron_job` (id, project_id, name, status, notify, spec, completed, completed_at) — `notify` is JSON-encoded `model.Notify`; `spec` is JSON-encoded `model.JobSpec` (rerun snapshot), captured from the job's `neutron.yaml`/webhook at trigger time
- `neutron_pod` (id, job_id, pod_name, pod_uid, phase)
- `neutron_job_report` (id, job_name, report_url, created_at) — test report link per job
- `neutron_setting` (key, value, updated_at) — generic key/value store for global config; currently holds the default pipeline under key `default_pipeline` (see **Default Pipeline Fallback**)

### Configuration

Runtime config is `config.yaml` (gitignored). Shape defined by `internal/model/config.go`: host, port, database (MySQL DSN), salt, log_url (external log platform link template with {namespace} and {podName} placeholders, optional), codebase map (url/token/skip_tls_verify per platform: GitLab, Codeup), pod_codebase (pod-side codebase addresses, optional), kubernetes (kube-config path — optional for in-cluster, auto-detected via ServiceAccount; required for out-of-cluster, namespace, git-private-key secret, init-image, checkout-image, image-pull-secrets), notify (IM notification config: url, corp_id, app_id, skip_tls_verify). Most fields can be overridden via environment variables (NEUTRON_*).

### Notifications

Notifications are configured **per job** in the repository's `neutron.yaml`, under each job's optional `notify` block:

```yaml
jobs:
  build:
    trigger: [PUSH, MR]
    notify:
      users:  [zhangsan, lisi]                          # IM personal-message recipients (user ids)
      groups: ["https://ccwork.example.com/robot/send?key=..."]  # CCWork group robot webhook URLs
```

Two channels, sent in parallel (fire-and-forget) on pipeline trigger and completion for each job that declares targets:
1. **IM notifications** (`internal/notify/`) — sends to individual users via enterprise IM bot API (`notify.users`).
2. **CCWork group webhooks** (`internal/ccwork/`) — sends to group chats via webhook URLs (`notify.groups`).

Both use structured attachment format with title (head) and body content. A job with no `notify` block sends nothing.

The config is parsed at trigger time and persisted as JSON on `neutron_job.notify`, so the completion handler (`POST /api/report/:jobName`, which only knows the job name) can read the same targets back via the `GetJobByName` lookup it already performs — see `sendJobNotifications` / `marshalNotify` / `parseNotify` in `cmd/api/`.

### Webhook URL Parameters

Custom parameters can be passed to pipeline pods by appending query parameters to the webhook URL:

```
POST https://neutron.example.com/webhook/abc-123?DEPLOY_ENV=prod&IMAGE_TAG=v1.2.3
```

All query parameters are injected as environment variables into the K8s Job's main container. Step commands can reference them directly via `$DEPLOY_ENV`, `$IMAGE_TAG`, etc.

### Trigger API

Programmatic pipeline trigger without webhook. Bypasses job trigger type validation.

```
POST /api/trigger
Content-Type: application/json

{
  "repo_url": "git@gitlab.example.com:group/project.git",
  "job_name": "deploy",
  "ref": "v1.2.3",
  "env": {
    "DEPLOY_ENV": "prod",
    "IMAGE_TAG": "v1.2.3"
  }
}
```

- `repo_url` — must match a registered project's repo URL exactly
- `job_name` — the job to execute (from `neutron.yaml`)
- `ref` — git ref to checkout (tag, branch, or commit SHA)
- `env` — optional key-value pairs injected as environment variables

Works for both GitLab and Codeup platforms. The repo URL is converted to a platform-specific API path to fetch `neutron.yaml` at the given ref.

### Shell Snippets

Snippets are reusable shell scripts stored in MySQL and exposed as `curl | bash` endpoints. Pipelines can reference a snippet by URL instead of duplicating shell code — the raw endpoint (`/s/:name`) returns the script with query parameters prepended as shell variable assignments.

**Database table** `neutron_snippet` (GORM auto-migrated):
- `id` (auto-increment PK), `name` (unique URL slug, `^[a-z0-9][a-z0-9-]*$`), `title` (display name), `content` (shell script body), `description` (free-text), `params` (comma-separated parameter names), `created_at`, `updated_at`

**API endpoints** (registered in `cmd/api/main.go`):
| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/snippets` | List all snippets (ordered by name) |
| `POST` | `/api/snippets` | Create a snippet |
| `GET` | `/api/snippets/:name` | Get a single snippet |
| `PATCH` | `/api/snippets/:name` | Update snippet fields (title, content, description, params; name is immutable) |
| `DELETE` | `/api/snippets/:name` | Delete a snippet |
| `GET` | `/s/:name` | Raw endpoint — returns the script content with query params prepended as shell variables. Intended for `curl -s "URL?PARAM=val" \| bash` |

**SPA frontend** (hash route `#/snippets`):
- List page with search/filter, name/title/description table, "View" button per snippet
- "New Snippet" modal — all fields editable, uses `POST /api/snippets`
- View modal — read-only display of snippet metadata, content, parameters, and `curl | bash` usage with copy button. Has an "Edit" button that opens the edit modal.
- Edit modal — pre-fills all fields except name (disabled). Uses `PATCH /api/snippets/:name`. On save, shows a confirmation alert: "Changes will impact all pipelines using this snippet. Continue?"
- Delete with `confirm()` dialog

**Repository methods** (`internal/repo.go` `Snippet` struct + CRUD): `ListSnippets`, `GetSnippetByName`, `CreateSnippet`, `UpdateSnippet` (partial map with `updated_at`), `DeleteSnippet`.

### Default Pipeline Fallback

A single **global** default `neutron.yaml` used when a repository has no `neutron.yaml` of its own. It applies to all registered projects and is edited in the UI.

**Trigger condition:** only when the platform file API returns **404** (file missing). Both GitLab and Codeup return 404 for a missing file, so detection is platform-agnostic. Auth/network/malformed errors are not caught — they still fail as before.

**Mechanism (two consumers of the same stored default):**
1. **API server** (webhook / trigger, same process) reads the default directly from the DB (`GetSetting`) to decide which jobs to launch and match triggers. `parser.Base.Parse()` wraps the 404 in the sentinel `parser.ErrPipelineNotFound`; `handleWebhook` / `handleTrigger` detect it with `errors.Is` and call `Server.defaultPipeline()` (`cmd/api/server.go`). If no default is configured, the request returns 400. `parseWebhook` populates the webhook-derived fields (trigger, SHA, source URL, …) *before* the pipeline fetch, so they remain valid on a 404 and the fallback pipeline can be substituted.
2. **Runner** (pod, separate process) reads `/repo/neutron.yaml`; if the cloned repo lacks it, `service.NewRunner` calls `fetchDefaultPipeline(apiUrl)` → `GET /api/default-pipeline` (reusing the existing `NEUTRON_API_URL` env, no extra config) and uses the returned content. If none is configured, it `log.Fatal`s (unchanged "no pipeline" semantics). Fallback usage is logged on both the server and runner side.

**Storage:** `neutron_setting` key/value table, key `default_pipeline` (const `internal.SettingDefaultPipeline`). Repository methods: `GetSetting(key)` (empty string when unset), `SetSetting(key, value)` (upsert via `clause.OnConflict`).

**API:** `GET /api/default-pipeline` (shared by SPA and runner), `PUT /api/default-pipeline` (validates the YAML parses to a pipeline with ≥1 job; empty content disables the fallback).

**SPA frontend** (hash route `#/default-pipeline`): a single-page YAML editor (`renderDefaultPipeline`) — a `<textarea>` prefilled from `GET`, a Save button that `PUT`s after a `confirm()` ("This default configuration will be used for all repositories missing a neutron.yaml. Save?").

**Note:** the default is resolved at runtime and **not snapshotted** into `JobSpec`. A rerun of a fallback job therefore uses the *current* default, not the one active at trigger time (acceptable; defaults change rarely).

## Conventions

- Go 1.23.0, Go modules (no vendor)
- Module name: `neutron`
- No test suite or linting config exists yet
- `test.http` contains manual HTTP requests for JetBrains HTTP Client
