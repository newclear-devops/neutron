# AGENTS.md

Go 1.23, module name `neutron` (short import paths like `neutron/internal/...`). No vendor dir, no lint config.

**`CLAUDE.md` is the authoritative, detailed architecture reference** (endpoints, DB schema, data flow, default-pipeline fallback, snippets). Read it before making non-trivial changes.

## Commands

```bash
go build ./...    # compiles all three binaries
go test ./...     # real tests exist: cmd/api (fake clientset) + internal/parser; no external deps needed
```

- The Makefile requires a POSIX shell (`rm`, `chmod`, `VAR=x` env prefixes) — it does not work from Windows PowerShell. Use `go build` directly.
- Makefile quirk: builds use `cmd/api/*.go` glob, not the package path. The `*-linux` targets cross-compile **arm64 only** (the dev kind cluster is arm64); CI releases build amd64+arm64.
- `make kind-load` builds all three Docker images and loads them into a kind cluster named `neutron`.

## Things easy to get wrong

- **Three binaries**: `cmd/api` (API server + embedded SPA), `cmd/gitlab-runner`, `cmd/codeup-runner` (run inside K8s pods). Runners get all config via env vars set by the API server.
- **The SPA is a single embedded file** (`cmd/api/static/index.html`, via go:embed). Frontend changes require rebuilding the API binary — no separate dev server.
- **Stale docs — trust code/CLAUDE.md over these**:
  - `README.md` DB schema lists `neutron_notify`/`neutron_ccwebhook` tables that no longer exist (notifications are per-job in `neutron.yaml`, persisted as JSON on `neutron_job.notify`).
  - `docs/testing.md` says the checkout container reuses the job image; it now uses the dedicated `neutron-checkout` image.
- `config.yaml` is gitignored (shape: `internal/model/config.go`); most fields overridable via `NEUTRON_*` env vars.
- **Job completion protocol**: runners report per-step status plus exactly one job-level final report (`final: true`); only the final report triggers completion notifications/`MarkJobCompleted`. A background reconciler (`cmd/api/reconcile.go`) closes out jobs whose pods died without a final report.
- Tests requiring K8s use `k8s.io/client-go/fake` — never hit a real cluster or MySQL.

## Release flow

Pushing a tag `v*` triggers `.github/workflows/release.yml`: builds all three binaries (linux amd64/arm64, CGO disabled) and publishes a GitHub release with checksums.
