"""`graphify graph-summary` — terminal per-repo + per-community visualizer.

Covers the summary logic directly (compute/render) and the CLI subcommand
end-to-end (human ASCII + --json), against a merged two-repo, two-community
fixture that mirrors what `merge-graphs` produces for a memory.
"""
from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import pytest

from graphify.graph_summary import (
    compute_summary,
    load_graph_json,
    render_human,
    resolve_graph_path,
    summarize,
)

PYTHON = sys.executable


def _run(args, cwd):
    return subprocess.run([PYTHON, "-m", "graphify"] + args, cwd=cwd,
                          capture_output=True, text=True)


# A merged graph: two repos (backend, frontend), two communities that each span
# both repos — the cross-repo story the summary is meant to make visible.
MERGED = {
    "directed": False,
    "multigraph": False,
    "nodes": [
        {"id": "backend::api", "repo": "backend", "local_id": "api",
         "label": "api.go", "file_type": "code", "community": 0,
         "community_name": "http-layer"},
        {"id": "backend::db", "repo": "backend", "local_id": "db",
         "label": "db.go", "file_type": "code", "community": 1,
         "community_name": "storage"},
        {"id": "frontend::client", "repo": "frontend", "local_id": "client",
         "label": "client.ts", "file_type": "code", "community": 0,
         "community_name": "http-layer"},
        {"id": "frontend::store", "repo": "frontend", "local_id": "store",
         "label": "store.ts", "file_type": "code", "community": 1,
         "community_name": "storage"},
        {"id": "frontend::misc", "repo": "frontend", "local_id": "misc",
         "label": "misc.ts", "file_type": "code"},  # no community -> unclustered
    ],
    "links": [
        {"source": "backend::api", "target": "frontend::client"},
        {"source": "backend::db", "target": "frontend::store"},
    ],
}


def _write_merged(tmp_path: Path) -> Path:
    p = tmp_path / "graphify-out" / "graph.json"
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(json.dumps(MERGED), encoding="utf-8")
    return p


def test_compute_summary_totals_and_grouping(tmp_path):
    data = load_graph_json(_write_merged(tmp_path))
    s = compute_summary(data)

    assert s["totals"]["nodes"] == 5
    assert s["totals"]["edges"] == 2
    assert s["totals"]["repos"] == 2
    # Two real communities (the unclustered node is NOT counted as a community).
    assert s["totals"]["communities"] == 2

    per_repo = {r["repo"]: r["nodes"] for r in s["per_repo"]}
    assert per_repo == {"backend": 2, "frontend": 3}

    # Each real community spans BOTH repos; the summary must surface that.
    real = [c for c in s["per_community"] if c["community"] != "(none)"]
    assert len(real) == 2
    for c in real:
        assert c["repos"] == ["backend", "frontend"], c
    names = {c["name"] for c in real}
    assert names == {"http-layer", "storage"}


def test_repo_prefix_fallback_when_no_repo_attr(tmp_path):
    # A merged graph may carry only the `<tag>::` id prefix (no `repo` attr).
    g = {"nodes": [{"id": "alpha::x"}, {"id": "beta::y"}], "links": []}
    p = tmp_path / "g.json"
    p.write_text(json.dumps(g), encoding="utf-8")
    s = compute_summary(load_graph_json(p))
    assert {r["repo"] for r in s["per_repo"]} == {"alpha", "beta"}


def test_per_repo_extract_is_local(tmp_path):
    # A single-repo extract (no prefixes, no repo attr) groups under "(local)".
    g = {"nodes": [{"id": "x", "label": "x"}], "edges": []}
    p = tmp_path / "g.json"
    p.write_text(json.dumps(g), encoding="utf-8")
    s = compute_summary(load_graph_json(p))
    assert s["per_repo"] == [{"repo": "(local)", "nodes": 1}]
    assert s["totals"]["edges"] == 0  # "edges" key normalized like "links"


def test_render_human_smoke(tmp_path):
    out = render_human(compute_summary(load_graph_json(_write_merged(tmp_path))))
    assert "Graph summary" in out
    assert "5 nodes" in out and "2 edges" in out
    assert "http-layer" in out and "storage" in out
    assert "backend" in out and "frontend" in out


def test_resolve_graph_path_accepts_dir(tmp_path):
    _write_merged(tmp_path)
    # A directory containing graphify-out/graph.json resolves to the file.
    assert resolve_graph_path(tmp_path).name == "graph.json"
    # The graphify-out dir directly resolves too.
    assert resolve_graph_path(tmp_path / "graphify-out").name == "graph.json"
    with pytest.raises(FileNotFoundError):
        resolve_graph_path(tmp_path / "nope")


def test_summarize_json_mode(tmp_path):
    txt = summarize(_write_merged(tmp_path), as_json=True)
    parsed = json.loads(txt)
    assert parsed["totals"]["communities"] == 2


def test_cli_human_and_json(tmp_path):
    p = _write_merged(tmp_path)

    r = _run(["graph-summary", str(p)], tmp_path)
    assert r.returncode == 0, r.stderr
    assert "Graph summary" in r.stdout
    assert "http-layer" in r.stdout

    rj = _run(["graph-summary", str(p), "--json"], tmp_path)
    assert rj.returncode == 0, rj.stderr
    parsed = json.loads(rj.stdout)
    assert parsed["totals"]["nodes"] == 5
    assert parsed["totals"]["communities"] == 2

    # A directory argument works too (resolves graphify-out/graph.json).
    rd = _run(["graph-summary", str(tmp_path), "--json"], tmp_path)
    assert rd.returncode == 0, rd.stderr
    assert json.loads(rd.stdout)["totals"]["repos"] == 2


def test_cli_missing_file_errors(tmp_path):
    r = _run(["graph-summary", str(tmp_path / "missing")], tmp_path)
    assert r.returncode == 1
    assert "error" in r.stderr.lower()
