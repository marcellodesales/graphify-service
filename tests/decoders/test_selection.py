"""Decoder registry, selection order, classifier hook, and CLI overrides.

Selection resolution order (base.select_decoder):
  --no-specialized -> generic; --decoder/GRAPHIFY_DECODER -> named (forced);
  installed classifier -> its choice; else highest positive can_decode
  (priority then registration order); else generic fallback.
"""
from __future__ import annotations

from pathlib import Path

import pytest

from graphify.decoders import (
    GenericAstDecoder,
    RepoContext,
    build_repo_context,
    registered_decoders,
    select_decoder,
)
from graphify.decoders.base import VionixRepoDecoder


class _Fake(VionixRepoDecoder):
    def __init__(self, name, score, priority=0):
        self.name = name
        self.priority = priority
        self._score = score

    def can_decode(self, ctx):
        return self._score

    def builder(self, ctx):  # pragma: no cover - not exercised in selection tests
        raise AssertionError("builder() should not be called during selection")


def _ctx() -> RepoContext:
    return RepoContext(root=Path("/repo"))


def _generic():
    return GenericAstDecoder()


def test_builtins_are_registered_generic_first():
    names = [d.name for d in registered_decoders()]
    assert "generic" in names and "cloud-native" in names
    # Generic registered first so it is the final tie-break fallback.
    assert names.index("generic") < names.index("cloud-native")


def test_highest_positive_score_wins():
    g = _generic()
    a = _Fake("a", 0.3)
    b = _Fake("b", 0.8)
    chosen = select_decoder(_ctx(), decoders=[g, a, b])
    assert chosen is b


def test_all_zero_falls_back_to_generic():
    g = _generic()
    a = _Fake("a", 0.0)
    chosen = select_decoder(_ctx(), decoders=[g, a])
    assert chosen is g


def test_priority_breaks_score_ties():
    g = _generic()
    lo = _Fake("lo", 0.5, priority=1)
    hi = _Fake("hi", 0.5, priority=9)
    chosen = select_decoder(_ctx(), decoders=[g, lo, hi])
    assert chosen is hi


def test_registration_order_breaks_priority_ties():
    g = _generic()
    first = _Fake("first", 0.5, priority=5)
    second = _Fake("second", 0.5, priority=5)
    chosen = select_decoder(_ctx(), decoders=[g, first, second])
    assert chosen is first  # earlier registration wins


def test_no_specialized_forces_generic():
    g = _generic()
    winner = _Fake("winner", 0.99)
    chosen = select_decoder(_ctx(), decoders=[g, winner], disable_specialized=True)
    assert chosen is g


def test_decoder_override_forces_named_even_with_zero_score():
    g = _generic()
    z = _Fake("zero", 0.0)
    chosen = select_decoder(_ctx(), decoders=[g, z], override="zero")
    assert chosen is z


def test_unknown_override_raises():
    g = _generic()
    with pytest.raises(ValueError):
        select_decoder(_ctx(), decoders=[g], override="does-not-exist")


def test_env_override_honored(monkeypatch):
    g = _generic()
    z = _Fake("zero", 0.0)
    monkeypatch.setenv("GRAPHIFY_DECODER", "zero")
    chosen = select_decoder(_ctx(), decoders=[g, z])
    assert chosen is z


def test_cli_override_beats_env(monkeypatch):
    g = _generic()
    a = _Fake("a", 0.0)
    b = _Fake("b", 0.0)
    monkeypatch.setenv("GRAPHIFY_DECODER", "a")
    chosen = select_decoder(_ctx(), decoders=[g, a, b], override="b")
    assert chosen is b


def test_classifier_choice_wins_over_scoring():
    g = _generic()
    scorer = _Fake("scorer", 0.9)
    picked = _Fake("picked", 0.0)

    class C:
        def classify(self, ctx, candidates):
            return picked

    chosen = select_decoder(_ctx(), decoders=[g, scorer, picked], classifier=C())
    assert chosen is picked


def test_classifier_none_defers_to_scoring():
    g = _generic()
    scorer = _Fake("scorer", 0.9)

    class C:
        def classify(self, ctx, candidates):
            return None

    chosen = select_decoder(_ctx(), decoders=[g, scorer], classifier=C())
    assert chosen is scorer


def test_broken_classifier_falls_back_to_scoring():
    g = _generic()
    scorer = _Fake("scorer", 0.9)

    class C:
        def classify(self, ctx, candidates):
            raise RuntimeError("boom")

    chosen = select_decoder(_ctx(), decoders=[g, scorer], classifier=C())
    assert chosen is scorer


def test_broken_can_decode_is_skipped_not_fatal():
    g = _generic()

    class Boom(VionixRepoDecoder):
        name = "boom"
        priority = 0

        def can_decode(self, ctx):
            raise RuntimeError("nope")

        def builder(self, ctx):  # pragma: no cover
            raise AssertionError

    ok = _Fake("ok", 0.4)
    chosen = select_decoder(_ctx(), decoders=[g, Boom(), ok])
    assert chosen is ok


def test_build_repo_context_flattens_census(tmp_path):
    (tmp_path / "a.py").write_text("x = 1\n")
    (tmp_path / "b.yaml").write_text("k: v\n")
    files_by_type = {
        "code": [str(tmp_path / "a.py"), str(tmp_path / "b.yaml")],
        "document": [],
    }
    ctx = build_repo_context(
        root=tmp_path,
        files_by_type=files_by_type,
        code_files=[tmp_path / "a.py"],
        cache_root=tmp_path,
        extract_kwargs={"max_workers": 2},
    )
    # census = full corpus; code_files = run subset.
    assert len(ctx.census_files) == 2
    assert ctx.code_files == (tmp_path / "a.py",)
    assert ctx.has_file("b.yaml")
    # extract_call_kwargs reproduces the CLI's own extract() call shape.
    kw = ctx.extract_call_kwargs()
    assert kw["cache_root"] == tmp_path and kw["root"] == tmp_path
    assert kw["max_workers"] == 2
