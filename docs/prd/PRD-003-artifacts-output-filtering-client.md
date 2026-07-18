# PRD-003 — Artifacts, output filtering & local client

**Requirements:** R4 (artifact inventory + zip + filtering), R7 (local client).

**Status update:** the worker now produces the **UI-ready format set** offline (no
LLM): `graph.json` (canonical), `graph.html`, `graph.graphml`, `graph.svg`,
`GRAPH_REPORT.md`, `<repo>-callflow.html` — via `graphify extract --code-only`
then best-effort `cluster-only` + `export {graphml,callflow-html,svg}` (the
graphify image bakes `svg`/`leiden`/`pdf`/`office`/`google`/`postgres` extras).
Beyond the zip, the API serves any single artifact directly for a UI:
`GET /api/v1/repositories/{id}/artifacts/{name}` (allowlisted, correct
content-type). `graph.json`/`GraphML` are the inputs for the future A2UI /
open-knowledge generator (that generator is our UI layer, not graphify).

## Problem

A finished graph produces many files. Clients want either the default set or a
filtered subset, streamed as a single archive, and a local client that writes
them into the current directory.

## Observed output files (from a real graphify-out)

| File | Kind | Default download? |
|---|---|---|
| `graph.json` | core graph | ✅ |
| `graph.html` | interactive viewer | ✅ |
| `GRAPH_REPORT.md` | highlights report | ✅ |
| `manifest.json` | build manifest | ✅ |
| `cost.json` | extraction cost | optional |
| `*-callflow.html` | call-flow view | optional |
| `.graphify_labels.json`, `.graphify_incremental.json`, `.graphify_target.json`, `.graphify_root`, `.graphify_python`, `.graphify_detect_err.txt` | internal state | ❌ (excluded by default) |

## Requirements

### R4 — Artifacts + filtering

1. `GET /api/v1/repositories/{id}/artifacts` → inventory (name, path, mediaType,
   size, sha256) built by the graphify worker and recorded in metadata
   (spec §6.5). Only allowlisted paths are ever surfaced (no `.git`, no source).
2. `GET /api/v1/repositories/{id}/download?format=zip` streams a zip of the
   **default set** (graph.json, graph.html, GRAPH_REPORT.md, manifest.json).
3. **Output filtering**: `?include=graph.json,GRAPH_REPORT.md` (or `?only=…`,
   `?exclude=…`) selects a subset from the allowlist. Unknown/again-listed names
   are ignored with a warning header, never a path escape.
4. **Generation policy**: if producing all default outputs is cheap (code-only
   AST extraction is), the worker generates the full default set and the API
   returns the requested subset. Expensive optional outputs (LLM cost.json,
   large callflow) are opt-in later.
5. Security (spec §6.6): no symlink follow, re-validate every resolved path,
   safe zip entry names, stream (don't buffer), max total size.

### R7 — Local client

1. A small CLI (`graphify-client` or `gref pull`) that: submits a URL, polls
   `/status/{id}` until `ready`, downloads the zip, and **extracts into the
   current working directory** (creating `graphify-out/` if absent), honoring the
   same `--include/--exclude` filters.
2. If the cwd already has `graphify-out/`, the client refuses to overwrite unless
   `--force` (protect user data — matches the "outputs them locally if the
   current directory has the content" intent).
3. Scope: documented here; built after T1 proves the API surface.

## Verification

- Unit: allowlist enforcement, filter parsing, zip traversal protection.
- Integration (T1): download default zip → contains exactly the default set;
  `?include=graph.json` → zip contains only graph.json; `.graphify_*` never present.
