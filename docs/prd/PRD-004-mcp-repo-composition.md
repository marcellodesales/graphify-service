# PRD-004 — MCP-against-repo composition & proxy

**Requirement:** R8. **Status: implemented (Option B).** A shared `graphify-mcp`
service serves the graphify query tools over Streamable HTTP for the whole repos
volume; the API's `POST /api/v1/repositories/{id}/query` composes with it,
injecting `project_path` per repo (stateless `tools/call`, no session). Exercised
by the integration suite (`tests/integration/bruno/07-query.bru`). A full MCP-
client front (Bruno driving `/mcp` directly, spec §12) remains future work.

## Problem

We want to answer questions about a specific graphified repo over MCP. Graphify
already ships an MCP server (`python -m graphify.serve`, see the gref-cloud
`graphify-mcp-server` skill) that serves ONE `graph.json` per process over
Streamable HTTP with query tools (query_graph, get_node, shortest_path, …).

Our public/front MCP server (the one users connect to) must answer repo
questions by **composing** with a graphify MCP instance bound to that repo's
graph. Open design question: does the graphify MCP load the graph into memory at
boot (one process per repo) or route per-request to a filesystem path?

## Findings (from the graphify MCP)

- The server is started with a specific `graph.json` and **loads it at boot**
  (in-memory). It is fundamentally **one graph per process**.
- BUT every tool accepts an optional injected `project_path` pointing at a dir
  containing `graphify-out/graph.json` — so a single server CAN answer about
  other repos' graphs on the shared volume by passing `project_path` per call.

This gives two viable architectures:

### Option A — one MCP process per repo (isolation)

A worker-like microservice (same image family as the graphify worker; it has the
`graphify` binary) boots `graphify.serve` bound to
`data/repos/<id>/repository/graphify-out/graph.json`. The front MCP **proxies**
tool calls to the right per-repo instance (lifecycle: start on first query, idle-
reap). Clean isolation; more processes.

### Option B — one shared MCP process, `project_path` routing

A single long-running graphify MCP over the shared `data/repos` volume. The front
MCP **injects `project_path=data/repos/<id>/repository`** into every tool call so
the shared server reads that repo's graph. Fewer processes; relies on
`project_path` support; all graphs must be on one shared volume.

**Recommendation:** start with **Option B** (reverse-proxy that rewrites tool
calls to inject `project_path`), because it reuses one microservice for any
directory — exactly the "reverse-proxy that translates the questions and makes
the MCP server see the directory we need" idea. Fall back to Option A if
`project_path` proves insufficient (e.g. large graphs, per-repo memory limits).

## Requirements (when built)

1. A **graphify-mcp microservice** (image = graphify base + config) serving
   Streamable HTTP over the shared `data/repos` volume.
2. The **front MCP** (our own Go MCP server, spec §12) exposes repo-query tools
   and, per call, resolves the reference ID → `project_path` and forwards to the
   graphify MCP (Option B) or the per-repo instance (Option A).
3. Only `ready` references are queryable; auth per spec §13.
4. Front MCP also exposes the control-plane tools (clone_repository,
   list_repositories, get_repository, get_graph_download_url) from spec §12.2 —
   so one MCP endpoint both drives the pipeline and answers repo questions
   (composition).

## Verification (T3)

- Bruno MCP flow: initialize → tools/list shows control + query tools → call
  `query_graph` for a `ready` id → assert a non-empty, on-topic answer.
- Assert isolation: querying id A never returns id B's nodes.
