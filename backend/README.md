# graphify-service backend (Go control plane)

API-first Go monorepo around the Python Graphify engine. Authoritative spec:
[`../docs/FEATURES-BACKEND-SERVICE.md`](../docs/FEATURES-BACKEND-SERVICE.md).

> **Status: Phase 1 — API + metadata.** The `api` component is implemented
> (deterministic repository IDs, filesystem metadata store, `POST`/list/status
> REST + health/readiness). `cloner` (Phase 2) and `worker` (Phase 3) are
> buildable placeholders. NATS, artifact downloads, MCP, and OIDC are later phases.

## Monorepo layout

One Go module (`github.com/marcellodesales/graphify-service/backend`), three
components each with their own `main` + Dockerfile, sharing `internal/`.

```
backend/
├── api/       main.go + Dockerfile   # REST + MCP control plane (Phase 1 ✅)
├── cloner/    main.go + Dockerfile   # NATS clone worker (Phase 2 stub)
├── worker/    main.go + Dockerfile   # graphify worker, built FROM the graphify image (Phase 3 stub)
└── internal/
    ├── config/      # env-driven configuration + validation
    ├── telemetry/   # slog JSON logger
    ├── giturl/      # URL parse/canonicalize + ref validation
    ├── repository/  # deterministic id, state machine, metadata, filesystem store
    └── api/         # REST handlers, middleware, routes
```

## Build & test

> This repo's environment sets `GO111MODULE=off`; prefix Go commands with
> `GO111MODULE=on` (or export it) so the module is used.

```bash
cd backend
export GO111MODULE=on
go build ./...       # builds api, cloner, worker + internal
go vet ./...
go test ./...        # giturl, repository, api
```

## Run the API locally

```bash
cd backend
export GO111MODULE=on
export GRAPHIFY_REPOS_ROOT="$PWD/.data/repos"
export GRAPHIFY_HTTP_ADDR="127.0.0.1:8080"
export GRAPHIFY_AUTH_MODE=none          # or: static + GRAPHIFY_API_TOKEN=...
go run ./api

# submit (202 + deterministic sha256 id)
curl -s -X POST http://127.0.0.1:8080/api/v1/repositories \
  -H 'Content-Type: application/json' \
  -d '{"githubRepoUrl":"https://github.com/octocat/Hello-World"}'
```

## Docker images

Each component has its own Dockerfile; build context is `./backend`:

```bash
# from repo root
docker build -f backend/api/Dockerfile    -t marcellodesales/graphify-api    ./backend
docker build -f backend/cloner/Dockerfile -t marcellodesales/graphify-cloner ./backend

# worker is built FROM the graphify image (needs the graphify binary) — build that first:
docker compose build graphify
docker build -f backend/worker/Dockerfile -t marcellodesales/graphify-worker ./backend
```

Or via the root `docker-compose.yaml` (services `graphify-api`, `graphify-cloner`,
`graphify-worker`, plus `nats`):

```bash
# from repo root
docker compose build graphify            # base image for the worker
docker compose up --build graphify-api   # API on http://localhost:8080
```

## CI/CD

Images are published by the shared vionix reusable workflow via per-component
caller workflows under `.github/workflows/docker-multiarch-cicd-{backend,cloner,worker}.yaml`
(and `docker-multiarch-cicd.yaml` for the Python `graphify` image). See the
`vionix-docker-cicd` skill for the conventions and the one-image-per-repo caveat.

## Selector semantics (spec §6.2)

- neither `githubRef` nor `githubSha` → `default` (remote default branch)
- `githubRef` only → `ref`; `githubSha` only → `sha`; both → `400`

Repository ID = `SHA-256(canonicalURL "\n" selectorType "\n" selectorValue)`.
