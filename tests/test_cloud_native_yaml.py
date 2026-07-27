"""Verify the cloud-native YAML use-case (kept OUT of the core library).

The general-purpose library extractor is domain-agnostic; the container-image
correlation heuristic lives in ``use-cases/cloud-native/yaml_config.py`` and is
NOT wired into ``graphify.extract._DISPATCH``. These tests pin its behaviour by
loading it directly by path — proving the capability without giving the library
any docker-image knowledge.

The contract that matters: the same image referenced by an app repo (compose) and
a deploy repo (k8s Deployment + kustomize override) produces the SAME node id, so
``merge-graphs`` collapses it into the single cross-repo bridge.
"""
from __future__ import annotations

import importlib.util
from pathlib import Path

import pytest

yaml = pytest.importorskip("yaml")

_MODULE_PATH = (
    Path(__file__).resolve().parent.parent
    / "use-cases" / "cloud-native" / "yaml_config.py"
)


def _load():
    spec = importlib.util.spec_from_file_location("cloud_native_yaml", _MODULE_PATH)
    assert spec and spec.loader
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _write(p: Path, text: str) -> Path:
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(text, encoding="utf-8")
    return p


def _image_ids(result) -> set[str]:
    return {n["id"] for n in result["nodes"] if n["id"].startswith("image")}


IMAGE = "registry.example.com/gpt/spa-service"


def test_module_is_not_registered_in_library_dispatch():
    # Guard: the cloud-native extractor must stay out of the core dispatch.
    from graphify.extract import _DISPATCH
    for fn in _DISPATCH.values():
        assert getattr(fn, "__name__", "") != "extract_cloud_native_yaml"


def test_compose_and_k8s_and_kustomize_collapse_to_one_image_id(tmp_path):
    mod = _load()

    compose = _write(tmp_path / "docker-compose.yml",
        "services:\n"
        "  spa:\n"
        f"    image: {IMAGE}:build-42\n")
    deployment = _write(tmp_path / "deploy" / "deployment.yaml",
        "apiVersion: apps/v1\n"
        "kind: Deployment\n"
        "metadata:\n  name: spa\n"
        "spec:\n  template:\n    spec:\n      containers:\n"
        f"        - name: spa\n          image: {IMAGE}:v1.0.0\n")
    kustomization = _write(tmp_path / "overlay" / "kustomization.yaml",
        "images:\n"
        f"  - name: {IMAGE}\n    newTag: prod\n")

    ci = _image_ids(mod.extract_cloud_native_yaml(compose))
    di = _image_ids(mod.extract_cloud_native_yaml(deployment))
    ki = _image_ids(mod.extract_cloud_native_yaml(kustomization))

    assert len(ci) == 1 and len(di) == 1 and len(ki) == 1
    # The bridge: identical id across all three flavors despite different tags.
    assert ci == di == ki
    only = next(iter(ci))
    # id has no file/dir/repo component (stable across repos).
    assert "docker-compose" not in only and "deployment" not in only and "overlay" not in only


def test_digest_and_tag_are_stripped_but_registry_port_preserved(tmp_path):
    mod = _load()
    a = _write(tmp_path / "a.yml", "services:\n  s:\n    image: host:5000/gpt/svc:v1\n")
    b = _write(tmp_path / "b.yml", "services:\n  s:\n    image: host:5000/gpt/svc@sha256:abc123\n")
    assert _image_ids(mod.extract_cloud_native_yaml(a)) == _image_ids(mod.extract_cloud_native_yaml(b))
    # registry host:port survived (label carries the normalized ref)
    node = next(n for n in mod.extract_cloud_native_yaml(a)["nodes"] if n["id"].startswith("image"))
    assert node["label"] == "host:5000/gpt/svc"


def test_always_emits_at_least_a_file_node(tmp_path):
    mod = _load()
    # Malformed YAML still yields the file node (never a hard failure / empty graph).
    p = _write(tmp_path / "broken.yaml", "a: [unterminated\n")
    r = mod.extract_cloud_native_yaml(p)
    assert len(r["nodes"]) >= 1
