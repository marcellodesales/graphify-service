# Graphiti Integration — Temporal Context Graph for gref-cloud

**Status:** Design / reference · **Owner:** graphify-service backend · **Last updated:** 2026-07-19

> Consolidated reference for adopting [Graphiti](https://github.com/getzep/graphiti) as the
> **temporal graph database** that stores graphify output and reconciles the *structural* code
> graph with *temporal* relationships, per customer. Pairs with the raw feasibility assessment in
> [`GRAPHITI-SERVICE.md`](./GRAPHITI-SERVICE.md) and the backend spec in
> [`FEATURES-BACKEND-SERVICE.md`](./FEATURES-BACKEND-SERVICE.md).

---

## 1. TL;DR

- **What Graphiti is:** an open-source (Apache-2.0) **temporal context-graph engine** (Python
  `graphiti-core`). It stores entities, relationships (facts), and their **bi-temporal validity
  windows** in a graph database, with full provenance back to the raw **episodes** that produced
  them. It is the OSS core behind Zep's agent-memory product.
- **Why we want it:** graphify gives us a *point-in-time structural* code graph. Graphiti lets us
  ingest those graphs **over time** (per scan/commit) and query **how the graph changed** — what a
  relationship *is now* vs. *what it was* — which is exactly the "reconcile structural + temporal"
  capability we want to offer customers.
- **How it fits our stack:** Graphiti runs on **Neo4j** (which we already ship). We drive ingestion
  from **NATS** through a **Go** worker (not Graphiti's in-process queue), keep Graphiti behind our
  **Go API/BFF** (never exposed to customers), and serve graph reads for visualization via **direct
  Neo4j Cypher** through Go (Graphiti's search API alone is insufficient for rendering — see §8).
- **The one big new dependency:** Graphiti's `add_memory` ingestion **requires an LLM + embeddings**
  (OpenAI/Anthropic/Gemini/Azure/Ollama) for entity/edge extraction. For our *already-structured*
  code graphs we can largely **bypass the LLM** with `add_triplet` / direct edge writes (§7) —
  deterministic and free. Reserve LLM extraction for unstructured inputs (docs, commit messages,
  customer chat).

---

## 2. What Graphiti is

### 2.1 Data model

| Component | What it stores |
|-----------|----------------|
| **Entities** (nodes) | People, products, files, symbols, concepts — with summaries that evolve over time |
| **Facts / Relationships** (edges) | `Entity → relationship → Entity` triplets, each with a **validity window** (`valid_at` → `invalid_at`) |
| **Episodes** (provenance) | Raw data as ingested (text / message / JSON). Every derived fact traces back to an episode |
| **Custom Types** (ontology) | Developer-defined entity & edge types via Pydantic models — *prescribed* or *learned* |

The defining feature is **bi-temporal tracking**: when information changes, old facts are
**invalidated, not deleted** — you can query "what is true now" *or* "what was true at time T",
and contradictions are auto-invalidated while history is preserved.

### 2.2 Requirements (upstream)

- **Python 3.10+**, `pip install graphiti-core`.
- **Graph backend:** Neo4j **5.26+** / FalkorDB 1.1.2 / Amazon Neptune (+OpenSearch) / Kuzu
  (*deprecated*). **We use Neo4j.**
- **LLM + embeddings:** OpenAI (default), Anthropic, Gemini, Groq, Azure OpenAI, or any
  OpenAI-compatible endpoint incl. local (Ollama/vLLM/LM Studio). **Structured (JSON) output is
  required** — small/local models are unreliable; prefer OpenAI/Anthropic/Gemini.
- **Concurrency:** `SEMAPHORE_LIMIT` (default 10) caps concurrent episode processing to avoid LLM
  `429`s. Each episode fans out into several LLM calls (extract → dedup → summarize).

### 2.3 The three ways to run Graphiti

| Surface | Image / package | Interface | Fit for us |
|---------|-----------------|-----------|-----------|
| **Core library** | `graphiti-core` (PyPI) | Python API (`Graphiti(...)`, `add_episode`, `add_triplet`, `search_`) | Embed in a Python worker if we need full control |
| **REST server** | `zepai/graphiti` (Docker Hub, multi-arch, port 8000, `/docs`) | FastAPI: `POST /messages`, `/entity-node`, `/search`, `GET /episodes/{group_id}`, `POST /get-memory`, destructive `/clear` | Ingestion/search REST; **read API insufficient for viz** (§8) |
| **MCP server** | `zepai/knowledge-graph-mcp` (HTTP `/mcp/` or stdio) | MCP tools: `add_memory`, `add_triplet`, `search_nodes`, `search_memory_facts`, `get_episodes`, `get_episode_entities`, `build_communities`, `delete_*`, `clear_graph`, `get_status` | **Best fit** — reuses our existing Go `mcpproxy` pattern (same as graphify-mcp) |

**Recommendation:** drive **writes** through the **MCP server** (we already have a Go MCP proxy),
and do **reads for visualization** with **direct Neo4j Cypher** from Go. See §7–§8.

---

## 3. What Graphiti gives us vs. what we build

| Graphiti core gives us (reuse) | We build (do not expect from Graphiti) |
|--------------------------------|-----------------------------------------|
| Episode ingestion + provenance | AuthN/AuthZ and **tenant isolation** (group_id is *not* a security boundary — §9) |
| Entity/edge creation & dedup | **Durable** ingestion queue, retries, DLQ (Graphiti's built-in queue is in-process — §6) |
| Bi-temporal fact invalidation | Customer-facing **graph read/visualization API** (source+target topology) |
| Hybrid search (semantic + BM25 + graph) | Control plane (tenants, workspaces, quotas, audit, billing) |
| Community detection / summaries | Deep health checks, migration jobs, observability wiring |

---

## 4. Where it sits in the gref-cloud pipeline

```
 GitHub repo ─▶ cloner ─▶ worker (graphify extract/enrich) ─▶ artifacts (graph.json, GraphML, …)
                                     │
                                     ▼  NATS: graph.ready
                        graphiti-ingest-worker (Go)          ← NEW
                                     │  (add_triplet for known code edges;
                                     │   add_memory for docs / commit msgs)
                                     ▼
                        Graphiti (MCP or REST)  ──▶  Neo4j  (temporal context graph)
                                     ▲                         │
        customer query / viz ─▶ Go API/BFF ──────────────────┘ (direct Cypher reads for topology)
```

- **graphify** already produces a structural graph (`graph.json`: nodes = files/symbols/modules,
  edges = calls/imports/contains). That is the *current-state* structural graph.
- Each scan is an **episode** in Graphiti. Re-ingesting on every commit/scan makes Graphiti track
  **how the code graph evolves** — new/removed symbols, changed call relationships — with validity
  windows. That is the **temporal** layer we reconcile against the structural graph.

---

## 5. Reconciling the two graphs

| | graphify graph | Graphiti graph |
|---|----------------|----------------|
| **Nature** | Structural, point-in-time | Temporal, cumulative |
| **Source** | AST extraction (deterministic, no LLM) | Episodes over time (LLM for unstructured) |
| **Answers** | "What calls what *now*?" | "When did X start calling Y? What changed since last release?" |
| **Storage** | `graph.json` / GraphML artifacts (+ optional Neo4j export) | Neo4j (Graphiti-owned database/namespace) |

**Keep them separate but linkable.** Two clean options in one Neo4j instance:
- **Separate Neo4j databases** — Graphiti's driver takes a `database` name; give Graphiti its own
  DB so its temporal graph never collides with any graphify Neo4j export.
- **Shared DB, distinct `group_id`** — Graphiti namespaces by `group_id`; use a per-workspace
  internal group id (§9). Simpler ops, weaker isolation.

Link the layers by carrying graphify's stable node identity (e.g. `repo@path#symbol`) as an entity
attribute in Graphiti, so a customer can pivot from "current structure" to "temporal history" of the
same symbol.

---

## 6. Production gaps in stock Graphiti — and how our stack closes them

The stock REST/MCP servers were built for single-tenant/demo use. The
[assessment](./GRAPHITI-SERVICE.md) flagged these; our chosen stack already addresses most:

| Gap (upstream) | Our resolution |
|----------------|----------------|
| **In-process `asyncio.Queue`** — work lost on restart, no retry/DLQ, no backpressure | Ingest from **NATS JetStream** via a Go worker. Durability, redelivery, idempotency (`Nats-Msg-Id`), and DLQ come from NATS — Graphiti is called **synchronously per message** so its internal queue is bypassed. |
| **Shallow `/healthcheck`** (always healthy, no DB check) | Our Go API/worker expose `/livez` + `/readyz` that verify **Neo4j + NATS + Graphiti** reachability (same pattern as the existing pipeline). |
| **Startup `build_indices_and_constraints()` on every replica** | Run once as a **K8s pre-install/pre-upgrade Job** (idempotent migration), not per-pod. |
| **`group_id` treated as identity** | `group_id` is an **internal namespace only**; authorization is resolved in our Go BFF (§9). |
| **Destructive routes (`/clear`, delete-by-uuid) unauthenticated** | Graphiti is **never** exposed to customers — only reachable from our backend inside the cluster network. |

---

## 7. Ingestion strategy (cost-aware)

Graphiti offers two write paths — **choose per input type**:

- **`add_triplet(source, fact, target)`** — writes a fact **directly, bypassing LLM extraction**.
  **Use this for graphify's structural edges** (we already know the entities and relationships from
  the AST). Deterministic, precise, **no LLM cost**.
- **`add_memory` / `add_episode(source="json"|"text")`** — runs the **LLM extraction pipeline**
  (entities, dedup, summaries, temporal invalidation). **Use for unstructured inputs**: commit
  messages, PR descriptions, design docs, customer conversations — where the value is *discovering*
  structure.

> This split is the single biggest cost lever. Piping a large `graph.json` through `add_memory`
> would incur an LLM call storm; `add_triplet` avoids it entirely for the parts we already know.

Guardrails to configure regardless of path: `SEMAPHORE_LIMIT` (per LLM tier), per-tenant
concurrency + monthly episode/token quotas, idempotency keys, retry limits + DLQ, cost attribution
by tenant, provider circuit breakers.

---

## 8. Visualization & reads

The stock `POST /search` returns **fact DTOs without source/target node identities**, so it cannot
alone drive a graph renderer. Two-part approach:

1. **Topology reads → direct Neo4j Cypher** from a Go read handler. Return the renderer-ready shape:
   ```json
   {
     "nodes": [{ "id": "uuid", "label": "Acme", "type": "Organization", "summary": "…", "createdAt": "…" }],
     "edges": [{ "id": "uuid", "source": "alice-uuid", "target": "acme-uuid",
                 "label": "WORKS_AT", "fact": "Alice works at Acme",
                 "validAt": "…", "invalidAt": null }],
     "nextCursor": "…"
   }
   ```
   Never download a whole tenant graph — start from a search hit or a selected node and **expand
   neighborhoods progressively** with cursor pagination + `valid_at` time filters.
2. **Semantic/fact search → Graphiti** (`search_nodes`, `search_memory_facts`) for the "find a
   starting point" step; then hand node UUIDs to the topology read above.

**Frontend:** start with **React + Cytoscape.js** (rich interaction); move to **Sigma.js +
Graphology** (WebGL) if customers render 10k+ nodes. Node styling by entity type, edges labelled by
fact, node-details panel, temporal-validity filter, workspace selector.

**Licensing note:** Graphiti is Apache-2.0 (modify/commercial/derivative OK). Zep's **dashboard and
SDKs are Enterprise** — build our own viz over the Apache core; **do not** copy Enterprise assets;
Apache-2.0 grants no trademark rights. Not legal advice — have counsel review positioning.

---

## 9. Multi-tenancy (control plane outside the graph)

`group_id` is **logical grouping, not authorization**. Design:

```
Tenant  (billing + security boundary)
  └── Users / Service Accounts  (Memberships + Roles)
  └── Workspaces  (graph/data boundary)
        └── Graph namespace (internal, random group_id)
              └── Conversations / Sources / Episodes
```

- Keep identity/ownership in **PostgreSQL** (`tenants`, `users`, `tenant_memberships`, `workspaces`,
  `graph_namespaces`, `ingestion_jobs`, `usage_counters`, `retention_policies`, `audit_events`).
- Customers send a **public `workspace_id`**. The Go BFF: authenticates → resolves tenant → checks
  membership/role → maps to the **internal random `group_id`** → passes only that to Graphiti.
- **Never** derive authorization from a predictable value (e.g. `tenantId:userId`).
- **Isolation tiers:** shared Neo4j + per-workspace random `group_id` (standard plans, cheapest,
  needs airtight query scoping + cross-tenant isolation tests) → **database/deployment per tenant**
  (enterprise plans; stronger blast-radius isolation; higher ops cost — verify Neo4j
  edition/licensing before promising DB-per-tenant).

---

## 10. Kubernetes deployment (phased)

Reconciled with our stack (NATS, Go, Neo4j). Phase estimates from the assessment.

**Target topology**
```
Ingress/Gateway ─▶ Go API/BFF ─┬─ PostgreSQL (tenants, ACLs, jobs, audit)
                               ├─ Redis (cache, rate limits)   [optional]
                               └─ NATS JetStream (durable ingestion)
                                        │
                        Go graphiti-ingest-worker ─▶ Graphiti (MCP/REST) ─▶ Neo4j
                                        │                                     ▲
   Visualization web app ─▶ Go API/BFF ┴──── direct Cypher reads ────────────┘
                                        └─ LLM + embedding provider
```

**Workloads**
- **Go API/BFF** `Deployment` — auth, graph read endpoints, job submission, rate limiting. No
  long-running extraction. Probes hit `/livez` `/readyz`.
- **Graphiti** `Deployment` (`zepai/knowledge-graph-mcp` or `zepai/graphiti`) — stateless; cluster-
  internal `Service` only (no Ingress). Secrets: LLM key, Neo4j creds.
- **graphiti-ingest-worker** `Deployment` (Go) — consumes NATS, calls Graphiti; own concurrency
  limits, retries, DLQ; horizontally scalable.
- **Migration `Job`** — `build_indices_and_constraints()` once (pre-install/pre-upgrade hook).
- **Neo4j** — prefer **managed** (Aura) or a separately operated StatefulSet; **do not** bury Neo4j
  in the app Helm chart except for dev (backup/restore/upgrade/HA concerns).
- **NATS** — JetStream with persistent storage (we already run it in compose).
- **Observability** — OpenTelemetry collector (Graphiti ships OTEL support), Prometheus, central
  logs; alerts on queue depth, job failures, Neo4j latency, LLM failures, token spend.

**Phasing**
1. **Internal MVP (~1–2 wk):** pinned images, Go API + managed Neo4j + NATS, secrets/network
   policies, probes, migration Job, basic OTEL, backup validation.
2. **Viz MVP (~2–4 wk):** authenticated `/graph` + `/neighbors` (source/target + cursor), React +
   Cytoscape.js, search, node details, temporal filter, progressive expansion.
3. **Multi-tenancy (~3–6 wk):** OIDC/JWT or API keys, Postgres control plane, RBAC, internal
   namespace resolution, tenant-safe reads/deletes, durable ingestion, quotas, audit, retention.
4. **Hardening (~2–6+ wk):** load + isolation tests, backup/restore drills, worker autoscaling,
   large-graph rendering, billing/metering, security review, SLOs, dedicated-tenant option.

---

## 11. Configuration reference

| Concern | Setting | Notes |
|---------|---------|-------|
| Neo4j | `NEO4J_URI` (`bolt://neo4j:7687`), `NEO4J_USER`, `NEO4J_PASSWORD` | Graphiti requires **5.26+**; verify our compose/K8s Neo4j version |
| LLM | `OPENAI_API_KEY` (default) or `ANTHROPIC_API_KEY` / `GOOGLE_API_KEY` / `GROQ_API_KEY` / Azure vars | Structured-output-capable model required |
| Local LLM | OpenAI-compatible `base_url` + placeholder key (Ollama/vLLM) | Dev only; small models unreliable for JSON schema |
| Concurrency | `SEMAPHORE_LIMIT` (default 10) | Tune per LLM tier (OpenAI T3 → 10–15; Anthropic default → 5–8; Ollama → 1–5) |
| MCP server | HTTP `/mcp/` (port 8000), `--group-id`, `--database-provider neo4j` | Reuse our Go `mcpproxy` |
| REST server | port 8000, `/docs`, `/redoc`, `/healthcheck` | `zepai/graphiti`, multi-arch |
| Telemetry | `GRAPHITI_TELEMETRY_ENABLED=false` | Anonymous PostHog stats; opt-out |

---

## 12. Open decisions

1. **Write surface:** MCP server (fits our `mcpproxy`) **[recommended]** vs. REST server vs. embed
   `graphiti-core` in a Python worker.
2. **LLM provider:** OpenAI (best structured output) vs. Gemini (aligns with the Google
   open-knowledge/A2UI direction) vs. Azure vs. local Ollama for dev. Cost + data-residency call.
3. **Neo4j:** managed (Aura) vs. self-operated StatefulSet; shared-DB+group_id vs. DB-per-tenant.
4. **Ingestion granularity:** what counts as an episode — per scan, per commit, per file? Drives
   temporal resolution and LLM cost.
5. **Structural vs. LLM split:** confirm `add_triplet` for graphify edges + `add_memory` only for
   unstructured inputs.
6. **Reconciliation key:** the stable cross-graph node identity (`repo@path#symbol`?).

---

## 13. References

- Graphiti README — <https://github.com/getzep/graphiti>
- REST server — <https://github.com/getzep/graphiti/tree/main/server> (`zepai/graphiti`)
- MCP server — <https://github.com/getzep/graphiti/tree/main/mcp_server> (`zepai/knowledge-graph-mcp`)
- Docs — <https://help.getzep.com/graphiti> · Quick start — <https://help.getzep.com/graphiti/graphiti/quick-start>
- Paper — *Zep: A Temporal Knowledge Graph Architecture for Agent Memory* — <https://arxiv.org/abs/2501.13956>
- Our feasibility assessment — [`GRAPHITI-SERVICE.md`](./GRAPHITI-SERVICE.md)
- Backend spec — [`FEATURES-BACKEND-SERVICE.md`](./FEATURES-BACKEND-SERVICE.md)
