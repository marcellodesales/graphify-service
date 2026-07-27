# PRD-005 — Integration testing (Bruno)

**Test cases:** T1 (HTTP integration), T2 (queue + verification), T3 (MCP).

We use [Bruno](https://www.usebruno.com/) — plain-text `.bru` files checked into
the repo — for HTTP integration tests. Bruno is git-friendly and (per the
ecosystem) bridges to MCP later, which suits our two test levels.

## Layout

```
tests/integration/
├── bruno/                         # repository API (T1)
│   ├── bruno.json                 # collection
│   ├── environments/local.bru     # baseURL, token, sample repo URL
│   ├── 01-submit.bru              # POST submit → capture reference id
│   ├── 02-status-poll.bru         # GET status until ready (retry)
│   ├── 03-artifacts.bru           # GET artifacts inventory
│   ├── 04-download-zip.bru        # GET download?format=zip
│   └── 05-service-status.bru      # GET /status/{id} on cloner + worker (T2)
├── bruno-memory/                  # memory API — single repo (T2)
│   ├── bruno.json
│   ├── environments/local.bru     # baseUrl, memoryId, resourceId, question
│   ├── 01-memory-ready.bru        # GET memory → ready + graphRef + links
│   ├── 02-resources-ready.bru     # every resource ready
│   ├── 03-merged-graph.bru        # GET graph → non-empty nodes[]
│   ├── 04-query-graph.bru         # POST query (query_graph) → on-topic
│   ├── 05-graph-stats.bru         # POST query (graph_stats)
│   ├── 06-artifacts.bru           # merged inventory includes graph.json
│   ├── 07-ssh-key-rejects-public.bru  # PUT ssh-key rejects a public key (400)
│   └── 08-work-status.bru         # GET .../status → lastOperation.status == succeeded
├── bruno-memory-2repos/           # memory API — 2-repo correlation (T3)
│   ├── bruno.json
│   ├── environments/local.bru     # baseUrl, memoryId, appSlug, deploySlug
│   ├── 01-two-resources.bru       # ≥2 resources, all ready
│   ├── 02-both-repos-in-graph.bru # both repo slugs present in unified graph
│   └── 03-image-query.bru         # cross-repo query resolves cleanly
├── ci/                            # in-container runner (no host bru/curl/jq)
│   ├── Dockerfile                 # bash + curl + jq + @usebruno/cli
│   └── run.sh                     # orchestrate (create→poll) then `bru run`
├── docker-compose.test.yaml       # overrides: build workers, mount test data
├── docker-compose-integration.yaml # include base+override + integration-tests svc
├── run.sh                         # manual host run (repo API, needs host bru)
├── run-memory.sh                  # manual host run (T2 memory, curl+jq)
├── run-memory-2repos.sh           # containerized T3 two-repo private suite (SUITE=memory2 + SSH key)
└── run-integration.sh             # containerized suite engine (Docker only; default SUITE=memory, 1 repo)
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

## Memory API suites (multi-source unified graph)

The memory abstraction (a user-defined collection of git/file resources merged
into one graph) has its own declarative Bruno coverage. Orchestration that a
`.bru` file can't express (create a memory, add resources, poll to `ready`) is
done in shell (`ci/run.sh` for the containerized run; `run-memory*.sh` for manual
runs); the collections then assert the HTTP contract against the ready memory.

- **`bruno-memory/`** (single repo) — memory `ready` with a committed `graphRef`
  and `graph`/`query`/`artifacts` links; every resource `ready`; merged graph has
  non-empty `nodes[]`; `query_graph` returns a non-error on-topic answer;
  `graph_stats` returns a non-error; the merged inventory includes `graph.json`
  and leaks no dotfiles; and a **`PUT .../ssh-key` with a public key is rejected
  `400 invalid_request`** — a non-mutating check that guards the no-secret-leak
  contract (rejection precedes any write).
- **`bruno-memory-2repos/`** (app + deploy) — ≥2 resources all `ready`; **both
  source repo slugs are present in the one unified graph** (proving the merge);
  and a cross-repo query resolves cleanly. Content correlation (matching a deploy
  image ref back to the app) depends on `--code-only` YAML indexing, so it is
  reported informationally by the shell driver, not hard-asserted.

## Decoder layer (how repos become graphs)

Extraction runs through a repo-level **decoder** layer (`graphify/decoders/`,
see its README) that sits above the per-file `extract._DISPATCH`. Each
`VionixRepoDecoder` self-scores a cloned repo (`can_decode`) and, if selected,
returns a `GraphBuilder` producing a schema-valid graph. Selection: `--decoder`
/ `GRAPHIFY_DECODER` override → future classifier → highest positive score →
generic fallback. This matters to these suites in two ways:

- **`generic`** reproduces the pre-decoder AST extraction byte-for-byte, so the
  T1 / single-repo (`bruno-memory/`) code repos are unchanged.
- **`cloud-native`** gives IaC repos (k8s / compose / kustomize / helm) a **rich
  structural graph** — a node per Kubernetes resource, compose service,
  kustomization, and referenced image — instead of the shallow file+top-level-key
  graph the generic YAML pass produced. This is what lets a YAML-heavy deploy repo
  extract to a non-empty graph at all (avoiding the `sys.exit(1)` empty-graph
  failure that fails the whole memory).

**Two communities, correlation deferred.** The `cloud-native` decoder emits
**file-scoped** image ids, so the same image referenced by the app repo and the
deploy repo gets **distinct** ids. Merging a 2-repo memory therefore yields **two
separate communities** — the library deliberately does not bridge them.
Cross-repo correlation (matching a deploy image ref back to the building app repo)
is a product concern deferred to the MCP query layer / future vionix devsecops
nodes; the globally-collapsing image bridge lives only in
`use-cases/cloud-native/`, never in the library. So `bruno-memory-2repos/`
asserts both repo slugs are present in the one unified graph (the merge happened),
but treats image-ref correlation as **informational**, not a hard assertion.

## T3 — MCP integration (Level 2)

**Deferred** with R8 (PRD-004). Bruno acts as an MCP client: `initialize` →
`tools/list` → `tools/call query_graph` for a `ready` id → assert on-topic
answer. Added once the front MCP + graphify-mcp microservice exist.

## Keys, work-status, and the graph visualizer

Three enrichments exercised by the containerized suites (`ci/run.sh`):

**Provisioned SSH keys (keyId flow).** The private two-repo suites no longer post
the key material with every git-add (inline `sshKey`). Instead `ci/run.sh` first
provisions a memory-scoped key once — `POST /api/v1/memories/{id}/keys`
(`{name, sshKey, knownHosts}`) — and references it by id on each git-add
(`{gitRepoUrl, keyId}`). Both repos live on the same host, so one key is reused
across both adds (and stays rotatable via `PUT .../keys/{keyId}`). The response
never echoes key material; only a fingerprint + timestamps are persisted (grep the
committed `memory.json` to confirm no `PRIVATE KEY` string is present). The
single-repo T2 suite stays public and provisions no key.

**Work-status assertions.** `bruno-memory/08-work-status.bru` asserts
`GET /api/v1/memories/{id}/status` reports `lastOperation.status == "succeeded"`
at the memory level and for every resource — the generalized last-operation
record (name/status/started/finished/error). `ci/run.sh`'s `report_memory` also
logs the status **and the full graph structure** at each merge point (a phase
label — e.g. "after first merge (app only)" then "after re-merge (app + deploy)"
in the one-by-one suite — makes the before/after growth self-describing).

**Terminal graph visualizer.** `graphify graph-summary <dir-or-graph.json>
[--json]` renders a merged graph's shape: totals (nodes/edges/communities),
per-repo node-count bars, and a per-community breakdown (size, spanned repos,
sample labels) — the cross-repo, two-community story. Two renderings land in the
test log: (1) `report_memory` reimplements the same ASCII **in jq** (the ci
container has no python/graphify) directly from `GET .../graph`, printed at each
merge so the graph is visible as the memory grows; and (2) `run-integration.sh`
runs the real `graphify graph-summary` host-side after the suite over the
bind-mounted memory graphs via a one-off graphify container. Unit-tested in
`tests/test_graph_summary.py`.

## Running

`tests/integration/run.sh`:
1. `docker compose -f docker-compose.yaml -f tests/integration/docker-compose.test.yaml up -d --build nats graphify-api graphify-cloner graphify-worker`
2. Wait for `/readyz`.
3. `bru run tests/integration/bruno --env local` (Bruno CLI).
4. Capture exit code; `docker compose … down -v` on exit.

### Containerized suite (reproducible, Docker-only)

`run.sh`/`run-memory.sh` are for **manual** runs — they need `bru`/`curl`/`jq`
on the host. For a reproducible run needing only Docker, use the containerized
suite. `run-integration.sh` is the engine and defaults to the **single-repo**
(public, keyless) memory suite; `run-memory-2repos.sh` is a thin wrapper that
re-invokes it pinned to the **two-repo** private correlation suite (SSH deploy
key required):

```
./tests/integration/run-integration.sh                 # SUITE=memory — 1 repo (default)
./tests/integration/run-memory-2repos.sh               # SUITE=memory2 — 2 private repos + SSH key
SUITE=repo    ./tests/integration/run-integration.sh   # T1 repository API only
SUITE=all     ./tests/integration/run-integration.sh   # everything (needs the deploy key)

# key selection for the private (memory2/all) suites:
SSH_KEY_FILE=~/.ssh/id_ed25519 ./tests/integration/run-memory-2repos.sh
PRIVATE=0 ./tests/integration/run-memory-2repos.sh     # public repos, no key
```

`docker-compose-integration.yaml` `include:`s the base stack + test override and
adds an **`integration-tests`** service — a small image (bash + curl + jq +
`@usebruno/cli`) whose **`command` is a set of shell commands (`ci/run.sh`)
delivered via a read-only `/suite` volume mount**, so editing a `.bru` spec or the
runner needs no image rebuild. The runner reaches services by compose DNS name
(`http://graphify-api:8080`), not host ports. `--exit-code-from integration-tests`
makes the runner's exit code the suite result; the wrapper builds the `graphify`
base first, then tears the stack down (unless `KEEP_STACK=1`).

CI: `.github/workflows/integration.yml` runs `run-integration.sh` as a **parallel
matrix** over `SUITE ∈ {repo, memory, memory2, memory2seq}` on PRs touching
`backend/**`, `graphify/**`, `tests/**`, compose, `Dockerfile`, or `pyproject.toml`
(kept separate from the image-build workflows). The public suites (`repo`,
`memory`) run keyless; the private two-repo suites (`memory2`, `memory2seq`)
consume the `SSH_DEPLOY_KEY` repo secret (an SSH key with read access to both
private repos), written to a 0600 runner-temp file and passed as `SSH_KEY_FILE` —
never committed, never printed. Each matrix job writes its before/after-merge
`graphify graph-summary` output to `$GITHUB_STEP_SUMMARY` and uploads it as an
artifact; a final job aggregates all suites into **one sticky PR comment** (found
+ updated by an HTML marker via `gh api`, so re-runs edit the same comment) using
only the built-in `GITHUB_TOKEN` (`pull-requests: write`).

## Non-goals

- Load/perf testing; private-repo (SSH) integration (public-first).
