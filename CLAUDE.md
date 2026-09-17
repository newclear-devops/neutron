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
- `GET /api/projects/:id/jobs` — lists a project's jobs, **paginated**: `page` (1-based, capped at 1000), `page_size` (default 20, max 100), `job_name` (exact logical job key) and `all` (`1` drops the 7-day recency window and pages the full history) are query params; the response carries `jobs`, `total`, `page`, `page_size`
- `GET /api/projects/:id/job-names` — distinct logical job keys a project has ever run (**no** recency window, so a job idle for longer than the default window stays selectable), used to populate the filter dropdown (impossible to derive client-side once the list is paginated)
- `GET /api/status/:jobName` — job/pod status (JSON). Served from the DB for completed jobs, otherwise from the K8s API. If the K8s Job no longer exists (TTL cleanup) the DB row is served instead, with `stuck: true` when the row never reached a terminal outcome — no more updates will ever arrive for it. Only when neither exists does it 400.
- `POST /api/report/:jobName` — runners push status back to API server for persistence
- `POST /api/report/:jobName/link` — set a test report URL for a job (`{"report_url": "..."}`)
- `POST /api/jobs/:jobName/rerun` — rerun a webhook-created job by recreating an identical K8s Job from its persisted spec (same commit/params/trigger, reports to platform like the original). Only jobs with a stored spec are rerunnable.
- `GET /api/default-pipeline` — returns the global default pipeline (`{"content": "..."}`, empty if unset). Shared by the SPA viewer and the pod-side runner fallback.
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
- `internal/launcher/` — shared K8s Job creation (platform-agnostic). Job names are `neutron-<project>-<job>-<YYMMDDHHMMSS>-<4-char hex>` (see **Job Naming** below).
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
- `neutron_job` (id, project_id, name, **job_name**, status, notify, spec, **params**, completed, completed_at) — `notify` is JSON-encoded `model.Notify`; `spec` is JSON-encoded `model.JobSpec` (rerun snapshot), captured from the job's `neutron.yaml`/webhook at trigger time; `params` is JSON-encoded `model.JobParams` (ref/env/trigger context, see **Trigger Details**). `job_name` is the logical pipeline job key from `neutron.yaml` — it cannot be parsed back out of `name` reliably (both name segments are `[a-z0-9]` and either may be truncated), so it is stored in its own column; empty on rows created before it existed. Indexed on `(project_id, job_name)`.
- `neutron_pod` (id, job_id, pod_name, pod_uid, phase)
- `neutron_job_report` (id, job_name, report_url, created_at) — test report link per job
- `neutron_setting` (key, value, updated_at) — generic key/value store for global config; currently holds the default pipeline under key `default_pipeline` (see **Default Pipeline Fallback**)

### Configuration

Runtime config is `config.yaml` (gitignored). Shape defined by `internal/model/config.go`: host, port, database (MySQL DSN), salt, log_url (external log platform link template with {namespace} and {podName} placeholders, optional), codebase map (url/token/skip_tls_verify per platform: GitLab, Codeup), pod_codebase (pod-side codebase addresses, optional), kubernetes (kube-config path — optional for in-cluster, auto-detected via ServiceAccount; required for out-of-cluster, namespace, git-private-key secret, init-image, checkout-image, image-pull-secrets, **job-ttl-minutes** — see **Job Cleanup (TTL)**, pod-api-url), notify (IM notification config: url, corp_id, app_id, skip_tls_verify). Most fields can be overridden via environment variables (NEUTRON_*).

### Job Cleanup (TTL)

Finished K8s Jobs and their Pods are reclaimed by Kubernetes itself: every pipeline Job is created with `TTLSecondsAfterFinished` set from `kubernetes.job-ttl-minutes` (`NEUTRON_JOB_TTL_MINUTES`). Without it completed Jobs accumulate forever — nothing in Neutron ever deletes them.

The config unit is **minutes** (easier to reason about); `model.KubernetesConfig.EffectiveJobTtlSeconds` converts to the seconds the K8s field requires.

| `job-ttl-minutes` | effect |
|---|---|
| unset or `0` | default **480** = 8h (`model.DefaultJobTtlMinutes`) |
| positive N | delete the Job N minutes after it reaches a terminal state |
| negative | disabled — no `TTLSecondsAfterFinished` field is set, nothing is auto-cleaned |

Configuring less than `reconcileGracePeriod` (2 min) logs a startup warning: the K8s Job could vanish before the reconciler ever reads it.

Deleting the Job cascades to its Pods via owner references. **DB rows are untouched** — history (`neutron_job` / `neutron_pod`) always lives in MySQL, and the status page serves completed jobs from there, so the UI is unaffected.

**Why the default must stay well above the reconciler's grace period:** for a job whose runner never delivered its final report (checkout conflict, image pull failure, OOM), the K8s Job is the *only* source of truth for the outcome. `healStuckCompletedJobs` derives the terminal status from it; once it is deleted, that row can never converge and stays "running" forever (a documented limitation in `cmd/api/reconcile.go`). 8h leaves ample margin over the reconciler's 30s tick + 2min grace.

**Cleaning up Jobs created before this setting existed:** the TTL applies only to Jobs created after the change. Backfill the existing ones by patching them — Kubernetes then deletes anything already past its TTL:

```bash
kubectl get jobs -o name | grep '^job\.batch/neutron-' | \
  xargs -r -n1 -I{} kubectl patch {} --type=merge -p '{"spec":{"ttlSecondsAfterFinished":28800}}'
```

Before doing that, check whether any row within `ListUncompletedJobs`' 7-day window is still uncompleted (`completed = 0`) — those are precisely the rows that would lose their last source of truth. Let the reconciler close them out first (it does so within ~1h of the K8s Job going terminal), then patch.

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

Two channels, sent in parallel (fire-and-forget) on pipeline trigger, rerun, and completion for each job that declares targets:
1. **IM notifications** (`internal/notify/`) — sends to individual users via enterprise IM bot API (`notify.users`). Requires the server-side `notify` config (url/corp_id/app_id); if missing, user targets are skipped with a logged warning.
2. **CCWork group webhooks** (`internal/ccwork/`) — sends to group chats via webhook URLs (`notify.groups`).

Both use structured attachment format with title (head) and body content. A job with no `notify` block sends nothing.

The config is parsed at trigger time and persisted as JSON on `neutron_job.notify`, so the completion handler (`POST /api/report/:jobName`, which only knows the job name) can read the same targets back via the `GetJobByName` lookup it already performs — see `sendJobNotifications` / `notifyJobCompleted` / `marshalNotify` / `parseNotify` in `cmd/api/`.

**Completion semantics:** the runner sends per-step reports plus exactly one job-level final report (`ReportJobFinal`, payload flag `final: true`, sent after the last step). Only the final report triggers the completion notification and `MarkJobCompleted` — per-step reports update status only. Jobs whose K8s Job reaches a terminal state without a final report (checkout conflict, image pull failure, OOM kill) are closed out by a background reconciler (`cmd/api/reconcile.go`, 30s tick): it waits 2 minutes after the K8s terminal time for a late final report, then writes a terminal status from K8s annotations, sends the completion notification with the K8s failure condition as reason, and marks the job completed. Jobs terminal for over an hour are closed silently (no late-notification flood). Note: the runner image and API server should be deployed together — an old runner with a new API server degrades to reconciler-driven completion notifications.

**Reconciler heal pass:** the same 30s reconciler tick also runs a heal step (`healStuckCompletedJobs`) that repairs jobs marked `completed` in the DB but still holding a non-terminal status (no `succeeded`/`failed`). This happens when a `GET /api/status` poll races the runner's final report and writes K8s's transient `active:1` over the terminal outcome before `MarkJobCompleted` runs — the job would otherwise stay stuck as "running" forever. The heal re-derives the terminal outcome from the K8s Job (the source of truth) and writes it back. It is idempotent, sends no notification (the final report already did), and does not `MarkJobCompleted`. A job whose K8s Job has already been TTL-cleaned cannot be healed and remains non-terminal — a known limitation bounded by the 7-day recency window.

### Job Naming

The K8s Job name is `neutron-<project>-<job>-<timestamp>-<suffix>` (`internal/launcher/launcher.go`, `buildJobName`), where:

| segment | source | notes |
|---|---|---|
| `<project>` | repo name extracted from the repo URL (`parser.ExtractRepoName`) | letters/digits only, lowercased, ≤20 chars; **omitted** when nothing usable can be extracted |
| `<job>` | pipeline job key from `neutron.yaml` | lowercased, ≤20 chars; dashes are preserved |
| `<timestamp>` | `YYMMDDHHMMSS` | second granularity; the 3 chars saved versus `YYYYMMDD-HHMMSS` go to the segments above |
| `<suffix>` | 4-char lowercase hex | random |

Projects sharing a default pipeline launch identically named jobs, so without `<project>` their pods are indistinguishable in `kubectl get pods`. The random suffix guarantees that two triggers of the same job within the same second no longer collide (previously the timestamp alone produced `AlreadyExists` on the second trigger).

Both added segments sit *before* / *after* carefully chosen positions: `<timestamp>-<suffix>` must stay at the very end, because consumers locate it from the tail:

- DB recency filters extract the timestamp via `jobTimestampExpr()` (`internal/repo.go`), which counts characters **from the end** and branches on the tail shape. Three layouts have shipped — with the compact timestamp, with the old `YYYYMMDD-HHMMSS` timestamp, and without a random suffix — and all three are normalized back to `YYYYMMDD-HHMMSS` so they compare correctly against `cutoffDays()`. Adding segments at the front is therefore safe and existing rows keep aging out.
- The SPA parses the trailing timestamp for log-expiry/duration via `parseNameTimestamp`, which accepts both timestamp layouts.
- The SPA no longer reverse-engineers the logical key from the name: `getJobLogicalName`/`projectSlug` were deleted once `neutron_job.job_name` existed, because the project and job segments are both `[a-z0-9]` and either can be truncated — the parse could not tell `deploy-to-production-cluster` from its 20-char truncation. The project page lists the generated K8s name as-is and reads the logical key from that column (surfaced in the tooltip).

The whole name is capped at **63 chars** (`jobNameMaxLength`), the K8s label value limit, so the `job-name` label derived from it stays complete. Fixed parts (`neutron` + 4 separators + 12-char timestamp + suffix) take 27 of them, leaving 36 for project+job — each segment is capped at 20, and when they do not both fit the project is trimmed and the job key stays whole.

Because the timestamp omits the century, every reader of the name prefixes `20` when reconstructing a date (`jobTimestampExpr`, `parseNameTimestamp`). That assumption breaks in 2100.

The runner is told its full name via the `FULL_JOB_NAME` env var and reports to `/api/report/<FULL_JOB_NAME>`; the logical `JOB_NAME` (from `neutron.yaml`) is only used to select steps, never to key DB/K8s lookups.

### Job Listings (pagination & filtering)

`GET /api/projects/:id/jobs` and `GET /api/jobs/recent` are paginated (`page_size` ≤ `MaxPageSize`=100, default `DefaultPageSize`=20; `page` ≤ `MaxPage`=1000 — beyond that the OFFSET only buys scanning and `(page-1)*pageSize` overflows; the cap still matters now that `all=1` can ask for an unbounded history) and return `total` alongside the rows. Both used to return **every** row in the 7-day window — including the `status`/`notify`/`spec` text blobs — plus an extra query per row to preload pods, which is the expensive part at a few thousand rows.

Consequences worth remembering:

- **Filtering moved server-side.** The project page's job-name dropdown is built from `GET /api/projects/:id/job-names` (`SELECT DISTINCT job_name`, served by the `idx_job_project_name` index and deliberately unbounded) instead of from the loaded rows, and `job_name` is an exact-match query param. Since the dropdown covers the full history while the list defaults to 7 days, the page carries a `Show all history` toggle that re-requests with `all=1`; without it a job idle for longer than the window would be selectable but always render an empty list.
- **The recent page's search box is server-side too** (`q`). Status words (`running`/`success`/`failed`/…) map onto the flags inside the status JSON; anything else matches the generated name, the job key, the status payload (which carries `repo_url`/`trigger_type`/`webhook_type`), or the owning project's `repo_url`.
- `job_name` is empty on rows created before the column existed; such rows do not appear in the dropdown. The live DB has been backfilled once (parse the K8s name: drop the `neutron-<project>-` prefix and the trailing `-<timestamp>[-<suffix>]`), but rows whose project segment had been trimmed by the length budget could only be recovered approximately. Display does not depend on the column either way — the list renders `name`.

### Trigger Details (ref / env)

The status page shows the run's trigger context — which ref it ran against and which env vars were injected — from `neutron_job.params` (JSON-encoded `model.JobParams`, written at trigger time by both paths):

- **webhook**: ref = `code_ref` (branch/tag name, falling back to the commit SHA for MRs), env = the webhook URL's query params
- **`/api/trigger`**: ref = the `ref` field, env = the `env` object — neither was persisted before, which is why this column exists

`params` is deliberately **not** `JobSpec`: a spec is what makes a job rerunnable, and API-triggered jobs must stay non-rerunnable. The status endpoint exposes it as `params` (plus `jobKey`, the logical job key); both are absent for rows created before this change.

Values are rendered verbatim — the status page has no access control, so anything passed through `env` is visible to everyone who can open it.

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

**SPA frontend** (hash route `#/default-pipeline`): a read-only YAML viewer (`renderDefaultPipeline`) — a `<textarea readonly>` prefilled from `GET`, with a note explaining that updates must go through `PUT /api/default-pipeline` (body `{"content": "..."}`). There is no Save button in the UI.

**Note:** the default is resolved at runtime and **not snapshotted** into `JobSpec`. A rerun of a fallback job therefore uses the *current* default, not the one active at trigger time (acceptable; defaults change rarely).

### Job-Level Fallback (scope & impact)

On top of the file-level fallback above, there is a **job-level** fallback: when a repository *has* its own `neutron.yaml` but that file does not define a particular job, Neutron looks the job up in the global default pipeline by name.

**Semantics:** the repository job always wins for same-named jobs — the whole job definition is overridden, there is **no field-level merge**. A default job is only consulted when the repository's `neutron.yaml` does not define that job name at all.

**Where it applies (and where it does NOT):**

- **Trigger API** (`POST /api/trigger`) — if `job_name` is missing from the repo's `neutron.yaml`, `handleTrigger` (`cmd/api/server.go`) loads the default pipeline and resolves the job from it.
- **Runner** (pod side) — `service.NewRunner` reads `/repo/neutron.yaml`; if the job is absent there, it fetches `GET /api/default-pipeline` and resolves the job from it.
- **Webhook is unchanged** — webhooks are trigger-driven and never name a job, so they still use only the repo's own `neutron.yaml` (or the default *whole-pipeline* on 404). Default jobs do **not** run on repos that have their own `neutron.yaml` via webhooks.

**Shared resolution logic:** both paths resolve jobs through `model.ResolveJob(repo, def, name)` (`internal/model/pipeline.go`), which enforces the "repo wins" rule in one place. The default pipeline is loaded lazily and at most once per request/run; load/YAML errors are logged (not swallowed) so a broken default is distinguishable from a genuinely missing job.

**Impact to keep in mind when editing the default:** a job added to the default pipeline becomes runnable on **every** registered project that does not already define that job name (via trigger API). Editing the default therefore has a wide blast radius across projects — this is broader than the file-level fallback, which only affects projects with *no* `neutron.yaml` at all.

## Conventions

- Go 1.23.0, Go modules (no vendor)
- Module name: `neutron`
- No test suite or linting config exists yet
- `test.http` contains manual HTTP requests for JetBrains HTTP Client
