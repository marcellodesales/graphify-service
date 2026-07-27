"""Cloud-native decoder — rich structural graphs for k8s / compose / kustomize.

IaC repos (Kubernetes manifests, docker-compose stacks, kustomize overlays, helm
charts) extract to only a *shallow* file + top-level-key graph under the generic
YAML extractor (``graphify/extractors/yaml_config.py``). This decoder recognizes
those repos and builds a coherent structural graph: a node per Kubernetes
resource, per compose service, per kustomization, plus a node per referenced
container image — with ``contains``/``references`` edges. Everything else in the
repo is delegated to the generic ``extract()`` pass and merged in, so a mixed
repo (manifests + a little code) still gets full AST coverage.

Two-communities rule (deliberate — see ``use-cases/cloud-native/``)
-------------------------------------------------------------------
The library must NOT correlate repos. Image nodes here are **file-scoped**:
``_make_id(stem, "image", <normalized-ref>)`` where ``stem`` is the repo-relative
path. The same image referenced by two different repos (or two files) therefore
gets **distinct** ids — merging a memory's repos yields two separate communities.
The globally-stable image bridge that intentionally collapses across repos lives
only in ``use-cases/cloud-native/yaml_config.py`` (loaded by path, never wired
into the library). The structural core here is parameterized by ``image_id_fn`` so
that use-case *could* later import it and pass the global-bridge strategy without
duplicating the parser.

No secrets: only structural identifiers (kinds, names, image references) are
emitted — never ``Secret.data`` / env values / tokens.
"""

from __future__ import annotations

from pathlib import Path
from typing import Callable

from graphify.decoders.base import (
    GraphBuilder,
    RepoContext,
    VionixRepoDecoder,
    empty_result,
    merge_results,
)
from graphify.extractors.base import _make_id
from graphify.ids import normalize_id

_YAML_SUFFIXES = frozenset({".yaml", ".yml"})
_YAML_MAX_BYTES = 1_048_576  # 1 MiB — skip huge generated manifests / data blobs
_CONTAINER_KEYS = frozenset({"containers", "initContainers", "ephemeralContainers"})

# Filenames that are near-certain cloud-native signals regardless of content.
_STRONG_NAMES = frozenset({
    "kustomization.yaml", "kustomization.yml",
    "chart.yaml", "chart.yml",
})
_COMPOSE_PREFIXES = ("docker-compose", "compose")


def _normalize_image(ref: str) -> str:
    """Reduce a container-image reference to a tag-agnostic identity.

    Strips a ``@sha256:…`` digest and a trailing ``:tag`` from the **last path
    segment only**, so a registry ``host:port`` is preserved:

        host:5000/gpt/svc:v1.0.0   -> host:5000/gpt/svc
        host/gpt/svc@sha256:abc     -> host/gpt/svc
    """
    ref = ref.strip()
    if not ref:
        return ""
    ref = ref.split("@", 1)[0]  # drop digest
    head, _, last = ref.rpartition("/")
    last = last.split(":", 1)[0]  # drop tag from the final segment only
    norm = f"{head}/{last}" if head else last
    return norm.strip()


def _rel(path: Path, root: Path | None) -> tuple[str, str]:
    """Return (source_file, stem) repo-relative to ``root``.

    Matches the portable form ``extract()`` produces for delegated files: ids and
    ``source_file`` are repo-relative, so on-disk location never leaks and two
    repos with the same layout still get distinct-per-repo ids only where their
    paths differ. Falls back to the bare name when ``path`` is outside ``root``.
    """
    p = Path(path)
    rel = p
    if root is not None:
        try:
            rel = p.resolve().relative_to(Path(root).resolve())
        except Exception:
            rel = Path(p.name)
    else:
        rel = Path(p.name)
    return rel.as_posix(), rel.with_suffix("").as_posix()


def _default_image_id(stem: str, norm: str) -> str:
    """File-scoped image id — the two-communities default (no global bridge)."""
    return _make_id(stem, "image", norm)


def _extract_cloud_native_structural(
    path: Path,
    root: Path | None,
    *,
    image_id_fn: Callable[[str, str], str] = _default_image_id,
) -> dict | None:
    """Parse one YAML file into k8s/compose/kustomize structure.

    Returns an extraction-result dict (file node + resource/service/image nodes)
    when the file contains at least one recognized manifest document; returns
    ``None`` otherwise so the caller routes the file to the generic YAML pass
    (preserving parity for plain-config YAML).
    """
    try:
        import yaml
    except ImportError:
        return None

    try:
        with Path(path).open("rb") as f:
            raw = f.read(_YAML_MAX_BYTES + 1)
        if len(raw) > _YAML_MAX_BYTES:
            return None
        text = raw.decode("utf-8", errors="replace")
    except Exception:
        return None

    try:
        docs = list(yaml.safe_load_all(text))
    except Exception:
        return None

    str_path, stem = _rel(path, root)
    file_nid = _make_id(stem)
    nodes: list[dict] = [{"id": file_nid, "label": Path(path).name, "file_type": "code",
                          "source_file": str_path, "source_location": None}]
    edges: list[dict] = []
    seen_ids: set[str] = {file_nid}
    seen_edges: set[tuple[str, str, str]] = set()
    recognized = False

    def add_node(nid: str, label: str) -> None:
        if nid and nid not in seen_ids:
            seen_ids.add(nid)
            nodes.append({"id": nid, "label": label, "file_type": "code",
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
        norm = _normalize_image(ref)
        if not norm or not normalize_id(norm):
            return
        iid = image_id_fn(stem, norm)
        add_node(iid, norm)
        add_edge(owner_nid, iid, "references", context="image")

    def iter_container_images(obj):
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

    for doc in docs:
        if not isinstance(doc, dict):
            continue
        kind = doc.get("kind")
        images = doc.get("images")

        # kustomize: `kind: Kustomization`, or any doc carrying an `images:` list.
        if kind == "Kustomization" or isinstance(images, list):
            recognized = True
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
            recognized = True
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
            recognized = True
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

    if not recognized:
        return None
    return {"nodes": nodes, "edges": edges, "input_tokens": 0, "output_tokens": 0}


def _looks_cloud_native(text: str) -> bool:
    """Cheap text sniff: does this YAML look like a k8s/compose/kustomize doc?"""
    if not text:
        return False
    if "\nkind:" in text or text.startswith("kind:"):
        if "apiVersion:" in text or "Kustomization" in text:
            return True
    if "\nservices:" in text or text.startswith("services:"):
        return True
    if "\nimages:" in text or text.startswith("images:"):
        return True
    if "Kustomization" in text:
        return True
    return False


class _CloudNativeBuilder:
    """Structural pass over recognized manifests + generic pass over the rest."""

    def __init__(self, ctx: RepoContext) -> None:
        self._ctx = ctx

    def build(self) -> dict:
        ctx = self._ctx
        if not ctx.code_files:  # incremental no-op / empty corpus
            return empty_result()

        structural: list[dict] = []
        rest: list[Path] = []
        for path in ctx.code_files:
            if path.suffix.lower() in _YAML_SUFFIXES:
                res = _extract_cloud_native_structural(path, ctx.root)
                if res is not None:
                    structural.append(res)
                    continue
            rest.append(path)

        # Delegate everything not recognized as a manifest to the generic pass,
        # anchored exactly like the CLI's own extract() call.
        rest_result = empty_result()
        if rest:
            from graphify.extract import extract as _extract
            rest_result = _extract(list(rest), **ctx.extract_call_kwargs())

        return merge_results(rest_result, *structural)


class CloudNativeDecoder(VionixRepoDecoder):
    """Recognizes and richly decodes Kubernetes / compose / kustomize repos."""

    name = "cloud-native"
    priority = 10

    def can_decode(self, ctx: RepoContext) -> float:
        yaml_files = ctx.census_by_suffix(*_YAML_SUFFIXES)
        if not yaml_files:
            return 0.0

        # Strong filename signals: a kustomization/Chart or a compose file.
        for p in yaml_files:
            n = p.name.lower()
            if n in _STRONG_NAMES or n.startswith(_COMPOSE_PREFIXES):
                return 0.95

        # Otherwise sniff a bounded sample of YAML files for manifest markers.
        sample = yaml_files[:50]
        manifests = sum(1 for p in sample if _looks_cloud_native(ctx.read_text(p)))
        if manifests == 0:
            return 0.0
        ratio = manifests / len(sample)
        return min(1.0, 0.5 + 0.45 * ratio)

    def builder(self, ctx: RepoContext) -> GraphBuilder:
        return _CloudNativeBuilder(ctx)
