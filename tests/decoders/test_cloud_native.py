"""Cloud-native decoder: rich structural graph, file-scoped images, two communities.

Verifies the library decoder gives IaC repos a coherent structural graph (k8s
resources, compose services, kustomizations, image refs) AND that its image nodes
are file-scoped — the same image in two repos gets DISTINCT ids, so merging a
memory's repos yields two separate communities. The globally-collapsing image
bridge stays out of the library (see use-cases/cloud-native/).
"""
from __future__ import annotations

from pathlib import Path

import pytest

from graphify.decoders import CloudNativeDecoder, build_repo_context, select_decoder
from graphify.extractors.base import _make_id
from graphify.validate import validate_extraction

pytest.importorskip("yaml")

IMAGE = "registry.example.com/gpt/spa-service"


def _write(p: Path, text: str) -> Path:
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(text, encoding="utf-8")
    return p


def _k8s_repo(root: Path) -> dict:
    dep = _write(root / "deploy" / "deployment.yaml",
        "apiVersion: apps/v1\n"
        "kind: Deployment\n"
        "metadata:\n  name: spa\n"
        "spec:\n  template:\n    spec:\n      containers:\n"
        f"        - name: spa\n          image: {IMAGE}:v1.0.0\n")
    compose = _write(root / "docker-compose.yml",
        "services:\n"
        "  spa:\n"
        f"    image: {IMAGE}:build-42\n")
    kust = _write(root / "overlay" / "kustomization.yaml",
        "images:\n"
        f"  - name: {IMAGE}\n    newTag: prod\n")
    app = _write(root / "src" / "app.py",
        "def handler():\n    return helper()\n\ndef helper():\n    return 1\n")
    code = [dep, compose, kust, app]
    return {
        "files_by_type": {"code": [str(p) for p in code]},
        "code_files": code,
    }


def _ctx(root: Path):
    spec = _k8s_repo(root)
    return build_repo_context(
        root=root,
        files_by_type=spec["files_by_type"],
        code_files=spec["code_files"],
        cache_root=root,
    )


def _image_ids(result) -> set[str]:
    return {n["id"] for n in result["nodes"] if "image" in n["id"]}


def test_selects_cloud_native_for_k8s_repo(tmp_path):
    ctx = _ctx(tmp_path)
    assert select_decoder(ctx).name == "cloud-native"


def test_selects_generic_for_plain_python_repo(tmp_path):
    app = _write(tmp_path / "app.py", "x = 1\n")
    ctx = build_repo_context(
        root=tmp_path,
        files_by_type={"code": [str(app)]},
        code_files=[app],
        cache_root=tmp_path,
    )
    assert select_decoder(ctx).name == "generic"


def test_can_decode_scores(tmp_path):
    dec = CloudNativeDecoder()
    # Strong filename signal.
    assert dec.can_decode(_ctx(tmp_path)) >= 0.9
    # Plain YAML with no manifest markers -> 0.0.
    plain = _write(tmp_path / "cfg" / "settings.yaml", "database:\n  host: localhost\n")
    plain_ctx = build_repo_context(
        root=tmp_path / "cfg",
        files_by_type={"code": [str(plain)]},
        code_files=[plain],
        cache_root=tmp_path / "cfg",
    )
    assert dec.can_decode(plain_ctx) == 0.0
    # No YAML at all -> 0.0.
    py = _write(tmp_path / "py" / "m.py", "x=1\n")
    py_ctx = build_repo_context(
        root=tmp_path / "py",
        files_by_type={"code": [str(py)]},
        code_files=[py],
        cache_root=tmp_path / "py",
    )
    assert dec.can_decode(py_ctx) == 0.0


def test_rich_structural_graph(tmp_path):
    ctx = _ctx(tmp_path)
    result = CloudNativeDecoder().builder(ctx).build()
    labels = {n["label"] for n in result["nodes"]}
    # k8s resource, compose service, kustomization all present.
    assert "Deployment/spa" in labels
    assert "spa" in labels           # compose service
    assert "kustomization" in labels
    # image node carries the normalized ref as its label.
    assert IMAGE in labels
    # delegated AST pass produced the .py symbols too (labels carry "()").
    assert any(n["label"].startswith("handler") for n in result["nodes"])


def test_image_ids_are_file_scoped_not_global(tmp_path):
    ctx = _ctx(tmp_path)
    result = CloudNativeDecoder().builder(ctx).build()
    norm = IMAGE  # already tag-free
    global_id = _make_id("image", norm)  # the use-case's global-bridge form
    ids = _image_ids(result)
    assert ids, "expected at least one image node"
    # No node uses the global bridge id (that lives only in the use-case).
    assert global_id not in {n["id"] for n in result["nodes"]}
    # Every image id is file-scoped (includes a path component before 'image').
    for iid in ids:
        assert iid != global_id


def test_same_image_two_repos_distinct_ids(tmp_path):
    # Realistic two-repo memory: an APP repo builds the image (compose at root) and
    # a DEPLOY repo deploys it (k8s manifest under a subdir). Their file paths
    # differ, so the file-scoped image ids differ — a plain merge keeps them as two
    # communities (no library-level bridge). The globally-collapsing bridge stays
    # in use-cases/cloud-native only.
    app_root = tmp_path / "app-repo"
    compose = _write(app_root / "docker-compose.yml",
        f"services:\n  spa:\n    image: {IMAGE}:build-42\n")
    deploy_root = tmp_path / "deploy-repo"
    dep = _write(deploy_root / "k8s" / "deployment.yaml",
        "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: spa\n"
        "spec:\n  template:\n    spec:\n      containers:\n"
        f"        - name: spa\n          image: {IMAGE}:v1.0.0\n")

    app_ctx = build_repo_context(
        root=app_root, files_by_type={"code": [str(compose)]},
        code_files=[compose], cache_root=app_root)
    deploy_ctx = build_repo_context(
        root=deploy_root, files_by_type={"code": [str(dep)]},
        code_files=[dep], cache_root=deploy_root)

    ia = _image_ids(CloudNativeDecoder().builder(app_ctx).build())
    ib = _image_ids(CloudNativeDecoder().builder(deploy_ctx).build())
    assert ia and ib
    # Two communities: the same image gets distinct file-scoped ids per repo.
    assert ia.isdisjoint(ib)
    # And neither uses the global-bridge form that would collapse them.
    global_id = _make_id("image", IMAGE)
    assert global_id not in ia and global_id not in ib


def test_schema_conformance(tmp_path):
    ctx = _ctx(tmp_path)
    result = CloudNativeDecoder().builder(ctx).build()
    assert validate_extraction(result) == []


def test_empty_code_is_noop(tmp_path):
    ctx = build_repo_context(
        root=tmp_path,
        files_by_type={"code": []},
        code_files=[],
        cache_root=tmp_path,
    )
    result = CloudNativeDecoder().builder(ctx).build()
    assert result["nodes"] == [] and result["edges"] == []
