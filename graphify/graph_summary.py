"""graph_summary — a terminal community/repo visualizer for a graph.json.

Reads a graphify ``graph.json`` (a single per-repo extract or a merged,
multi-repo graph) and renders either a human-readable ASCII summary or a
machine-readable JSON stats object.

The merged graph produced by ``graphify merge-graphs`` prefixes every node id
with ``<repo_tag>::`` and stamps a ``repo`` attribute, plus ``community`` (int)
and ``community_name`` on clustered nodes. This module groups by those to show
the cross-repo community story ("which repos does each community span?") that the
raw ``N nodes / M edges / K communities`` line can't convey.

It reads the JSON directly (no networkx dependency) so it works on any
graph.json and stays cheap enough for CI logs. Used by humans and shelled out to
by the integration harness once a memory is ``ready``.
"""

from __future__ import annotations

import json
from collections import Counter, defaultdict
from pathlib import Path
from typing import Any

# How many example labels / spanned repos to list per community in human output.
_TOP_LABELS = 5
_TOP_REPOS = 6
_BAR_WIDTH = 30  # max width, in chars, of an ASCII count bar


def resolve_graph_path(path_or_dir: str | Path) -> Path:
    """Resolve a user-supplied path to an actual graph.json file.

    Accepts the graph.json itself, a directory containing it, or a directory
    whose ``graphify-out/graph.json`` holds it (the on-disk layout a memory
    resource / merged memory uses). Raises FileNotFoundError otherwise.
    """
    p = Path(path_or_dir)
    if p.is_file():
        return p
    candidates = [p / "graph.json", p / "graphify-out" / "graph.json"]
    for c in candidates:
        if c.is_file():
            return c
    raise FileNotFoundError(
        f"no graph.json at {p} (looked for {', '.join(str(c) for c in candidates)})"
    )


def load_graph_json(path_or_dir: str | Path) -> dict[str, Any]:
    """Load and lightly normalize a graph.json into a {nodes, edges} dict."""
    path = resolve_graph_path(path_or_dir)
    raw = json.loads(path.read_text(encoding="utf-8"))
    nodes = raw.get("nodes", []) or []
    # graphify's extract output keys edges as "links"; some paths use "edges".
    edges = raw.get("links")
    if edges is None:
        edges = raw.get("edges", [])
    return {"nodes": nodes, "edges": edges or []}


def _repo_of(node: dict[str, Any]) -> str:
    """The repo tag a node belongs to: explicit ``repo`` attr, else the
    ``<tag>::`` id prefix (merged graphs), else ``(local)`` for a per-repo graph.
    """
    repo = node.get("repo")
    if repo:
        return str(repo)
    nid = str(node.get("id", ""))
    if "::" in nid:
        return nid.split("::", 1)[0]
    return "(local)"


def _community_key(node: dict[str, Any]) -> Any:
    c = node.get("community")
    return c if c is not None else "(none)"


def compute_summary(data: dict[str, Any]) -> dict[str, Any]:
    """Compute totals, per-repo, and per-community stats from a loaded graph."""
    nodes = data["nodes"]
    edges = data["edges"]

    per_repo: Counter[str] = Counter()
    file_types: Counter[str] = Counter()
    node_types: Counter[str] = Counter()

    comm_nodes: dict[Any, list[dict[str, Any]]] = defaultdict(list)
    comm_name: dict[Any, str] = {}

    for n in nodes:
        per_repo[_repo_of(n)] += 1
        if n.get("file_type"):
            file_types[str(n["file_type"])] += 1
        if n.get("node_type"):
            node_types[str(n["node_type"])] += 1
        ck = _community_key(n)
        comm_nodes[ck].append(n)
        name = n.get("community_name")
        if name and ck not in comm_name:
            comm_name[ck] = str(name)

    communities = []
    for ck, members in comm_nodes.items():
        repos_spanned = sorted({_repo_of(m) for m in members})
        labels = [str(m.get("label") or m.get("local_id") or m.get("id"))
                  for m in members]
        communities.append({
            "community": ck,
            "name": comm_name.get(ck, "" if ck == "(none)" else str(ck)),
            "size": len(members),
            "repos": repos_spanned,
            "top_labels": labels[:_TOP_LABELS],
        })
    # Largest communities first; a real (int) community outranks the "(none)" bucket.
    communities.sort(key=lambda c: (c["size"], c["community"] != "(none)"), reverse=True)

    # A merged graph has >1 real repo tag; a per-repo extract has just "(local)".
    real_communities = [c for c in communities if c["community"] != "(none)"]

    return {
        "totals": {
            "nodes": len(nodes),
            "edges": len(edges),
            "communities": len(real_communities),
            "repos": len(per_repo),
        },
        "per_repo": [{"repo": r, "nodes": c}
                     for r, c in per_repo.most_common()],
        "per_community": communities,
        "file_types": dict(file_types.most_common()),
        "node_types": dict(node_types.most_common()),
    }


def _bar(count: int, maximum: int) -> str:
    if maximum <= 0:
        return ""
    filled = max(1, round(count / maximum * _BAR_WIDTH)) if count else 0
    return "█" * filled


def render_human(summary: dict[str, Any]) -> str:
    """Render an ASCII summary suitable for a terminal / CI log."""
    t = summary["totals"]
    lines: list[str] = []
    lines.append("Graph summary")
    lines.append("=" * 40)
    lines.append(
        f"{t['nodes']} nodes · {t['edges']} edges · "
        f"{t['communities']} communities · {t['repos']} repo(s)"
    )

    per_repo = summary["per_repo"]
    if per_repo:
        lines.append("")
        lines.append("Per-repo (node counts)")
        lines.append("-" * 40)
        maxr = max(r["nodes"] for r in per_repo)
        width = max(len(r["repo"]) for r in per_repo)
        for r in per_repo:
            lines.append(
                f"  {r['repo']:<{width}}  {_bar(r['nodes'], maxr):<{_BAR_WIDTH}} "
                f"{r['nodes']}"
            )

    communities = summary["per_community"]
    if communities:
        lines.append("")
        lines.append("Communities (largest first)")
        lines.append("-" * 40)
        maxc = max(c["size"] for c in communities)
        for c in communities:
            head = c["name"] or (
                "(unclustered)" if c["community"] == "(none)" else f"#{c['community']}"
            )
            lines.append(f"  {head}")
            lines.append(
                f"    {_bar(c['size'], maxc):<{_BAR_WIDTH}} {c['size']} nodes"
            )
            spans = c["repos"][:_TOP_REPOS]
            extra = len(c["repos"]) - len(spans)
            span_str = ", ".join(spans) + (f" (+{extra} more)" if extra > 0 else "")
            lines.append(f"    repos: {span_str}")
            if c["top_labels"]:
                lines.append(f"    e.g. {', '.join(c['top_labels'])}")

    ft = summary.get("file_types")
    if ft:
        lines.append("")
        lines.append("File types: " + ", ".join(f"{k}={v}" for k, v in ft.items()))

    return "\n".join(lines) + "\n"


def summarize(path_or_dir: str | Path, as_json: bool = False) -> str:
    """Load a graph and return its rendered summary (JSON or human ASCII)."""
    summary = compute_summary(load_graph_json(path_or_dir))
    if as_json:
        return json.dumps(summary, ensure_ascii=False, indent=2) + "\n"
    return render_human(summary)
