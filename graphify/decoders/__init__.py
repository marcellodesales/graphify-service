"""Repo-level specialized graph builders (``VionixRepoDecoder`` → ``GraphBuilder``).

Importing this package registers the built-in decoders (generic fallback +
cloud-native) into the module-level registry, so ``select_decoder`` sees them
without the caller wiring anything. New decoders register themselves the same
way (append to ``_REGISTRY`` via ``register_decoder``); registration order is the
final tie-break in selection, so the generic fallback is registered first and
specialized decoders after.
"""

from __future__ import annotations

from graphify.decoders.base import (
    GENERIC_DECODER_NAME,
    GraphBuilder,
    RepoClassifier,
    RepoContext,
    VionixRepoDecoder,
    build_repo_context,
    empty_result,
    get_classifier,
    merge_results,
    register_decoder,
    registered_decoders,
    select_decoder,
    set_classifier,
)
from graphify.decoders.cloud_native import CloudNativeDecoder
from graphify.decoders.generic import GenericAstDecoder

# Register built-ins once, at import time. Generic first (fallback, lowest
# priority), then specialized decoders.
register_decoder(GenericAstDecoder())
register_decoder(CloudNativeDecoder())

__all__ = [
    "GENERIC_DECODER_NAME",
    "GraphBuilder",
    "RepoClassifier",
    "RepoContext",
    "VionixRepoDecoder",
    "GenericAstDecoder",
    "CloudNativeDecoder",
    "build_repo_context",
    "empty_result",
    "get_classifier",
    "merge_results",
    "register_decoder",
    "registered_decoders",
    "select_decoder",
    "set_classifier",
]
