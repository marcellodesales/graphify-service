# Repo-level decoders (`VionixRepoDecoder` → `GraphBuilder`)

The graphify library extracts a graph **per file** (`extract._DISPATCH` maps one
extractor to each extension). That is the right granularity for source code, but
some whole-repo shapes extract to a poor or empty graph under the generic AST
pipeline — Infrastructure-as-Code (Kubernetes YAML, kustomize, helm,
docker-compose), notebook-only repos, data-only repos, or languages without an
extractor. An empty graph is a **hard failure** downstream (`graphify extract`
exits non-zero on a zero-node build), which fails the whole memory in the Go
worker.

This package adds a repo-level layer *above* the per-file dispatch. A
`VionixRepoDecoder` inspects the whole cloned repo and returns a `GraphBuilder`
that produces a coherent graph following graphify's schema standard. This is
"vionix's specialized ways of decoding any github repository."

## The contract

```
VionixRepoDecoder
  .name: str                       # stable id for --decoder / GRAPHIFY_DECODER
  .priority: int                   # tie-break; generic = -1
  .can_decode(ctx) -> float        # 0.0..1.0 self-score; cheap, side-effect-free
  .builder(ctx) -> GraphBuilder

GraphBuilder
  .build() -> {"nodes", "edges", "input_tokens", "output_tokens"}   # schema-valid
```

`RepoContext` (see `base.py`) carries two distinct file sets:

- **`census_files`** — the FULL live corpus (every classified file). `can_decode`
  reads this to recognize a repo shape.
- **`code_files`** — the run's dispatch subset. On a full scan this is every code
  file; on an **incremental** scan it is only the *changed* code files. Builders
  consume **only** this.

> **Incremental-safety rule:** a builder must never synthesize nodes from
> `census_files` when `code_files` is empty. Classify on the full repo; build on
> the changed subset. Both built-in builders return an empty result when
> `code_files` is empty, preserving the CLI's incremental no-op gate.

## Selection order (`select_decoder`)

1. `--no-specialized` → generic fallback.
2. `--decoder NAME` (or `GRAPHIFY_DECODER` env) → that named decoder, **forced**
   regardless of score (errors, exit 2, if the name is unknown).
3. an installed `RepoClassifier` (future, possibly LLM, selector) that commits to
   a decoder.
4. highest positive `can_decode` score among specialized decoders (priority, then
   registration order, break ties).
5. generic fallback.

A broken `can_decode` or classifier is logged and skipped — it never fails
extraction.

## Built-in decoders

- **`generic`** (`generic.py`, priority −1, `can_decode`→0.0): the always-available
  fallback. Its builder issues the **exact** `extract(code_files,
  cache_root=out_root, root=target[, max_workers=…])` call the CLI made before the
  decoder layer existed, so ordinary code repos are **byte-for-byte unchanged**
  (locked by `tests/decoders/test_generic_parity.py`).
- **`cloud-native`** (`cloud_native.py`, priority 10): recognizes k8s / compose /
  kustomize / helm repos and emits a rich structural graph — a node per Kubernetes
  resource, compose service, kustomization, and referenced container image, with
  `contains`/`references` edges. Non-manifest files in the repo are delegated to
  the generic `extract()` pass and merged in.

## Two-communities rule (no cross-repo bridge in the library)

Correlating a memory's repos is a **product concern**, deferred to the MCP query
layer and future vionix devsecops nodes — not something the core extractor should
hard-code. So the `cloud-native` decoder emits **file-scoped** image ids
(`_make_id(stem, "image", <normalized-ref>)`, where `stem` is the repo-relative
path). The same image referenced by an app repo and a deploy repo therefore gets
**distinct** ids, and merging a memory's repos yields **two separate
communities**.

The globally-stable image bridge that *intentionally* collapses the same image
across repos (the correlation reference implementation) lives only in
[`use-cases/cloud-native/yaml_config.py`](../../use-cases/cloud-native/README.md)
— loaded by path in its own test, never wired into `extract._DISPATCH` or this
registry. The structural parser here (`_extract_cloud_native_structural`) is
parameterized by an `image_id_fn`, so the use-case *could* import it and pass the
global-bridge strategy without duplicating the parser.

No secrets are ever emitted — only structural identifiers (kinds, names, image
references), never `Secret.data` / env values / tokens.

## CLI

```
graphify extract <path> [--decoder NAME] [--no-specialized]
```

The Go service never passes `--decoder`, so the `graphify extract` shell-out
contract is unchanged; auto-selection applies there. The flag/env override exists
for rollout and debugging.

## Adding a decoder

1. Subclass `VionixRepoDecoder` in a new module; implement `can_decode` (score
   from `ctx.census_files`) and `builder`.
2. Register it in `__init__.py` via `register_decoder(...)`, after `generic`.
3. Keep the builder driven strictly by `ctx.code_files`; return `empty_result()`
   when it is empty.
4. Validate output with `graphify.validate.validate_extraction` in a test under
   `tests/decoders/`.
