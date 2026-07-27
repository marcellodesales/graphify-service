#!/usr/bin/env bash
# Integration test T3 (memory abstraction, cross-repo correlation) — ONE-BY-ONE.
#
# Build ONE memory from TWO related private repos added SEQUENTIALLY: add the app
# repo, wait for the memory to reach ready, THEN add the deploy repo and wait for
# the memory to re-merge and reach ready again. Proves a memory grows
# incrementally — appending a resource to a ready memory re-triggers the merge
# (worker merge fingerprint) — using the SAME HTTP APIs, with no backend change.
#
#   - azure-chatgpt-spa-service        (app):    docker-compose.yml BUILDS the image
#   - azure-chatgpt-spa-service-deploy (deploy): kustomize base DEPLOYS that image
#
# The final unified/correlated graph is identical to the "together" run: both
# repos present, bridged by the shared docker image reference (extracted from YAML
# by the yaml_config extractor, --code-only). This is the counterpart to
# run-memory-2repos-together.sh. Thin wrapper over run-integration.sh pinned to
# SUITE=memory2seq.
#
#   ./tests/integration/run-memory-2repos-1by1.sh
#   SSH_KEY_FILE=~/.ssh/id_ed25519 ./tests/integration/run-memory-2repos-1by1.sh
#   PRIVATE=0 ./tests/integration/run-memory-2repos-1by1.sh   # public repos, no key
#   KEEP_STACK=1 ./tests/integration/run-memory-2repos-1by1.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

# Two-repo correlation suite, repos added one at a time; private unless overridden.
export SUITE=memory2seq
export PRIVATE="${PRIVATE:-1}"

exec "$HERE/run-integration.sh" "$@"
