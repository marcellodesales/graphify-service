"""Regression lock: the generic decoder reproduces bare extract() exactly.

The whole point of the decoder layer is that ordinary code repos are unchanged.
GenericAstDecoder's builder must be byte-for-byte equivalent (same node ids, same
(source, target, relation) edges) to the pre-decoder AST seam call:
``extract(code_files, cache_root=out_root, root=target)``.
"""
from __future__ import annotations

from pathlib import Path

from graphify.decoders import GenericAstDecoder, build_repo_context, empty_result
from graphify.extract import extract


def _write(p: Path, text: str) -> Path:
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(text, encoding="utf-8")
    return p


def _node_ids(result) -> set[str]:
    return {n["id"] for n in result["nodes"]}


def _edge_keys(result) -> set[tuple]:
    return {(e["source"], e["target"], e["relation"]) for e in result["edges"]}


def test_generic_builder_matches_bare_extract(tmp_path):
    py = _write(tmp_path / "pkg" / "mod.py",
                "def foo():\n    return bar()\n\ndef bar():\n    return 1\n")
    js = _write(tmp_path / "web" / "app.js",
                "export function greet(name) { return hello(name); }\n"
                "function hello(n) { return n; }\n")
    code_files = [py, js]

    ctx = build_repo_context(
        root=tmp_path,
        files_by_type={"code": [str(py), str(js)]},
        code_files=code_files,
        cache_root=tmp_path,
    )
    via_decoder = GenericAstDecoder().builder(ctx).build()
    direct = extract(code_files, cache_root=tmp_path, root=tmp_path)

    assert _node_ids(via_decoder) == _node_ids(direct)
    assert _edge_keys(via_decoder) == _edge_keys(direct)


def test_generic_builder_empty_code_is_noop(tmp_path):
    ctx = build_repo_context(
        root=tmp_path,
        files_by_type={"document": [str(tmp_path / "README.md")]},
        code_files=[],
        cache_root=tmp_path,
    )
    # No code files (docs-only / incremental no-op): empty result, never crash.
    assert GenericAstDecoder().builder(ctx).build() == empty_result()
