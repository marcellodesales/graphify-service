# Use-case: cloud-native correlation

This directory holds domain-specific knowledge that is **deliberately kept out of
the general-purpose graphify library**. The core library extracts and merges
graphs; it does not know what a container image is, and it does not decide how a
memory's repos should correlate.

## What lives here

`yaml_config.py` — `extract_cloud_native_yaml(path)`: a structural (non-LLM)
extractor for Kubernetes / docker-compose / kustomize manifests. Beyond the file
and structure nodes the library already produces, it emits a **globally-stable,
tag-normalized container-image node**:

- id `image::<normalized-ref>` — **no** file / dir / repo component;
- tag- and digest-agnostic (`svc:build-42`, `svc:v1.0.0`, `svc@sha256:…` all
  collapse to `svc`), so a build tag on one side matches a release tag on the other;
- `file_type="code"` so the backend correlator (`backend/internal/memory/correlate.go`)
  will hub it.

Because `merge-graphs` keys nodes by id, that single image node is the same object
whether it came from an **app** repo (which *builds* the image, in
`docker-compose.yml`) or a **deploy** repo (which *deploys* it, in
`deployment.yaml` + `kustomization.yaml`). Merging the two graphs therefore
bridges the two repos through that one image.

## Why it is a use-case, not library code

Correlation is a product concern, not an extraction concern. The current service
merges a memory's repos into **two separate communities** and leaves them
uncorrelated on purpose. Correlation is expected to come later from:

1. the **MCP query layer** (a question may relate nodes across communities at
   query time), and
2. the planned **vionix devsecops nodes** — a general-purpose SDLC memory that
   correlates any resource (build, deploy, image, service, …) without a hard-coded
   per-domain heuristic.

Baking a docker-image heuristic into `graphify/extractors/yaml_config.py` would
freeze one correlation policy into every graphify run. Keeping it here documents
the cloud-native correlation path and lets it evolve independently.

## Status / wiring

- **Not** registered in `graphify.extract._DISPATCH`. The running memory worker
  uses the library's generic YAML extractor, so the live service produces two
  communities (no image bridge) today.
- Verified at the tests level by `tests/test_cloud_native_yaml.py`, which loads
  this module by path and asserts the image id collapses identically across a k8s
  Deployment, a docker-compose service, and a kustomize `images:` override.
- To activate it in a future build, add an extractor-plugin hook to graphify and
  register `extract_cloud_native_yaml` for `.yaml`/`.yml` from this directory.
