"""Generic AST decoder — the always-available fallback.

Reproduces the pre-decoder pipeline exactly: its builder issues the same
``extract(code_files, cache_root=out_root, root=target[, max_workers=...])`` call
the CLI made at the AST seam, and returns the same ``ast_result`` dict. This is
the byte-for-byte parity guarantee that lets the decoder layer be introduced with
no behavior change for ordinary code repos.

``can_decode`` returns ``0.0`` so the generic decoder never wins the specialized
selection race; it is chosen only as the explicit fallback (no specialized
decoder scored positive, or ``--no-specialized``/``--decoder generic``).
"""

from __future__ import annotations

from graphify.decoders.base import (
    GENERIC_DECODER_NAME,
    GraphBuilder,
    RepoContext,
    VionixRepoDecoder,
    empty_result,
)


class _GenericAstBuilder:
    """Builder that delegates wholesale to ``graphify.extract.extract``."""

    def __init__(self, ctx: RepoContext) -> None:
        self._ctx = ctx

    def build(self) -> dict:
        ctx = self._ctx
        # Empty code list (docs-only corpus, or an incremental no-op) must not
        # reach extract() — skip cleanly, exactly like the CLI's `if code_files:`
        # guard. Never synthesize from census_files here.
        if not ctx.code_files:
            return empty_result()
        from graphify.extract import extract as _extract
        return _extract(list(ctx.code_files), **ctx.extract_call_kwargs())


class GenericAstDecoder(VionixRepoDecoder):
    """The default decoder: whole-corpus per-file AST extraction."""

    name = GENERIC_DECODER_NAME
    priority = -1  # never outranks a specialized decoder

    def can_decode(self, ctx: RepoContext) -> float:
        return 0.0

    def builder(self, ctx: RepoContext) -> GraphBuilder:
        return _GenericAstBuilder(ctx)
