"""Repo-level specialized graph builders — the ``VionixRepoDecoder`` seam.

The graphify library extracts a graph **per file** (``extract._DISPATCH`` maps one
extractor to each extension). That is the right granularity for source code, but
some whole-repo shapes extract to a poor or empty graph under the generic AST
pipeline (IaC: kubernetes YAML, terraform, helm; notebook-only or data-only
repos). An empty graph is a hard failure downstream (``cli.py`` exits non-zero on
a zero-node build), so those repos fail the whole memory in the Go worker.

This package adds a repo-level layer *above* the per-file dispatch: a
``VionixRepoDecoder`` inspects the whole cloned repo and returns a
``GraphBuilder`` that produces a coherent graph following graphify's schema
standard. The generic fallback (``GenericAstDecoder``) reproduces today's
behavior byte-for-byte; specialized decoders (first: cloud-native) give IaC repos
a rich structural graph instead of a shallow one.

Design mirrors ``graphify/resolver_registry.py``: a frozen registry plus
``register``/``registered`` plus a small driver, with a strict one-way dependency
``cli -> decoders -> extract/extractors/ids/validate``. This module knows nothing
about any specific domain — decoders register themselves (see ``__init__``) so a
new decoder plugs in without editing this seam.

Selection is pluggable: an injected ``RepoClassifier`` (a future, possibly LLM,
selector) wins; otherwise a ``--decoder``/``GRAPHIFY_DECODER`` override forces a
choice; otherwise each decoder self-scores via ``can_decode(ctx) -> float`` and
the highest positive score wins (priority breaks ties); otherwise the generic
fallback runs. ``--no-specialized`` forces generic.

Correctness note on incremental runs: classification reads ``census_files`` (the
FULL live corpus) while builders consume ``code_files`` (the run's dispatch
subset, which is only the *changed* files under an incremental scan). A builder
must never synthesize nodes from ``census_files`` when ``code_files`` is empty, or
it would break the incremental no-op gate and re-emit unchanged content.
"""

from __future__ import annotations

import abc
import logging
import os
from collections import Counter
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Protocol, runtime_checkable

_LOG = logging.getLogger(__name__)

# Name of the generic fallback decoder. Kept as a constant so selection can find
# the fallback without importing ``generic`` (which imports this module).
GENERIC_DECODER_NAME = "generic"

# Environment override, honored by ``select_decoder`` when no explicit override is
# passed. The CLI ``--decoder`` flag takes precedence over this.
_ENV_DECODER = "GRAPHIFY_DECODER"

_READ_MAX_BYTES = 65_536  # bounded sniff read for can_decode heuristics


@dataclass(frozen=True)
class RepoContext:
    """Immutable view of a repo for decoder selection and graph building.

    ``census_files`` is the FULL live corpus (every classified file, across all
    types) — decoders inspect it in ``can_decode`` to recognize a repo shape.
    ``code_files`` is the run's dispatch subset (all code files on a full scan,
    only the *changed* code files on an incremental scan) — builders consume it.
    ``doc_files``/``paper_files``/``image_files`` are carried for completeness so a
    future specialized builder can reason about them, but the current builders use
    only ``code_files`` (the semantic pass owns docs/papers/images downstream).

    ``root`` anchors ids/``source_file`` relativization; ``cache_root`` is the
    graphify-out cache location; ``extract_kwargs`` carries any extra kwargs for
    ``extract()`` (e.g. ``max_workers``) so a builder's delegated call matches the
    CLI's own call exactly.
    """

    root: Path
    census_files: tuple[Path, ...] = ()
    code_files: tuple[Path, ...] = ()
    doc_files: tuple[Path, ...] = ()
    paper_files: tuple[Path, ...] = ()
    image_files: tuple[Path, ...] = ()
    cache_root: Path | None = None
    extract_kwargs: Mapping[str, object] = field(default_factory=dict)

    def extract_call_kwargs(self) -> dict:
        """Kwargs for a generic ``extract()`` call anchored like the CLI's own.

        Reproduces ``extract(code_files, cache_root=out_root, root=target,
        [max_workers=...])`` — the exact shape used at the AST seam in
        ``cli.py`` — so a builder that delegates to ``extract()`` is byte-for-byte
        equivalent to the pre-decoder pipeline.
        """
        kwargs: dict = {"cache_root": self.cache_root, "root": self.root}
        kwargs.update(self.extract_kwargs)
        return kwargs

    def extension_census(self) -> Counter:
        """Case-insensitive count of file suffixes across the full corpus."""
        return Counter(p.suffix.lower() for p in self.census_files)

    def has_file(self, name: str) -> bool:
        """True if any census file's basename equals ``name`` (case-insensitive)."""
        target = name.lower()
        return any(p.name.lower() == target for p in self.census_files)

    def census_by_suffix(self, *suffixes: str) -> list[Path]:
        """Census files whose suffix is one of ``suffixes`` (case-insensitive)."""
        wanted = {s.lower() for s in suffixes}
        return [p for p in self.census_files if p.suffix.lower() in wanted]

    def read_text(self, path: Path, *, max_bytes: int = _READ_MAX_BYTES) -> str:
        """Bounded, error-tolerant read used by ``can_decode`` sniff heuristics."""
        try:
            with Path(path).open("rb") as f:
                raw = f.read(max_bytes)
            return raw.decode("utf-8", errors="replace")
        except Exception:
            return ""


@runtime_checkable
class GraphBuilder(Protocol):
    """Produces a schema-valid extraction result for one repo.

    ``build()`` returns the ``ast_result``-shaped dict the CLI expects at the AST
    seam: ``{"nodes": [...], "edges": [...], "input_tokens": int,
    "output_tokens": int}``. Nodes/edges must conform to the graphify schema
    (``validate.validate_extraction``).
    """

    def build(self) -> dict: ...


class VionixRepoDecoder(abc.ABC):
    """Super-type for repo-level specialized graph builders.

    A decoder recognizes a repo shape (``can_decode``) and, when selected,
    provides a ``GraphBuilder`` (``builder``). ``name`` is the stable identifier
    used by the ``--decoder``/``GRAPHIFY_DECODER`` override; ``priority`` breaks
    ties between decoders that return the same ``can_decode`` score.
    """

    #: Stable identifier for overrides and logging.
    name: str = ""
    #: Tie-break weight; higher wins when scores are equal. Generic uses -1.
    priority: int = 0

    @abc.abstractmethod
    def can_decode(self, ctx: RepoContext) -> float:
        """Self-score in ``[0.0, 1.0]``: confidence this decoder fits ``ctx``.

        ``0.0`` means "not my repo" (the generic fallback always returns 0.0 so it
        never wins the specialized race but is always available as the default).
        Must be cheap and side-effect-free — it may be called for every registered
        decoder on every repo.
        """

    @abc.abstractmethod
    def builder(self, ctx: RepoContext) -> GraphBuilder:
        """Return the builder that will produce this repo's graph."""


# ── registry ─────────────────────────────────────────────────────────────────
# Module-level, populated by callers via register_decoder() (see __init__).
_REGISTRY: list[VionixRepoDecoder] = []


def register_decoder(decoder: VionixRepoDecoder) -> VionixRepoDecoder:
    """Append a decoder to the global registry and return it (for inline use)."""
    _REGISTRY.append(decoder)
    return decoder


def registered_decoders() -> list[VionixRepoDecoder]:
    """Return a copy of the registered decoders, in registration order."""
    return list(_REGISTRY)


# ── classifier hook (future LLM plug-point) ──────────────────────────────────
@runtime_checkable
class RepoClassifier(Protocol):
    """Optional selector that overrides self-scoring.

    A classifier is given the repo context and the candidate decoders and returns
    the chosen decoder, or ``None`` to defer to score-based selection. This is the
    seam a future (possibly LLM) repo classifier plugs into — the same capability
    the plan refers to as "what was built before may certainly be that classifier".
    """

    def classify(
        self, ctx: RepoContext, candidates: Sequence[VionixRepoDecoder]
    ) -> VionixRepoDecoder | None: ...


_CLASSIFIER: RepoClassifier | None = None


def set_classifier(classifier: RepoClassifier | None) -> None:
    """Install (or clear, with ``None``) the global repo classifier."""
    global _CLASSIFIER
    _CLASSIFIER = classifier


def get_classifier() -> RepoClassifier | None:
    """Return the installed classifier, if any."""
    return _CLASSIFIER


# ── selection ────────────────────────────────────────────────────────────────
def _find_generic(candidates: Sequence[VionixRepoDecoder]) -> VionixRepoDecoder:
    """Return the generic fallback decoder from ``candidates``.

    Prefers the decoder named ``generic``; falls back to the lowest-priority
    decoder so selection still terminates if the fallback was renamed.
    """
    for d in candidates:
        if d.name == GENERIC_DECODER_NAME:
            return d
    if not candidates:
        raise LookupError("no decoders registered; cannot select a fallback")
    return min(candidates, key=lambda d: d.priority)


def select_decoder(
    ctx: RepoContext,
    *,
    override: str | None = None,
    disable_specialized: bool = False,
    decoders: Sequence[VionixRepoDecoder] | None = None,
    classifier: RepoClassifier | None = None,
    use_env: bool = True,
) -> VionixRepoDecoder:
    """Choose the decoder for ``ctx``.

    Resolution order:
      1. ``disable_specialized`` (``--no-specialized``) -> generic fallback.
      2. ``override`` (``--decoder NAME``) or ``GRAPHIFY_DECODER`` env -> that
         named decoder, forced regardless of score (errors if unknown).
      3. an injected/installed ``RepoClassifier`` that returns a decoder.
      4. highest positive ``can_decode`` score among specialized decoders,
         priority then registration order breaking ties.
      5. generic fallback.
    """
    candidates = list(decoders) if decoders is not None else registered_decoders()
    generic = _find_generic(candidates)

    if disable_specialized:
        return generic

    # Override: CLI flag first, then env. Forced regardless of score.
    forced = override
    if forced is None and use_env:
        forced = os.environ.get(_ENV_DECODER) or None
    if forced:
        for d in candidates:
            if d.name == forced:
                return d
        available = ", ".join(sorted(d.name for d in candidates)) or "(none)"
        raise ValueError(
            f"unknown decoder {forced!r}; available: {available}"
        )

    # Classifier hook wins over self-scoring when it commits to a decoder.
    active_classifier = classifier if classifier is not None else _CLASSIFIER
    if active_classifier is not None:
        try:
            chosen = active_classifier.classify(ctx, candidates)
        except Exception as exc:  # a broken classifier must not fail extraction
            _LOG.warning("repo classifier failed, falling back to scoring: %s", exc)
            chosen = None
        if chosen is not None:
            return chosen

    # Self-scoring: highest positive score among the specialized decoders.
    best: VionixRepoDecoder | None = None
    best_key: tuple[float, int, int] | None = None
    for idx, d in enumerate(candidates):
        if d is generic:
            continue
        try:
            score = float(d.can_decode(ctx))
        except Exception as exc:  # a broken decoder must not fail extraction
            _LOG.warning("%s.can_decode failed, skipping: %s", d.name, exc)
            continue
        if score <= 0.0:
            continue
        # Higher score wins; then higher priority; then earlier registration
        # (negative idx so a smaller index sorts as the larger key).
        key = (score, d.priority, -idx)
        if best_key is None or key > best_key:
            best, best_key = d, key

    return best if best is not None else generic


def build_repo_context(
    *,
    root: Path,
    files_by_type: Mapping[str, Sequence] | None = None,
    code_files: Sequence | None = None,
    doc_files: Sequence | None = None,
    paper_files: Sequence | None = None,
    image_files: Sequence | None = None,
    cache_root: Path | None = None,
    extract_kwargs: Mapping[str, object] | None = None,
) -> RepoContext:
    """Assemble a ``RepoContext`` from the CLI's already-computed corpus.

    ``files_by_type`` is the detect() map of the FULL live corpus (type ->
    paths); it becomes ``census_files``. ``code_files`` is the run's dispatch
    subset. All path inputs may be ``str`` or ``Path``.
    """
    def _paths(seq: Sequence | None) -> tuple[Path, ...]:
        return tuple(Path(p) for p in (seq or ()))

    census: list[Path] = []
    if files_by_type:
        for group in files_by_type.values():
            census.extend(Path(p) for p in (group or ()))

    return RepoContext(
        root=Path(root),
        census_files=tuple(census),
        code_files=_paths(code_files),
        doc_files=_paths(doc_files),
        paper_files=_paths(paper_files),
        image_files=_paths(image_files),
        cache_root=Path(cache_root) if cache_root is not None else None,
        extract_kwargs=dict(extract_kwargs or {}),
    )


# Shape of the empty extraction result, shared by builders for the no-work case.
def empty_result() -> dict:
    """The ``ast_result``-shaped dict for an empty/no-op build."""
    return {"nodes": [], "edges": [], "input_tokens": 0, "output_tokens": 0}


def merge_results(*results: dict) -> dict:
    """Merge extraction results, de-duping nodes by id and edges by identity.

    Nodes are keyed by ``id`` (first wins). Edges are keyed by
    ``(source, target, relation)`` (first wins). Token counts are summed. Used by
    specialized builders that combine a structural pass with a delegated
    ``extract()`` pass.
    """
    nodes: list[dict] = []
    edges: list[dict] = []
    seen_nodes: set[str] = set()
    seen_edges: set[tuple] = set()
    in_tok = 0
    out_tok = 0
    for r in results:
        if not r:
            continue
        for n in r.get("nodes", []):
            nid = n.get("id")
            if nid is None or nid in seen_nodes:
                continue
            seen_nodes.add(nid)
            nodes.append(n)
        for e in r.get("edges", []):
            key = (e.get("source"), e.get("target"), e.get("relation"))
            if key in seen_edges:
                continue
            seen_edges.add(key)
            edges.append(e)
        in_tok += int(r.get("input_tokens", 0) or 0)
        out_tok += int(r.get("output_tokens", 0) or 0)
    return {"nodes": nodes, "edges": edges,
            "input_tokens": in_tok, "output_tokens": out_tok}
