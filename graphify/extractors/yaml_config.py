"""Generic YAML config extractor — domain-agnostic, non-LLM, structural.

Mirrors ``json_config.py``: a YAML file becomes a file node plus a shallow pass
over the top-level keys of each document (``contains`` edges). That is enough to
keep a YAML-only repo (a kustomize deploy repo, a compose stack, a config tree)
from extracting to an empty graph in ``--code-only`` mode — so it merges into its
own community like any other repo.

Deliberately dumb about *meaning*: it does not know what a Kubernetes resource, a
docker-compose service, or a container image is, and it does not try to correlate
repos. Correlating a memory's repos is a product concern handled later (the MCP
query layer, and the planned vionix devsecops SDLC nodes), not something the core
extractor should hard-code. The cloud-native image-correlation heuristic lives
outside the library in ``use-cases/cloud-native/yaml_config.py``.
"""
from __future__ import annotations


from pathlib import Path
from graphify.extractors.base import _file_stem, _make_id
from graphify.ids import normalize_id


_YAML_MAX_BYTES = 1_048_576  # 1 MiB — skip huge generated manifests / data blobs
_MAX_KEYS_PER_DOC = 200      # cap so a large doc can't explode into orphan nodes


def extract_yaml(path: Path) -> dict:
    """Extract shallow structure from a YAML file.

    Always emits at least a file node, so any YAML file yields a non-empty graph
    (defeating the empty-graph hard-exit for YAML-only repos in ``--code-only``).
    For each document (``safe_load_all`` handles multi-doc streams) that is a
    mapping, emit a node per top-level key with a ``contains`` edge from the file.
    Nested structure, lists, and scalar values are intentionally not walked —
    that keeps generic config meaningfully non-empty without producing hundreds
    of orphan value-nodes (same rationale as ``json_config``'s data-JSON guard).
    """
    try:
        import yaml
    except ImportError:
        return {"nodes": [], "edges": [], "error": "pyyaml not installed. Run: pip install pyyaml"}

    try:
        # Bounded read (read one byte past the cap to detect oversized files).
        with path.open("rb") as f:
            raw = f.read(_YAML_MAX_BYTES + 1)
        if len(raw) > _YAML_MAX_BYTES:
            return {"nodes": [], "edges": [], "error": "yaml file too large to index"}
        text = raw.decode("utf-8", errors="replace")
    except Exception as e:
        return {"nodes": [], "edges": [], "error": str(e)}

    str_path = str(path)
    stem = _file_stem(path)
    file_nid = _make_id(str_path)

    nodes: list[dict] = [{"id": file_nid, "label": path.name, "file_type": "code",
                          "source_file": str_path, "source_location": None}]
    edges: list[dict] = []
    seen_ids: set[str] = {file_nid}

    def add_node(nid: str, label: str) -> None:
        if nid and nid not in seen_ids:
            seen_ids.add(nid)
            nodes.append({"id": nid, "label": label, "file_type": "code",
                          "source_file": str_path, "source_location": None})

    def add_edge(src: str, tgt: str, relation: str) -> None:
        if not src or not tgt or src == tgt:
            return
        edges.append({"source": src, "target": tgt, "relation": relation,
                      "confidence": "EXTRACTED", "source_file": str_path,
                      "source_location": None, "weight": 1.0})

    try:
        docs = list(yaml.safe_load_all(text))
    except Exception:
        # A malformed document still leaves us the file node — never fail hard.
        docs = []

    count = 0
    for doc in docs:
        if not isinstance(doc, dict):
            continue
        for k in doc:
            if count >= _MAX_KEYS_PER_DOC:
                break
            if not isinstance(k, str) or not normalize_id(k):
                continue
            knid = _make_id(stem, k)
            if not knid:
                continue
            add_node(knid, k)
            add_edge(file_nid, knid, "contains")
            count += 1

    return {"nodes": nodes, "edges": edges}
