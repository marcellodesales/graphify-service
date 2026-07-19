# PRD-005 — Integration testing (Bruno)

**Test cases:** T1 (HTTP integration), T2 (queue + verification), T3 (MCP).

We use [Bruno](https://www.usebruno.com/) — plain-text `.bru` files checked into
the repo — for HTTP integration tests. Bruno is git-friendly and (per the
ecosystem) bridges to MCP later, which suits our two test levels.

## Layout

```
tests/integration/
├── bruno/
│   ├── bruno.json                 # collection
│   ├── environments/local.bru     # baseURL, token, sample repo URL
│   ├── 01-submit.bru              # POST submit → capture reference id
│   ├── 02-status-poll.bru         # GET status until ready (retry)
│   ├── 03-artifacts.bru           # GET artifacts inventory
│   ├── 04-download-zip.bru        # GET download?format=zip
│   └── 05-service-status.bru      # GET /status/{id} on cloner + worker (T2)
├── docker-compose.test.yaml       # overrides: build workers, mount test data
└── run.sh                         # bring up stack, run bruno, tear down
```

## T1 — HTTP integration (submit → poll → download)

**Level 1.** Runs the real async pipeline end-to-end against a **live public
github.com repo** (no keys), graphified **code-only** (`graphify extract
--code-only`, no LLM key).

Flow & assertions:
1. `01-submit` → `202`; `res.body.id` is 64-hex; save as `refId`.
2. `02-status-poll` → poll `GET /api/v1/repositories/{refId}` until
   `res.body.status == "ready"` (bounded retries/timeout); `resolvedSha` present.
3. `03-artifacts` → `200`; inventory contains `graph.json` + `GRAPH_REPORT.md`;
   no `.graphify_*` / `.git` entries.
4. `04-download-zip` → `200`, `content-type: application/zip`, non-empty body.

Sample repo: a small public repo so code-only extraction is fast (e.g.
`https://github.com/octocat/Hello-World` for smoke, or a slightly richer small
repo for a non-trivial graph). Chosen in `environments/local.bru`.

## T2 — Queue + per-service verification

**Level 1.5.** Proves the status protocol (PRD-001 R5) and that the queue drove
the workers:
- `05-service-status` calls `GET /status/{refId}` on the cloner and worker and
  asserts each reports a coherent `phase` for the same id, and `service` names
  the responder.
- Optionally assert idempotency: re-submitting the same URL returns the same id
  and does not double-run (status timestamps unchanged).

## T3 — MCP integration (Level 2)

**Deferred** with R8 (PRD-004). Bruno acts as an MCP client: `initialize` →
`tools/list` → `tools/call query_graph` for a `ready` id → assert on-topic
answer. Added once the front MCP + graphify-mcp microservice exist.

## Running

`tests/integration/run.sh`:
1. `docker compose -f docker-compose.yaml -f tests/integration/docker-compose.test.yaml up -d --build nats graphify-api graphify-cloner graphify-worker`
2. Wait for `/readyz`.
3. `bru run tests/integration/bruno --env local` (Bruno CLI).
4. Capture exit code; `docker compose … down -v` on exit.

CI: a `.github/workflows/integration.yml` runs `run.sh` on PRs touching
`backend/**` or `tests/**`. Kept separate from the image-build workflows.

## Non-goals

- Load/perf testing; private-repo (SSH) integration (public-first).
