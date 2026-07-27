"""Cloud-native YAML extractor (Kubernetes / docker-compose / kustomize).

USE-CASE MODULE — intentionally **not** part of the general-purpose graphify
library. The core library's YAML extractor (``graphify/extractors/yaml_config.py``)
is domain-agnostic: it emits a file node plus shallow top-level structure so a
YAML-only repo is non-empty and merges into its own community. It does **not**
know what a container image is, and it does **not** try to correlate repos.

This module adds the cloud-native knowledge: it reads k8s / compose / kustomize
manifests and emits a **globally-stable, tag-normalized container-image node**
whose id has no file/dir/repo component. Because ``merge-graphs`` keys nodes by
id, the same image referenced by an *app* repo (which *builds* it, in
``docker-compose.yml``) and a *deploy* repo (which *deploys* it, in
``deployment.yaml`` + ``kustomization.yaml``) collapses to ONE node — the bridge
that correlates the two repos.

Why it's a use-case and not in the library
------------------------------------------
Correlating a memory's repos is a product concern, not an extraction concern.
The current service deliberately leaves the two repos as two separate communities
after merge; correlation is expected to come *later* from the MCP query layer and
from the planned vionix devsecops nodes (a general-purpose SDLC memory). Baking a
docker-image heuristic into the core extractor would hard-code one correlation
policy into every graphify run. So this lives here as the reference implementation
of the cloud-native correlation use-case, exercised by ``tests/test_cloud_native_yaml.py``.

It reuses only library *primitives* (``_make_id`` / ``_file_stem`` / ``normalize_id``);
it is loaded by path in the test, never wired into ``graphify.extract._DISPATCH``.
"""
from __future__ import annotations


from pathlib import Path
from graphify.extractors.base import _file_stem, _make_id
from graphify.ids import normalize_id


_YAML_MAX_BYTES = 1_048_576  # 1 MiB — skip huge generated manifests / data blobs
_CONTAINER_KEYS = frozenset({"containers", "initContainers", "ephemeralContainers"})


def _normalize_image(ref: str) -> str:
    """Reduce a container-image reference to a tag-agnostic identity.

    Strips a ``@sha256:…`` digest and a trailing ``:tag`` **from the last path
    segment only**, so a registry ``host:port`` is preserved but ``:build-123``
    vs ``:v1.0.0`` collapse to the same identity — letting the same image match
    across an app repo and a deploy repo regardless of tag.

        host:5000/gpt/svc:v1.0.0        -> host:5000/gpt/svc
        host/gpt/svc@sha256:abc         -> host/gpt/svc
        host/gpt/svc:build-42           -> host/gpt/svc
    """
    ref = ref.strip()
    if not ref:
        return ""
    ref = ref.split("@", 1)[0]  # drop digest
    head, _, last = ref.rpartition("/")
    last = last.split(":", 1)[0]  # drop tag from the final segment only
    norm = f"{head}/{last}" if head else last
    return norm.strip()


def extract_cloud_native_yaml(path: Path) -> dict:
    """Extract Kubernetes / docker-compose / kustomize structure from a YAML file.

    Always emits at least a file node, so any YAML file yields a non-empty graph.
    Recognized flavors additionally emit:

    - **Kubernetes** (``apiVersion`` + ``kind``): a resource node per document and
      an image node per container ``image:``.
    - **kustomize** (``kind: Kustomization`` or an ``images:`` list): a
      kustomization node and an image node per ``images:`` override
      (``newName``/``name`` + ``newTag``).
    - **docker-compose** (a ``services:`` map): a node per service and an image
      node per service ``image:``.
    - **plain config**: a shallow top-level-key pass so generic YAML is still
      meaningfully non-empty.

    The image node is the correlation key: id ``image::<normalized-ref>`` with no
    file component, ``file_type="code"`` so the backend correlator will hub it.
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
    seen_edges: set[tuple[str, str, str]] = set()

    def add_node(nid: str, label: str, file_type: str = "code") -> None:
        if nid and nid not in seen_ids:
            seen_ids.add(nid)
            nodes.append({"id": nid, "label": label, "file_type": file_type,
                          "source_file": str_path, "source_location": None})

    def add_edge(src: str, tgt: str, relation: str, context: str | None = None) -> None:
        if not src or not tgt or src == tgt:
            return
        key = (src, tgt, relation)
        if key in seen_edges:
            return
        seen_edges.add(key)
        edge = {"source": src, "target": tgt, "relation": relation,
                "confidence": "EXTRACTED", "source_file": str_path,
                "source_location": None, "weight": 1.0}
        if context:
            edge["context"] = context
        edges.append(edge)

    def add_image(ref: str, owner_nid: str) -> None:
        """Emit the shared, tag-agnostic image node (the cross-repo bridge)."""
        norm = _normalize_image(ref)
        if not norm or not normalize_id(norm):
            return
        iid = _make_id("image", norm)
        add_node(iid, norm, file_type="code")  # code so the correlator hubs it
        add_edge(owner_nid, iid, "references", context="image")

    def iter_container_images(obj):
        """Yield image strings from any containers/initContainers list within obj."""
        if isinstance(obj, dict):
            for k, v in obj.items():
                if k in _CONTAINER_KEYS and isinstance(v, list):
                    for c in v:
                        if isinstance(c, dict):
                            img = c.get("image")
                            if isinstance(img, str) and img.strip():
                                yield img.strip()
                else:
                    yield from iter_container_images(v)
        elif isinstance(obj, list):
            for it in obj:
                yield from iter_container_images(it)

    try:
        docs = list(yaml.safe_load_all(text))
    except Exception:
        # A malformed document still leaves us the file node — never fail hard.
        docs = []

    for doc in docs:
        if not isinstance(doc, dict):
            continue
        kind = doc.get("kind")
        images = doc.get("images")

        # kustomize: `kind: Kustomization`, or any doc carrying an `images:` list
        # of overrides (kustomization.yaml often omits the explicit kind).
        if kind == "Kustomization" or isinstance(images, list):
            knid = _make_id(stem, "kustomization")
            add_node(knid, "kustomization")
            add_edge(file_nid, knid, "contains")
            if isinstance(images, list):
                for entry in images:
                    if not isinstance(entry, dict):
                        continue
                    name = entry.get("newName") or entry.get("name")
                    tag = entry.get("newTag")
                    if isinstance(name, str) and name.strip():
                        ref = f"{name.strip()}:{tag}" if tag else name.strip()
                        add_image(ref, knid)
            continue

        # Kubernetes resource: apiVersion + kind.
        if "apiVersion" in doc and isinstance(kind, str) and kind:
            meta = doc.get("metadata") if isinstance(doc.get("metadata"), dict) else {}
            name = meta.get("name") if isinstance(meta.get("name"), str) else ""
            rid = _make_id(stem, "k8s", kind, name)
            label = f"{kind}/{name}" if name else kind
            add_node(rid, label)
            add_edge(file_nid, rid, "contains")
            for img in iter_container_images(doc):
                add_image(img, rid)
            continue

        # docker-compose: a `services:` map.
        services = doc.get("services")
        if isinstance(services, dict):
            for sname, svc in services.items():
                if not isinstance(sname, str) or not isinstance(svc, dict):
                    continue
                snid = _make_id(stem, "compose", sname)
                add_node(snid, sname)
                add_edge(file_nid, snid, "contains")
                img = svc.get("image")
                if isinstance(img, str) and img.strip():
                    add_image(img, snid)
            continue

        # Plain config YAML: a shallow top-level-key pass keeps it meaningfully
        # non-empty without exploding nested data into orphan nodes.
        count = 0
        for k in doc:
            if count >= 200:
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
