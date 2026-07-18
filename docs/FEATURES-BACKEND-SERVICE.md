# Graphify-as-a-Service: Technical Specification

**Target repository:** `marcellodesales/graphify-service`
**Implementation language:** Go for the service plane; existing Python Graphify remains the graph engine.
**Status:** Proposed implementation specification based on analysis of:

- `marcellodesales/graphify-service`
- `marcellodesales/cloner`
- `marcellodesales/kubernetes-mcp-server`
- `marcellodesales/vault-mcp-server`

No repository changes or pull request were created because the coding-agent session failed to start.

> **Requirement status:** This document is the authoritative requirements/spec for the
> Graphify-as-a-Service backend. Implementation happens in this repository
> (`github.com/marcellodesales/graphify-service`) under a Go service plane. Refer back
> to this file for every implementation decision.

---

## 1. Executive summary

Graphify-as-a-Service should be an API-first Go service that:

1. Accepts a Git repository URL and optional Git ref or commit SHA.
2. Immediately returns a deterministic SHA-256 repository/job ID.
3. Publishes a clone request to NATS JetStream.
4. Clones only the requested revision into a shared volume.
5. Publishes a CloudEvent after cloning.
6. Runs Graphify asynchronously against that repository.
7. Stores Graphify outputs alongside repository metadata.
8. Exposes status and approved artifacts through REST.
9. Exposes the same operations as MCP tools over Streamable HTTP.
10. Supports OAuth-aware MCP discovery and secure bearer authentication.

The initial implementation should use the filesystem as the authoritative metadata and artifact store. NATS JetStream provides durable asynchronous delivery; no SQL database is needed for the first version.

---

## 2. Findings from the existing repositories

### 2.1 `graphify-service`

The repository already contains:

- The full Python Graphify implementation.
- A Graphify CLI entry point:
  - `graphify = graphify.__main__:main`
  - `graphify-mcp = graphify.serve:_main`
- A Python MCP server supporting stdio and HTTP.
- A Docker image with a `graphify` entry point.
- A Compose setup containing:
  - Neo4j.
  - A one-shot Graphify CLI service.
  - A long-running `graphify watch .` service.
- A shared volume currently mounted as:

```yaml
- ./data:/workspace
```

The service already generates these principal artifacts:

- `graphify-out/graph.json`
- `graphify-out/graph.html`
- `graphify-out/GRAPH_REPORT.md`

#### Important issue in the current Dockerfile

The current Dockerfile installs:

```text
.[neo4j,watch]
```

That is sufficient for CLI/watch usage, but not for Graphify’s Python HTTP MCP server. The `mcp` optional dependency would also be required if that Python server remains exposed.

For Graphify-as-a-Service, however, the Go service should own the public REST and MCP interfaces. The Python MCP server does not need to be exposed publicly in the first implementation.

### 2.2 `cloner`

The `cloner` repository provides a useful model for:

- Parsing Git URLs.
- Organizing repositories by host, organization, and repository.
- Supporting HTTPS and SSH repositories.
- Resolving SSH private-key paths.
- Keeping a configurable clone base directory.

Its storage convention is approximately:

```text
<base>/<git-host>/<organization>/<repository>
```

Examples:

```text
github.com/marcellodesales/cloner
gitlab.com/group/subgroup/repository
```

#### What should be reused conceptually

- Repository URL normalization.
- Host/organization/repository directory structure.
- Explicit clone-root configuration.
- SSH key references supplied as paths rather than raw key content.

#### What should not be copied unchanged

The existing implementation predates the service requirements and does not provide all of the following:

- Shallow ref-only cloning.
- SHA-only fetch semantics.
- Atomic clone publication.
- Durable asynchronous events.
- Deterministic request IDs.
- Symlink-safe credential confinement.
- `known_hosts` enforcement.
- API-safe error handling.
- Idempotent distributed workers.

These capabilities should be implemented afresh in modern Go.

### 2.3 `kubernetes-mcp-server`

This is the strongest architectural reference for a production Go MCP service.

Useful patterns include:

- Standard Go project layout under `cmd/` and `pkg/`.
- Streamable HTTP MCP transport at `/mcp`.
- Health, readiness, metrics, and graceful shutdown.
- Server card:
  - `/.well-known/mcp/server-card.json`
- OAuth authorization-server metadata:
  - `/.well-known/oauth-authorization-server`
- OAuth protected-resource metadata:
  - `/.well-known/oauth-protected-resource`
  - `/.well-known/oauth-protected-resource/mcp`
- Public MCP discovery methods with protected tool execution.
- Structured tool definitions and output schemas.
- Non-root container execution.
- Stateless MCP mode suitable for horizontally scaled deployment.
- Request middleware, body limits, rate limiting, metrics, and timeouts.

The complete Kubernetes toolset architecture is more complex than this project needs initially. Graphify should use the same principles but with fewer abstraction layers.

### 2.4 `vault-mcp-server`

This repository supplies additional useful patterns:

- Streamable HTTP MCP server.
- OAuth 2.1 and PKCE discovery.
- Server-card metadata.
- Bearer middleware.
- Public MCP discovery while protecting `tools/call`.
- Structured JSON logging.
- CORS configuration.
- Environment-driven HTTP mode.
- TLS requirements for non-localhost deployments.
- Docker Compose examples.
- Graceful shutdown and long-lived HTTP connection handling.

Graphify should not inherit Vault-specific login or credential propagation. It should implement service authentication independently.

---

## 3. Key architecture decision

### 3.1 Components

The system should contain five logical components:

1. **Graphify API**
   - REST API.
   - MCP server.
   - OAuth/resource metadata.
   - Filesystem metadata reads.
   - Event publication.

2. **Clone worker**
   - Consumes clone jobs from NATS.
   - Performs secure, shallow Git operations.
   - Updates metadata.
   - Publishes cloned/failed events.

3. **Graphify worker**
   - Consumes cloned events.
   - Runs `graphify extract`.
   - Updates metadata and artifact inventory.
   - Publishes ready/failed events.

4. **NATS JetStream**
   - Durable event transport.
   - Work-queue semantics.
   - CloudEvents-compatible payloads.

5. **Shared repository volume**
   - Repository contents.
   - State metadata.
   - Graphify artifacts.
   - Temporary work directories.

Neo4j may remain optional. It is not required for the initial filesystem-backed Graphify workflow.

### 3.2 Recommended process and image model

One Go codebase should produce one binary with separate modes:

```text
graphify-service api
graphify-service clone-worker
graphify-service graphify-worker
```

This allows:

- Shared models and filesystem logic.
- Independent scaling.
- Separate security boundaries.
- Small operational footprint.
- One release version.

#### API image

Contains:

- Go binary only.
- No Git executable.
- No SSH private-key access.
- Read/write metadata access.
- Read artifact access.
- NATS access.

#### Clone-worker image

Contains:

- Go binary.
- `git`.
- `openssh-client`.
- CA certificates.
- Read-only SSH credential mounts.
- Writable repository volume.

#### Graphify-worker image

The recommended implementation is a worker image based on the existing Graphify runtime image:

```dockerfile
FROM graphify-runtime AS graphify-worker

COPY --from=go-builder /out/graphify-service /usr/local/bin/graphify-service

ENTRYPOINT ["/usr/local/bin/graphify-service"]
CMD ["graphify-worker"]
```

This is preferable to mounting the Docker socket and launching Graphify containers dynamically.

The Graphify worker then invokes the existing `graphify` executable directly, without a shell:

```text
graphify extract /graphify-service/repos/<id>/repository
```

This satisfies the requirement to use the Graphify image while keeping the Docker socket out of the API and worker containers.

---

## 4. Shared storage design

### 4.1 Host/container mount

The required host mount is:

```yaml
volumes:
  - ./data/repos:/graphify-service/repos
```

It must be mounted by:

- API service.
- Clone worker.
- Graphify worker.

The API may eventually use a read-only mount if metadata mutations are delegated elsewhere. In the first version, the API needs write access to create initial queued metadata.

### 4.2 Repository job layout

Each request gets a deterministic hash ID:

```text
data/repos/
└── <sha256-id>/
    ├── metadata.json
    ├── repository/
    │   ├── .git/
    │   ├── source files...
    │   └── graphify-out/
    │       ├── graph.json
    │       ├── graph.html
    │       ├── GRAPH_REPORT.md
    │       └── ...
    ├── logs/
    │   ├── clone.log
    │   └── graphify.log
    └── locks/
        ├── clone.lock
        └── graphify.lock
```

Temporary directories should be siblings of the final directory:

```text
data/repos/.tmp/<id>-<random>/
```

The clone worker must clone into the temporary directory and atomically rename it to the final `repository/` directory after successful checkout and validation.

### 4.3 Human-readable source identity

Metadata should preserve:

- Git host.
- Owner/group path.
- Repository name.
- Normalized URL.
- Requested ref or SHA.
- Resolved SHA.

The authoritative storage lookup remains the hash ID. URL-derived components must not directly control arbitrary filesystem paths.

A non-authoritative display field can use:

```text
github.com/marcellodesales/graphify-service
```

---

## 5. Deterministic repository ID

### 5.1 Canonical identity

The repository ID should be:

```text
SHA-256(canonical_repository_url + "\n" + selector_type + "\n" + selector_value)
```

Where:

- `canonical_repository_url` excludes:
  - Passwords.
  - Tokens.
  - URL userinfo.
  - Query parameters.
  - Fragments.
- `selector_type` is one of:
  - `default`
  - `ref`
  - `sha`
- `selector_value` is:
  - Empty for default.
  - The exact normalized ref for ref requests.
  - Lowercase commit SHA for SHA requests.

Example canonical input:

```text
https://github.com/marcellodesales/graphify-service.git
ref
refs/heads/main
```

### 5.2 URL normalization

The service should accept:

```text
https://github.com/owner/repo
https://github.com/owner/repo.git
ssh://git@github.com/owner/repo.git
git@github.com:owner/repo.git
```

Canonical forms should normalize to either HTTPS or SSH based on the original transport:

```text
https://github.com/owner/repo.git
ssh://git@github.com/owner/repo.git
```

Normalization rules:

- Lowercase the host.
- Remove default ports.
- Clean duplicate separators.
- Strip one trailing slash.
- Ensure `.git` suffix.
- Preserve case-sensitive repository paths unless the host is explicitly known to be case-insensitive.
- Reject URL userinfo containing passwords or tokens.
- Reject query strings and fragments.
- Reject local paths and `file://`.
- Reject unsupported protocols.
- Optionally enforce an allowed-host list.

### 5.3 Idempotency

The same canonical repository and selector must always return the same ID.

If a request already exists:

- `queued`, `cloning`, `cloned`, `graphifying`:
  - Return the existing ID and state.
  - Do not publish duplicate work unless metadata indicates a lost or stale job.
- `ready`:
  - Return the existing ID and artifact links.
- `failed`:
  - Default behavior should return the existing failure.
  - A later API version may add an explicit retry endpoint.

Duplicate NATS deliveries must be safe.

---

## 6. API specification

### 6.1 API conventions

Base path:

```text
/api/v1
```

JSON content type:

```text
application/json
```

Error envelope:

```json
{
  "error": {
    "code": "invalid_request",
    "message": "githubSha must be a hexadecimal commit identifier",
    "requestId": "01J..."
  }
}
```

Every response should include:

- `X-Request-ID`
- Safe structured logs using the same request ID.

Request bodies should be limited, initially to 1 MiB.

### 6.2 Submit repository

```text
POST /api/v1/repositories
```

Request:

```json
{
  "githubRepoUrl": "git@github.com:private-org/private-repo.git",
  "githubRef": "refs/heads/main",
  "githubSha": "",
  "sshKeyRef": "github-production"
}
```

Fields:

| Field | Required | Meaning |
|---|---:|---|
| `githubRepoUrl` | Yes | HTTPS or SSH Git URL |
| `githubRef` | No | Branch, tag, or full ref |
| `githubSha` | No | Commit SHA; authoritative when present |
| `sshKeyRef` | No | Name of a mounted SSH credential |
| `force` | No | Not recommended for v1; retries should use a separate operation |

#### Selector rules

- Neither ref nor SHA:
  - Clone the remote default branch with depth 1.
- Ref only:
  - Fetch and check out only that ref with depth 1.
- SHA only:
  - Fetch and detached-checkout only that commit.
- Ref and SHA:
  - Reject the request with `400 Bad Request` in v1.
  - This avoids unclear semantics and makes hash identity stable.

#### Successful response

Status:

```text
202 Accepted
```

Body:

```json
{
  "id": "f5ec1ca6d93ebf4e71dbd3221efee1690bbdc6719afd3c8f4df7ba765305ab61",
  "status": "queued",
  "statusUrl": "/api/v1/repositories/f5ec1ca6d93ebf4e71dbd3221efee1690bbdc6719afd3c8f4df7ba765305ab61",
  "artifactsUrl": "/api/v1/repositories/f5ec1ca6d93ebf4e71dbd3221efee1690bbdc6719afd3c8f4df7ba765305ab61/artifacts"
}
```

### 6.3 List repositories

```text
GET /api/v1/repositories
```

Query parameters:

- `status`
- `host`
- `owner`
- `limit`
- `cursor`

Response:

```json
{
  "repositories": [
    {
      "id": "f5ec...",
      "source": {
        "host": "github.com",
        "owner": "marcellodesales",
        "repository": "graphify-service"
      },
      "selector": {
        "type": "ref",
        "value": "refs/heads/main"
      },
      "status": "ready",
      "resolvedSha": "51d5269caadb02c4976acbaf2207c82cd05246de",
      "createdAt": "2026-07-17T22:00:00Z",
      "updatedAt": "2026-07-17T22:03:12Z"
    }
  ],
  "nextCursor": null
}
```

The implementation can scan metadata files for the first version. Pagination prevents unbounded responses.

### 6.4 Get repository status

```text
GET /api/v1/repositories/{id}
```

Response:

```json
{
  "id": "f5ec...",
  "status": "graphifying",
  "source": {
    "normalizedUrl": "https://github.com/marcellodesales/graphify-service.git",
    "host": "github.com",
    "owner": "marcellodesales",
    "repository": "graphify-service"
  },
  "selector": {
    "type": "sha",
    "value": "51d5269caadb02c4976acbaf2207c82cd05246de"
  },
  "resolvedSha": "51d5269caadb02c4976acbaf2207c82cd05246de",
  "timestamps": {
    "createdAt": "2026-07-17T22:00:00Z",
    "cloneStartedAt": "2026-07-17T22:00:01Z",
    "cloneFinishedAt": "2026-07-17T22:00:06Z",
    "graphifyStartedAt": "2026-07-17T22:00:07Z",
    "graphifyFinishedAt": null,
    "updatedAt": "2026-07-17T22:00:07Z"
  },
  "links": {
    "self": "/api/v1/repositories/f5ec...",
    "artifacts": "/api/v1/repositories/f5ec.../artifacts",
    "downloadZip": "/api/v1/repositories/f5ec.../download?format=zip"
  }
}
```

### 6.5 List artifacts

```text
GET /api/v1/repositories/{id}/artifacts
```

Response:

```json
{
  "id": "f5ec...",
  "status": "ready",
  "artifacts": [
    {
      "name": "graph.json",
      "path": "graphify-out/graph.json",
      "mediaType": "application/json",
      "size": 842371,
      "sha256": "..."
    },
    {
      "name": "graph.html",
      "path": "graphify-out/graph.html",
      "mediaType": "text/html; charset=utf-8",
      "size": 1283012,
      "sha256": "..."
    },
    {
      "name": "GRAPH_REPORT.md",
      "path": "graphify-out/GRAPH_REPORT.md",
      "mediaType": "text/markdown; charset=utf-8",
      "size": 18291,
      "sha256": "..."
    }
  ]
}
```

Artifact inventory should be generated by the Graphify worker and recorded in metadata. The API should not trust arbitrary metadata paths without revalidating them against an allowlist.

### 6.6 Download artifacts

```text
GET /api/v1/repositories/{id}/download?format=zip
```

Allowed initial format:

```text
zip
```

Approved files:

```text
graphify-out/graph.json
graphify-out/graph.html
graphify-out/GRAPH_REPORT.md
```

Possible future formats:

- `json`
- `html`
- `report`
- `graphml`
- `svg`

Security requirements:

- Do not include `.git`.
- Do not include source code.
- Do not include logs unless explicitly approved later.
- Do not follow symlinks.
- Revalidate every path after resolving it.
- Set safe ZIP entry names.
- Stream the archive rather than buffering it entirely.
- Limit total downloadable size.
- Use a stable filename such as:

```text
graphify-<id-prefix>.zip
```

### 6.7 Health and readiness

```text
GET /healthz
GET /readyz
```

`/healthz` reports that the process is alive.

`/readyz` should verify:

- Repository root exists and is writable where required.
- NATS connection is active.
- Required JetStream streams and consumers exist.
- For the clone worker:
  - `git` is available.
  - `known_hosts` configuration is valid if SSH is enabled.
- For the Graphify worker:
  - `graphify` is executable.

Health and readiness must not require OAuth.

---

## 7. Metadata and state machine

### 7.1 States

```text
queued
cloning
cloned
graphifying
ready
failed
```

Recommended future states:

```text
clone_failed
graphify_failed
cancelled
expired
deleting
```

For v1, `failed` should include a `stage` field.

### 7.2 Valid transitions

```text
queued       -> cloning
cloning      -> cloned
cloning      -> failed
cloned       -> graphifying
graphifying  -> ready
graphifying  -> failed
failed       -> queued       only through an explicit retry operation
```

Workers must use compare-and-transition semantics. A duplicate event should not cause:

- `ready -> graphifying`
- Two simultaneous clones.
- Two simultaneous Graphify executions.

### 7.3 Metadata schema

```json
{
  "schemaVersion": 1,
  "id": "f5ec...",
  "status": "ready",
  "stage": "complete",
  "source": {
    "normalizedUrl": "https://github.com/marcellodesales/graphify-service.git",
    "host": "github.com",
    "ownerPath": "marcellodesales",
    "repository": "graphify-service",
    "transport": "https",
    "private": false,
    "sshKeyRef": ""
  },
  "selector": {
    "type": "ref",
    "value": "refs/heads/main"
  },
  "resolvedSha": "51d5269caadb02c4976acbaf2207c82cd05246de",
  "attempts": {
    "clone": 1,
    "graphify": 1
  },
  "timestamps": {
    "createdAt": "2026-07-17T22:00:00Z",
    "updatedAt": "2026-07-17T22:03:12Z",
    "cloneStartedAt": "2026-07-17T22:00:01Z",
    "cloneFinishedAt": "2026-07-17T22:00:06Z",
    "graphifyStartedAt": "2026-07-17T22:00:07Z",
    "graphifyFinishedAt": "2026-07-17T22:03:12Z"
  },
  "artifacts": [],
  "failure": null
}
```

Never persist:

- Raw private keys.
- Passphrases.
- Tokens.
- Authenticated HTTPS URLs.
- Full secret-bearing environment variables.

### 7.4 Atomic metadata updates

Update process:

1. Acquire a job-specific lock.
2. Read current metadata.
3. Validate the transition.
4. Write a complete new JSON document to a temporary file.
5. `fsync` the temporary file.
6. Rename it over `metadata.json`.
7. Optionally `fsync` the containing directory.
8. Release the lock.

The metadata file must never be modified in place.

---

## 8. NATS JetStream event design

### 8.1 Why NATS JetStream

NATS is appropriate because it is:

- Lightweight.
- CNCF graduated.
- Easy to run in Docker Compose.
- Durable with JetStream.
- Suitable for work queues.
- Compatible with Kubernetes deployment later.
- Easy to bridge to Knative Eventing.

“KEvents” should be treated as Knative Eventing integration. CloudEvents envelopes provide the cleanest future bridge.

### 8.2 Stream

Recommended stream:

```text
GRAPHIFY_JOBS
```

Subjects:

```text
graphify.repository.clone.requested.v1
graphify.repository.cloned.v1
graphify.repository.clone.failed.v1
graphify.repository.graph.started.v1
graphify.repository.graph.ready.v1
graphify.repository.graph.failed.v1
```

Suggested retention:

- Work-queue semantics for requested/cloned work subjects.
- Interest or limits retention for lifecycle/audit events.
- Alternatively, use two streams:
  - `GRAPHIFY_WORK`
  - `GRAPHIFY_EVENTS`

Two streams provide cleaner retention but slightly increase setup complexity.

### 8.3 CloudEvent envelope

```json
{
  "specversion": "1.0",
  "id": "01J4...",
  "source": "urn:graphify-service:api",
  "type": "com.graphify.repository.clone.requested.v1",
  "subject": "repository/f5ec...",
  "time": "2026-07-17T22:00:00Z",
  "datacontenttype": "application/json",
  "dataschema": "https://graphify.example/schemas/repository-clone-requested-v1.json",
  "data": {
    "repositoryId": "f5ec...",
    "selectorType": "ref",
    "selectorValue": "refs/heads/main"
  }
}
```

Do not place these in event payloads:

- SSH key bytes.
- Secret tokens.
- Credential file paths outside a safe named reference.
- Unredacted authenticated URLs.

The worker reads complete non-secret request metadata from the shared metadata file using `repositoryId`.

### 8.4 Consumers

Clone-worker consumer:

```text
graphify-clone-workers-v1
```

Graphify-worker consumer:

```text
graphify-graph-workers-v1
```

Requirements:

- Durable consumers.
- Explicit acknowledgement.
- Acknowledge only after durable metadata and event changes.
- Bounded delivery attempts.
- Exponential backoff.
- Dead-letter or terminal-failure event after the maximum.
- `Nats-Msg-Id` set to an idempotency key.

Suggested message IDs:

```text
clone-request:<repository-id>
repository-cloned:<repository-id>:<resolved-sha>
graph-ready:<repository-id>:<resolved-sha>
```

### 8.5 Delivery behavior

For clone processing:

1. Receive clone-requested event.
2. Load metadata.
3. If state is `cloned`, `graphifying`, or `ready`, acknowledge it as a duplicate.
4. If state is `cloning`, check lock ownership and staleness.
5. Transition `queued -> cloning`.
6. Clone.
7. Persist `cloned` state.
8. Publish cloned event.
9. Acknowledge original event.

Publishing and state mutation cannot be fully transactional across the filesystem and NATS. Idempotency closes that gap:

- If metadata is saved but event publication fails, redelivery sees `cloned` and republishes the deterministic cloned event.
- If event publication succeeds but acknowledgement fails, duplicate delivery sees completed state and safely acknowledges.

---

## 9. Git clone implementation

### 9.1 Recommended implementation

Use the system `git` executable through `os/exec.CommandContext`.

Reasons:

- Correct support for SHA-only shallow fetches.
- Mature protocol handling.
- Better compatibility with enterprise Git servers.
- Familiar SSH configuration.
- Easier reproduction of command behavior.

Never invoke Git through a shell.

### 9.2 Default-branch shallow clone

When neither ref nor SHA is supplied:

```text
git clone --depth 1 --single-branch --no-tags <url> <temporary-directory>
git -C <temporary-directory> rev-parse HEAD
```

The remote chooses its default branch.

### 9.3 Explicit branch or tag

For a branch or tag:

```text
git clone --depth 1 --single-branch --no-tags --branch <ref> <url> <temporary-directory>
git -C <temporary-directory> rev-parse HEAD
```

However, full refs may not work uniformly with `--branch`. A more controlled implementation is:

```text
git init <temporary-directory>
git -C <temporary-directory> remote add origin <url>
git -C <temporary-directory> fetch --depth 1 --no-tags origin <ref>
git -C <temporary-directory> checkout --detach FETCH_HEAD
git -C <temporary-directory> rev-parse HEAD
```

The controlled fetch path is recommended because it treats branches and tags consistently and avoids fetching unrelated history.

### 9.4 SHA-only clone

For a provided SHA:

```text
git init <temporary-directory>
git -C <temporary-directory> remote add origin <url>
git -C <temporary-directory> fetch --depth 1 --no-tags origin <sha>
git -C <temporary-directory> checkout --detach FETCH_HEAD
git -C <temporary-directory> rev-parse HEAD
```

The resolved SHA must equal the requested SHA, allowing a shortened request SHA only if Git resolves it unambiguously.

Some servers reject fetching unadvertised commit SHAs. The API must report a clear terminal failure:

```text
The remote does not allow fetching the requested commit directly. Supply a branch or tag containing the commit, or configure the Git server to allow reachable SHA fetches.
```

The service must not silently fall back to fetching all branches or full history.

### 9.5 Ref validation

Reject refs containing:

- NUL bytes.
- Newlines.
- Leading `-`.
- `..`
- `@{`
- Backslashes.
- Control characters.
- Shell metacharacters are not intrinsically dangerous without a shell, but should still be rejected where invalid in Git refs.

Use:

```text
git check-ref-format --branch <value>
```

or equivalent strict validation. Full refs require a corresponding validation path.

### 9.6 Timeouts and limits

Recommended defaults:

| Operation | Timeout |
|---|---:|
| Git clone/fetch | 10 minutes |
| Git metadata commands | 30 seconds |
| Graphify execution | 60 minutes |
| API write request | 30 seconds |
| API read header | 10 seconds |

Configure through environment variables.

Disk quotas are difficult to enforce portably inside Compose. The first version should support:

- Maximum clone duration.
- Optional preflight disk-space threshold.
- Maximum artifact ZIP size.
- Operational documentation recommending filesystem quotas in production.

---

## 10. Private repository and SSH security

### 10.1 API credential field

The API must accept a credential reference, not key bytes:

```json
{
  "sshKeyRef": "github-production"
}
```

Credential root:

```text
/run/secrets/graphify-ssh
```

Expected files:

```text
/run/secrets/graphify-ssh/
├── github-production
├── github-development
└── known_hosts
```

### 10.2 Key confinement

To resolve `github-production`:

1. Require a simple name, not a path.
2. Reject `/`, `\`, `.`, `..`, NUL, and control characters.
3. Construct the candidate under the configured credential root.
4. Evaluate symlinks.
5. Verify the final path remains inside the evaluated credential root.
6. Require a regular file.
7. Check restrictive permissions where possible.
8. Never return the resolved path to API clients.

Symlink escape must be tested explicitly.

### 10.3 SSH command

Set `GIT_SSH_COMMAND` only for the Git child process:

```text
ssh -i /run/secrets/graphify-ssh/github-production \
  -o IdentitiesOnly=yes \
  -o BatchMode=yes \
  -o UserKnownHostsFile=/run/secrets/graphify-ssh/known_hosts \
  -o StrictHostKeyChecking=yes
```

Construct the value safely or use an executable wrapper with fixed arguments. Do not set:

```text
StrictHostKeyChecking=no
```

Do not copy private keys into repository storage.

Passphrase-protected keys require an external agent or secret integration and should be documented as unsupported in the first version unless `SSH_AUTH_SOCK` support is explicitly added.

### 10.4 HTTPS private repositories

Raw access tokens must not be accepted in `githubRepoUrl`.

A future implementation may support:

- A named HTTPS credential reference.
- GitHub App installation tokens.
- Workload identity.
- An external secret provider.

For v1, private repositories should use mounted SSH deploy keys.

---

## 11. Graphify worker

### 11.1 Command

The initial command should be:

```text
graphify extract /graphify-service/repos/<id>/repository
```

Potential flags:

```text
--force
--timing
```

Use `--force` only when the worker is intentionally rebuilding an existing graph. First-time processing should not need it.

### 11.2 Execution flow

1. Consume `repository.cloned`.
2. Load metadata.
3. Verify the resolved SHA matches the event.
4. Acquire the graphification lock.
5. If state is `ready`, acknowledge the duplicate.
6. Transition `cloned -> graphifying`.
7. Publish a graph-started event.
8. Execute Graphify without a shell.
9. Capture bounded output.
10. Validate expected artifacts.
11. Compute artifact sizes and SHA-256 values.
12. Persist artifact inventory.
13. Transition to `ready`.
14. Publish a graph-ready event.
15. Acknowledge the cloned event.

On failure:

1. Capture a safe error summary.
2. Persist `failed`, stage `graphify`.
3. Publish graph-failed.
4. Acknowledge terminal failures or trigger bounded retry for transient failures.

### 11.3 Output capture

Store worker logs under:

```text
logs/clone.log
logs/graphify.log
```

Requirements:

- Bound each captured log, for example to 10 MiB.
- Keep the last portion if truncation is required.
- Redact repository credentials and environment secrets.
- Do not expose logs through the artifact-download endpoint.
- Include command exit code and duration in metadata.

### 11.4 Idempotency

Before running Graphify, verify:

- Current state.
- Lock ownership.
- Resolved commit SHA.
- Existing artifact inventory.
- Whether artifacts match the same resolved SHA.

A stale `graphify-out` from another SHA must never be returned as current.

The worker can write a build manifest:

```json
{
  "repositoryId": "f5ec...",
  "resolvedSha": "51d5269...",
  "graphifyVersion": "0.9.18",
  "startedAt": "...",
  "finishedAt": "..."
}
```

---

## 12. MCP server

### 12.1 Transport

Expose Streamable HTTP at:

```text
/mcp
```

Default deployment mode should be stateless unless server-initiated notifications are needed.

Use a maintained Go MCP SDK. Based on the reference projects, either of these is viable:

- Official `modelcontextprotocol/go-sdk`.
- `mark3labs/mcp-go`.

The official Go SDK is preferable for alignment with the current MCP specification.

### 12.2 Tools

#### `clone_repository`

Input:

```json
{
  "githubRepoUrl": "https://github.com/owner/repository.git",
  "githubRef": "refs/heads/main",
  "githubSha": "",
  "sshKeyRef": ""
}
```

Output:

```json
{
  "id": "f5ec...",
  "status": "queued",
  "statusUrl": "/api/v1/repositories/f5ec..."
}
```

Annotations:

- Destructive: false.
- Idempotent: true.
- Open-world: true because it contacts an external Git host.

#### `list_repositories`

Returns repository IDs, source identities, and states.

Annotations:

- Read-only: true.
- Idempotent: true.

#### `get_repository`

Input:

```json
{
  "id": "f5ec..."
}
```

Returns metadata and status.

#### `list_graph_artifacts`

Returns the approved artifact inventory.

#### `get_graph_download_url`

Returns a REST URL rather than embedding a potentially large ZIP in MCP.

Output:

```json
{
  "repositoryId": "f5ec...",
  "format": "zip",
  "url": "https://graphify.example/api/v1/repositories/f5ec.../download?format=zip"
}
```

A future version could issue time-limited signed URLs.

### 12.3 MCP resources

Useful optional MCP resource templates:

```text
graphify://repositories
graphify://repositories/{id}
graphify://repositories/{id}/artifacts
```

These should be read-only and backed by the same service layer as REST.

### 12.4 Server card

```text
GET /.well-known/mcp/server-card.json
```

Response concept:

```json
{
  "name": "io.github.marcellodesales.graphify-service",
  "title": "Graphify as a Service",
  "description": "Clone repositories, generate Graphify knowledge graphs, and retrieve graph artifacts.",
  "transports": [
    {
      "type": "streamable-http",
      "url": "https://graphify.example/mcp"
    }
  ],
  "auth": {
    "type": "oauth2",
    "authorizationServerUrl": "https://graphify.example"
  },
  "tags": [
    "graphify",
    "knowledge-graph",
    "git",
    "source-code",
    "mcp"
  ]
}
```

---

## 13. Authentication and OAuth

### 13.1 Development mode

For local Compose development, support a configured static bearer token:

```text
GRAPHIFY_AUTH_MODE=static
GRAPHIFY_API_TOKEN=<random-secret>
```

This mode should be documented as local/development only.

The token must not be committed to Compose. It should come from `.env` or a Docker secret.

### 13.2 Production mode

Recommended production model:

```text
GRAPHIFY_AUTH_MODE=oidc
GRAPHIFY_OIDC_ISSUER=https://identity.example
GRAPHIFY_OIDC_AUDIENCE=graphify-service
GRAPHIFY_OIDC_JWKS_URL=https://identity.example/.well-known/jwks.json
```

The service validates bearer JWTs issued by an external OAuth/OIDC provider.

The service does not need to implement an entire user-login authorization server in the first version.

### 13.3 OAuth discovery

Authorization-server metadata:

```text
/.well-known/oauth-authorization-server
```

If using external OIDC, this endpoint may:

- Redirect to the external issuer metadata, or
- Return metadata that accurately identifies the external endpoints.

Protected-resource metadata:

```json
{
  "resource": "https://graphify.example/mcp",
  "authorization_servers": [
    "https://identity.example"
  ],
  "scopes_supported": [
    "graphify:read",
    "graphify:clone",
    "graphify:download"
  ]
}
```

Endpoints:

```text
/.well-known/oauth-protected-resource
/.well-known/oauth-protected-resource/mcp
```

### 13.4 Authorization policy

Public:

- `/healthz`
- `/readyz`
- OAuth metadata.
- MCP server card.
- MCP initialization/discovery methods if required for client discovery.

Protected:

- REST repository creation.
- REST repository listing/status.
- Artifact listing/download.
- MCP `tools/call`.
- MCP resources containing repository information.

Suggested scopes:

| Scope | Operations |
|---|---|
| `graphify:read` | List/get repository metadata and artifact inventories |
| `graphify:clone` | Submit clone jobs |
| `graphify:download` | Download generated artifacts |
| `graphify:admin` | Retry/delete/operations added later |

---

## 14. Docker Compose specification

A target Compose topology should look like this:

```yaml
services:
  nats:
    image: nats:2-alpine
    command:
      - --jetstream
      - --store_dir=/data
      - --http_port=8222
    volumes:
      - nats_data:/data
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "-", "http://localhost:8222/healthz"]
      interval: 5s
      timeout: 3s
      retries: 20

  graphify-api:
    build:
      context: .
      dockerfile: Dockerfile.service
      target: api
    command: ["api"]
    environment:
      GRAPHIFY_REPOS_ROOT: /graphify-service/repos
      NATS_URL: nats://nats:4222
      GRAPHIFY_AUTH_MODE: ${GRAPHIFY_AUTH_MODE:-static}
      GRAPHIFY_API_TOKEN: ${GRAPHIFY_API_TOKEN:?set GRAPHIFY_API_TOKEN}
    volumes:
      - ./data/repos:/graphify-service/repos
    ports:
      - "8080:8080"
    depends_on:
      nats:
        condition: service_healthy

  clone-worker:
    build:
      context: .
      dockerfile: Dockerfile.service
      target: clone-worker
    command: ["clone-worker"]
    environment:
      GRAPHIFY_REPOS_ROOT: /graphify-service/repos
      GRAPHIFY_SSH_ROOT: /run/secrets/graphify-ssh
      GRAPHIFY_KNOWN_HOSTS: /run/secrets/graphify-ssh/known_hosts
      NATS_URL: nats://nats:4222
    volumes:
      - ./data/repos:/graphify-service/repos
      - ./secrets/ssh:/run/secrets/graphify-ssh:ro
    depends_on:
      nats:
        condition: service_healthy

  graphify-worker:
    build:
      context: .
      dockerfile: Dockerfile.service
      target: graphify-worker
    command: ["graphify-worker"]
    environment:
      GRAPHIFY_REPOS_ROOT: /graphify-service/repos
      NATS_URL: nats://nats:4222
    volumes:
      - ./data/repos:/graphify-service/repos
    depends_on:
      nats:
        condition: service_healthy

  neo4j:
    image: neo4j:5.22.0-community
    profiles: ["neo4j"]
    environment:
      NEO4J_AUTH: ${NEO4J_AUTH:?set NEO4J_AUTH}
    ports:
      - "7474:7474"
      - "7687:7687"
    volumes:
      - neo4j_data:/data

volumes:
  nats_data:
  neo4j_data:
```

This is illustrative, not a final file. Important refinements are still needed:

- Avoid `${VAR:?}` if it breaks existing CI Compose service discovery.
- Preserve the existing unprofiled `graphify` build target because current multi-architecture CI discovers it through `docker compose config --services`.
- Do not hard-code Neo4j credentials.
- Add explicit UID/GID handling for the host bind mount.
- Health checks must use only tools installed in each image.

---

## 15. Container security

All service containers should:

- Run as non-root.
- Use a fixed UID/GID, ideally configurable.
- Have a writable repository volume only where needed.
- Drop Linux capabilities.
- Use `no-new-privileges`.
- Use a read-only root filesystem where feasible.
- Store temporary files under a dedicated writable `tmpfs`.
- Avoid the Docker socket.
- Avoid mounting the entire host SSH directory.
- Mount only a service-specific credential directory.
- Avoid storing secrets in image layers.

The shared volume creates a permissions issue. Recommended Compose variables:

```text
GRAPHIFY_UID=10001
GRAPHIFY_GID=10001
```

All three service images should use the same runtime UID/GID.

---

## 16. Proposed Go project structure

```text
cmd/
└── graphify-service/
    └── main.go

internal/
├── api/
│   ├── handlers.go
│   ├── middleware.go
│   ├── routes.go
│   └── models.go
├── app/
│   └── service.go
├── artifacts/
│   ├── inventory.go
│   └── zip.go
├── auth/
│   ├── bearer.go
│   ├── oidc.go
│   └── metadata.go
├── clone/
│   ├── git.go
│   ├── ssh.go
│   └── worker.go
├── config/
│   └── config.go
├── events/
│   ├── cloudevents.go
│   ├── jetstream.go
│   └── subjects.go
├── graphify/
│   ├── command.go
│   └── worker.go
├── mcp/
│   ├── server.go
│   ├── tools.go
│   └── resources.go
├── repository/
│   ├── id.go
│   ├── metadata.go
│   ├── paths.go
│   ├── state.go
│   └── store.go
├── securepath/
│   └── securepath.go
└── telemetry/
    ├── logging.go
    └── metrics.go

schemas/
├── repository-clone-requested-v1.json
├── repository-cloned-v1.json
└── repository-graph-ready-v1.json

integration/
├── clone_test.go
├── api_test.go
└── artifacts_test.go

scripts/
└── smoke.sh

Dockerfile.service
docker-compose.yaml
.env.example
Makefile
go.mod
go.sum
```

---

## 17. Configuration

Suggested environment variables:

| Variable | Default | Purpose |
|---|---|---|
| `GRAPHIFY_HTTP_ADDR` | `:8080` | API/MCP bind address |
| `GRAPHIFY_REPOS_ROOT` | `/graphify-service/repos` | Shared repository root |
| `GRAPHIFY_SSH_ROOT` | `/run/secrets/graphify-ssh` | SSH credential root |
| `GRAPHIFY_KNOWN_HOSTS` | `<ssh-root>/known_hosts` | Host-key database |
| `NATS_URL` | `nats://nats:4222` | NATS connection |
| `GRAPHIFY_AUTH_MODE` | none | `static` or `oidc` |
| `GRAPHIFY_API_TOKEN` | none | Development static token |
| `GRAPHIFY_OIDC_ISSUER` | none | OIDC issuer |
| `GRAPHIFY_OIDC_AUDIENCE` | none | Required audience |
| `GRAPHIFY_ALLOWED_GIT_HOSTS` | empty | Optional host allowlist |
| `GRAPHIFY_CLONE_TIMEOUT` | `10m` | Git timeout |
| `GRAPHIFY_RUN_TIMEOUT` | `60m` | Graphify timeout |
| `GRAPHIFY_MAX_REQUEST_BYTES` | `1MiB` | API body limit |
| `GRAPHIFY_MAX_DOWNLOAD_BYTES` | configurable | ZIP limit |
| `GRAPHIFY_LOG_LEVEL` | `info` | Structured log level |
| `GRAPHIFY_MCP_STATELESS` | `true` | MCP session mode |

Production startup should fail if authentication is disabled while binding to a non-loopback address, unless an explicit insecure-development override is set.

---

## 18. Observability

### 18.1 Logs

Use structured JSON logs containing:

- Timestamp.
- Severity.
- Component.
- Request ID.
- Repository ID.
- Event ID.
- NATS delivery attempt.
- State transition.
- Duration.
- Safe error classification.

Never log:

- SSH key content.
- Authorization headers.
- API tokens.
- URL passwords.
- Complete process environments.
- Git credential helpers.

### 18.2 Metrics

Expose:

```text
GET /metrics
```

Suggested Prometheus metrics:

```text
graphify_api_requests_total
graphify_api_request_duration_seconds
graphify_repository_jobs_total
graphify_repository_state
graphify_clone_duration_seconds
graphify_clone_failures_total
graphify_run_duration_seconds
graphify_run_failures_total
graphify_artifact_bytes
graphify_nats_redeliveries_total
graphify_worker_active_jobs
```

Do not use repository URLs as metric labels. Repository ID labels can also create high cardinality and should generally be omitted.

---

## 19. Testing plan

### 19.1 Unit tests

Required:

- URL normalization.
- HTTPS and SCP-like SSH parsing.
- Removal or rejection of URL credentials.
- Deterministic hash generation.
- Default/ref/SHA identity differences.
- Ref validation.
- Repository ID validation.
- Safe repository-path construction.
- SSH key-reference validation.
- Symlink-escape prevention.
- State-transition validation.
- Atomic metadata persistence.
- CloudEvent encoding.
- Duplicate-event handling.
- Artifact-allowlist enforcement.
- ZIP traversal protection.
- REST handler status and error responses.
- Authentication middleware.
- MCP tool input/output conversion.

### 19.2 Integration tests

#### Public default-branch clone

- Submit a small public repository.
- Verify depth is 1 where supported.
- Verify only the default branch is checked out.
- Verify `resolvedSha`.

#### Explicit branch

- Submit with a known branch.
- Verify detached or expected checkout.
- Verify no unrelated history.

#### Explicit tag

- Submit with a known tag.
- Verify the resolved commit.

#### SHA-only

- Submit an advertised or reachable commit SHA.
- Verify detached checkout.
- Verify the resolved SHA.
- Test a clear failure for an unavailable SHA.

#### Idempotency

- Submit the same request twice.
- Verify the same ID.
- Verify only one clone execution.
- Redeliver NATS messages.
- Verify no duplicate Graphify execution.

#### Artifacts

- Produce a test `graphify-out`.
- Verify only allowed files enter the ZIP.
- Add `.git/config`, source files, symlinks, and traversal candidates.
- Verify none are downloadable.

### 19.3 Compose smoke test

Expected flow:

```bash
set -euo pipefail

response="$(
  curl -fsS \
    -H "Authorization: Bearer ${GRAPHIFY_API_TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"githubRepoUrl":"https://github.com/octocat/Hello-World.git"}' \
    http://localhost:8080/api/v1/repositories
)"

id="$(printf '%s' "$response" | jq -r '.id')"

until [ "$(curl -fsS \
  -H "Authorization: Bearer ${GRAPHIFY_API_TOKEN}" \
  "http://localhost:8080/api/v1/repositories/${id}" | jq -r '.status')" = "ready" ]; do
  sleep 2
done

curl -fsS \
  -H "Authorization: Bearer ${GRAPHIFY_API_TOKEN}" \
  "http://localhost:8080/api/v1/repositories/${id}/artifacts" | jq .

curl -fsS \
  -H "Authorization: Bearer ${GRAPHIFY_API_TOKEN}" \
  -o "graphify-${id}.zip" \
  "http://localhost:8080/api/v1/repositories/${id}/download?format=zip"
```

The smoke test should also initialize MCP, list tools, and call `get_repository`.

---

## 20. Makefile targets

```text
make build
make test
make test-unit
make test-integration
make lint
make fmt
make vet
make docker-build
make compose-up
make compose-down
make smoke
make clean
make generate-schemas
```

Existing Python tests should remain available:

```text
make test-python
```

A combined CI target should run both Go and Python tests.

---

## 21. Delivery phases

### Phase 1: API and metadata

Implement:

- Go binary.
- Configuration.
- Filesystem store.
- URL normalization.
- Deterministic hashes.
- State model.
- `POST`, list, and status APIs.
- Health/readiness.
- Unit tests.

Acceptance:

- Submission returns a stable ID.
- Duplicate submission returns the same ID.
- Metadata is atomically persisted.

### Phase 2: NATS and clone worker

Implement:

- NATS JetStream.
- CloudEvent envelopes.
- Clone worker.
- Default/ref/SHA shallow Git behavior.
- SSH key references and `known_hosts`.
- Clone metadata and failure events.
- Integration tests.

Acceptance:

- A public repository can be cloned asynchronously.
- SHA-only works where supported.
- No full-history fallback occurs.
- Duplicate delivery is harmless.

### Phase 3: Graphify worker and artifacts

Implement:

- Graphify worker image based on the Graphify runtime.
- Graphify execution.
- Artifact inventory.
- ZIP downloads.
- Ready/failed events.
- Artifact-security tests.

Acceptance:

- A clone automatically triggers Graphify.
- API reaches `ready`.
- ZIP includes only approved artifacts.

### Phase 4: MCP

Implement:

- Streamable HTTP `/mcp`.
- Five required tools.
- Structured schemas and results.
- Optional repository resources.
- Server card.

Acceptance:

- An MCP client can list tools.
- An MCP client can submit and inspect a repository.
- The download tool returns a REST URL.

### Phase 5: Authentication and hardening

Implement:

- Static development bearer mode.
- External OIDC JWT validation.
- OAuth protected-resource metadata.
- CORS.
- Rate limits.
- Metrics.
- Non-root images.
- Compose security options.
- Operational documentation.

Acceptance:

- Tool execution and protected REST routes reject unauthenticated requests.
- Discovery and health remain accessible.
- Production non-loopback mode refuses insecure startup.

---

## 22. Important risks and decisions

### 22.1 SHA-only Git fetch compatibility

Not every Git server permits direct fetching of an unadvertised SHA. The service must report this honestly and must not fetch all branches as an implicit fallback.

### 22.2 Filesystem-store scalability

Filesystem metadata is suitable for the first vertical slice but has limitations:

- Listing requires directory scanning.
- Multi-node deployment requires shared POSIX-compatible storage.
- Locks may behave differently on network filesystems.
- There is no rich querying.
- Cleanup and retention need explicit policies.

A later version may move metadata to PostgreSQL while leaving repositories and artifacts on object or shared storage.

### 22.3 Bind-mount permissions

Non-root API and worker containers must share a common UID/GID. This must be tested on Linux and documented for Docker Desktop.

### 22.4 Arbitrary repository workload

Cloning and parsing untrusted repositories consumes:

- Disk.
- CPU.
- Memory.
- Time.

Graphify processes file contents, so production deployments should add:

- Resource limits.
- Job quotas.
- Repository-size limits.
- Tenant isolation.
- Retention policies.
- Network-egress restrictions.
- Possibly Kubernetes Jobs or sandboxed workers.

### 22.5 SSRF and network access

A user-provided Git URL is an outbound network request. The service must:

- Allow only HTTPS and SSH.
- Reject localhost, link-local, and private-network destinations by default in public deployments.
- Optionally require `github.com` or an explicit allowlist.
- Re-resolve and validate DNS addresses to mitigate rebinding.
- Document enterprise Git-host exceptions.

This is a critical production requirement.

### 22.6 Repository submodules and LFS

The initial implementation should not automatically fetch:

- Git submodules.
- Git LFS objects.

Both can cause additional credential use and network access. They should be explicit future options with separate security review.

### 22.7 Graphify semantic-extraction credentials

Code-only extraction can run locally. Documents, PDFs, and images may require an LLM backend.

The first service version should either:

- Run code-only/local extraction by default, or
- Support operator-provided backend credentials through container secrets.

User-provided LLM keys should not be accepted in the clone API.

---

## 23. Definition of done for the first production-worthy vertical slice

The implementation is complete when:

1. `docker compose up` starts NATS, API, clone worker, and Graphify worker.
2. The API accepts a public GitHub URL.
3. It returns `202` and a deterministic SHA-256 ID immediately.
4. NATS durably dispatches the clone job.
5. The clone worker performs a depth-one clone.
6. The selected ref or SHA is correctly checked out.
7. Metadata records the resolved full SHA.
8. A cloned CloudEvent triggers Graphify.
9. Graphify creates `graphify-out`.
10. Repository status becomes `ready`.
11. REST lists artifacts.
12. REST downloads an allowlisted ZIP.
13. MCP lists and invokes the required tools.
14. Authentication protects tool execution and repository APIs.
15. Health/readiness remain public.
16. Duplicate requests and NATS deliveries are harmless.
17. Private-repository SSH keys are mounted references only, with strict host-key verification.
18. No Docker socket is mounted.
19. All containers run as non-root.
20. Existing Graphify Python behavior and the multi-architecture image target continue working.

---

## 24. Summary

This plan establishes Graphify-as-a-Service as an API-first Go control plane around the existing Python Graphify engine.

The proposed architecture provides:

- Deterministic and idempotent repository jobs.
- Secure shallow Git cloning.
- Explicit SHA-only behavior.
- NATS JetStream asynchronous processing.
- CloudEvents-compatible lifecycle events.
- A dedicated Graphify worker.
- Filesystem-backed metadata for the initial version.
- Approved artifact downloads.
- REST and MCP interfaces backed by the same service layer.
- OAuth-aware discovery and protected tool execution.
- A clear path from local Docker Compose to Kubernetes and Knative Eventing.

The implementation should proceed as a sequence of working vertical slices rather than as one large speculative rewrite.
