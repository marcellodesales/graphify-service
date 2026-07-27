#!/usr/bin/env bash
# Integration test T3 (memory abstraction, cross-repo correlation) — TOGETHER.
#
# Build ONE memory from TWO related private repos added in the SAME request cycle
# (app + deploy), producing a single merge, and prove the unified/correlated graph
# spans both repos and answers the cross-repo "image" question.
#
#   - azure-chatgpt-spa-service        (app):    docker-compose.yml BUILDS the image
#   - azure-chatgpt-spa-service-deploy (deploy): kustomize base DEPLOYS that image
#
# The shared docker image reference (extracted structurally from YAML by the
# yaml_config extractor, --code-only) is the entity that bridges the two repos.
#
# This is the "added together" counterpart to run-memory-2repos-1by1.sh (which
# adds the repos one at a time). It is a thin wrapper over the containerized
# engine (run-integration.sh) pinned to SUITE=memory2.
#
#   ./tests/integration/run-memory-2repos-together.sh
#   SSH_KEY_FILE=~/.ssh/id_ed25519 ./tests/integration/run-memory-2repos-together.sh
#   PRIVATE=0 ./tests/integration/run-memory-2repos-together.sh   # public repos, no key
#   KEEP_STACK=1 ./tests/integration/run-memory-2repos-together.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

# Two-repo correlation suite, both repos added at once; private unless overridden.
export SUITE=memory2
export PRIVATE="${PRIVATE:-1}"

exec "$HERE/run-integration.sh" "$@"
