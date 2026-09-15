# Neutron - a lightweight pipeline system based on Kubernetes

## Architecture overview

![Concept Arch](./cmd/api/static/arch.svg)

Neutron is a lightweight CI/CD pipeline system built on Kubernetes. It consists of a stateless API server and a MySQL database. Code hosting platforms (GitLab, Codeup) send pipeline requests to the API server via webhooks. Neutron auto-detects the platform, parses these requests, reads pipeline definitions from a `neutron.yaml` file in the repository, and creates Kubernetes Jobs to execute the pipeline steps. GitLab status is reported via commit statuses; Codeup has no status API (logged as TODO).

## How it works

1. A code event (push, tag, or merge request) triggers a webhook to Neutron's `/webhook/:id` endpoint
2. The API server auto-detects the platform (GitLab or Codeup via `X-Codeup-Event` header) and parses the webhook payload
3. Neutron fetches `neutron.yaml` from the Git repository via the platform API
4. For each job whose `trigger` list matches the current trigger type, a Kubernetes Job is created
5. Each K8s Job has two init containers:
   - **checkout** — clones the repository using SSH
   - **init** — copies the platform-specific runner binary from the runner Docker image
6. The main container runs the runner binary, which reads `neutron.yaml`, executes steps sequentially, and reports status (GitLab: commit statuses; Codeup: logs TODO)

## Prerequisites

- Go 1.23+
- MySQL
- Kubernetes cluster with kubectl access
- GitLab and/or Codeup instance with API access token

## Quick start

### 1. Build

```bash
# Build all Docker images (API server + runner) and load into kind
make kind-load

# Or build individually:
make docker-api      # API server image
make docker-runner   # Runner image (contains both gitlab-runner and codeup-runner)

# Local binaries only (no Docker):
make api             # macOS API server
make gitlab          # macOS GitLab runner
make codeup          # macOS Codeup runner
```

### 2. Configure

Create `config.yaml` in the project root:

```yaml
host: "http://your-neutron-host"
port: 8888
database: "user:password@tcp(127.0.0.1:3306)/neutron?charset=utf8mb4&parseTime=True&loc=Local"
salt: "your-random-salt"

# Optional: external log platform URL template
# {namespace} and {podName} are replaced at runtime
# log_url: "https://log.internal.com/view?namespace={namespace}&pod={podName}"

codebase:
  GitLab:
    url: "https://gitlab.example.com"
    token: "your-gitlab-private-token"
  # Codeup:
  #   url: "https://codeup.example.com"
  #   token: "your-codeup-token"

# Optional: pod-side codebase addresses (if pods access codebase differently than the API server)
# pod_codebase:
#   GitLab:
#     url: "http://gitlab.default.svc.cluster.local"
#     token: "your-gitlab-private-token"

kubernetes:
  kube-config: "/path/to/.kube/config"  # optional when deploying in-cluster (auto-detected via ServiceAccount)
  namespace: "default"
  # Optional: how long (minutes) finished pipeline Jobs are kept before
  # Kubernetes deletes them (cascading to their Pods). Default 480 (8 hours).
  # Job history itself lives in MySQL and is never deleted.
  # Set a negative value to disable cleanup entirely.
  job-ttl-minutes: 480
```

### 3. Initialize database

```bash
mysql -u user -p < dds.sql
```

### 4. Run

```bash
./bin/neutron-api
```

## Deploy to Kubernetes

### Build and push images

```bash
# Build
make docker-api
make docker-runner
make docker-checkout

# Tag and push to your registry
docker tag neutron-api:local <registry>/neutron-api:v0.0.2
docker tag neutron-runner:local <registry>/neutron-runner:v0.0.2
docker tag neutron-checkout:local <registry>/neutron-checkout:v0.0.2
docker push <registry>/neutron-api:v0.0.2
docker push <registry>/neutron-runner:v0.0.2
docker push <registry>/neutron-checkout:v0.0.2
```

### RBAC

Neutron uses a dedicated ServiceAccount to create Jobs and query Pods. `k8s-deploy.yaml` includes all RBAC resources:

| Resource | Purpose |
|----------|---------|
| `ServiceAccount/neutron` | Identity for the API server pod |
| `ClusterRole/neutron` | `jobs` (create/get/list/delete), `pods` + `pods/log` (get/list) |
| `ClusterRoleBinding/neutron` | Binds the role to the service account |

### Configure for in-cluster deployment

When running inside K8s, Neutron automatically detects the in-cluster environment and uses the ServiceAccount token. The `kube-config` field is ignored.

```yaml
host: "http://your-neutron-external-address"  # must be reachable from GitLab for webhook callbacks
port: 8888
database: "user:password@tcp(<mysql-host>:3306)/neutron?charset=utf8mb4&parseTime=True&loc=Local"
salt: "your-random-salt"

log_url: "https://log.internal.com/view?namespace={namespace}&pod={podName}"

codebase:
  GitLab:
    url: "https://gitlab.example.com"
    token: "your-gitlab-private-token"
  # Codeup:
  #   url: "https://codeup.example.com"
  #   token: "your-codeup-token"

kubernetes:
  kube-config: ""                               # leave empty for in-cluster, auto-detected via ServiceAccount
  namespace: "default"
  git-private-key: "git-ssh-secret"
  init-image: "<registry>/neutron-runner:v0.0.1"
  checkout-image: "<registry>/neutron-checkout:v0.0.1"
```

### Create secrets and deploy

```bash
# SSH key for git clone
kubectl create secret generic git-ssh-secret --from-file=id_rsa=$HOME/.ssh/id_ed25519

# Update k8s-deploy.yaml image fields, then apply
kubectl apply -f k8s-deploy.yaml

# Verify
kubectl get pods -l app=neutron-api
kubectl logs -l app=neutron-api
```

### Access

The pod does not use `hostNetwork`. Expose the service via one of:

```bash
# Port forward (development)
kubectl port-forward deployment/neutron-api 8888:8888

# NodePort (add to k8s-deploy.yaml)
# LoadBalancer / Ingress (production)
```

## Pipeline configuration

Add a `neutron.yaml` file to the root of your repository:

```yaml
jobs:
  build:
    image: node:18-alpine
    trigger:
      - MR
      - PUSH
    steps:
      - name: install
        cmd: npm install
      - name: build
        cmd: npm run build
      - name: test
        cmd: npm test

  deploy:
    image: alpine:latest
    trigger:
      - TAG
    steps:
      - name: deploy
        cmd: echo "deploying..."
```

### Fields

| Field | Description |
|-------|-------------|
| `jobs.<name>` | Job identifier, used in the K8s Job name |
| `image` | Docker image for the pipeline container |
| `trigger` | List of trigger types that activate this job: `MR`, `TAG`, `PUSH` |
| `steps[].name` | Step name, reported as commit status context |
| `steps[].cmd` | Shell command to execute (runs via `sh -c`, supports pipes, redirects, `&&`) |

Steps run sequentially. If a step fails, all subsequent steps are marked as failed and the process exits.

### Image requirements

Each K8s Job creates three containers, each using a dedicated image:

| Container | Purpose | Image | Configured in |
|-----------|---------|-------|---------------|
| **checkout** (init) | Clone repo, merge source branch for MR | `neutron-checkout` (built-in, includes git + ssh) | `config.yaml` → `kubernetes.checkout-image` |
| **init** (init) | Copy runner binary to shared volume | `neutron-runner` (built-in, busybox + runner binaries) | `config.yaml` → `kubernetes.init-image` |
| **pipeline** (main) | Execute pipeline steps | User-specified image from `neutron.yaml` | `neutron.yaml` → `image` |

**Pipeline image requirements:**

- Steps are executed via `sh -c "<cmd>"`, so the image must have `/bin/sh`
- No need to install `git` or `ssh` — checkout is handled by the dedicated checkout image

## Register a project

Open `http://your-neutron-host/register` in a browser, or use the API:

```bash
curl -X POST http://localhost:8888/api/register \
  -d "webhookType=GitLab" \
  -d "repoUrl=git@gitlab.example.com:group/project.git"

# For Codeup:
curl -X POST http://localhost:8888/api/register \
  -d "webhookType=Codeup" \
  -d "repoUrl=ssh://git@codeup.example.com/group/project.git"
```

The response includes the webhook URL to configure in GitLab/Codeup:

```
POST http://your-neutron-host/webhook/<uuid>
```

Platform is auto-detected from webhook headers (`X-Codeup-Event` → Codeup, otherwise → GitLab).

## API endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/config` | Runtime config (log URL template, namespace, codebase URLs) |
| POST | `/api/register` | Register a project, returns JSON with webhook URL |
| POST | `/webhook/:id` | Receive webhook (GitLab/Codeup auto-detect), create K8s Jobs |
| GET | `/api/status/:jobName` | Job/pod status (JSON, from DB or K8s API). Includes `reportUrl` if set |
| POST | `/api/report/:jobName/link` | Set a test report URL for a job (`{"report_url": "..."}`) |

The frontend is a vanilla JS SPA served from `/` (hash-based routing: `#/`, `#/projects`, `#/project/:id`, `#/status/:jobName`). Pod names on the status page link to an external log platform if `log_url` is configured. When a test report URL is set via the API, a "查看测试报告" button appears on the job detail page.

## Database schema

Tables (auto-migrated by GORM):

- **neutron_project** — registered projects (`id`, `webhook_type`, `repo_url`)
- **neutron_job** — K8s job metadata (`id`, `project_id`, `name`, `status` as JSON, `completed`, `completed_at`)
- **neutron_pod** — pod records per job (`id`, `job_id`, `pod_name`, `pod_uid`, `phase`)
- **neutron_notify** — IM notification recipients per project (`id`, `project_id`, `user_id`)
- **neutron_ccwebhook** — CCWork group webhook URLs per project (`id`, `project_id`, `webhook_url`, `description`)
- **neutron_job_report** — test report link per job (`id`, `job_name`, `report_url`, `created_at`)

## Project structure

```
cmd/
  api/              # API server (Gin framework)
    main.go
    static/         # embedded SPA (index.html) + CSS + architecture diagram
  gitlab-runner/    # GitLab runner binary (runs inside K8s pods)
    main.go
    reporter.go     # reports status to GitLab commit statuses
  codeup-runner/    # Codeup runner binary (runs inside K8s pods)
    main.go
    reporter.go     # no-op reporter (Codeup has no status API)
internal/
  gitlab/
    parser.go       # GitLab webhook parsing + neutron.yaml fetching
  codeup/
    parser.go       # Codeup webhook parsing + neutron.yaml fetching
  launcher/
    launcher.go     # shared K8s Job creation (platform-agnostic)
  model/
    config.go       # application config struct
    pipeline.go     # Pipeline/Job/Step/RunnerConfig models + interfaces
  service/
    runner.go       # step execution engine
  repo.go           # MySQL data access layer
```
