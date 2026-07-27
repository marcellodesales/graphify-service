#!/usr/bin/env bash
# Integration test T3 (memory abstraction, cross-repo correlation): build ONE
# memory out of TWO related git repos and prove the merged/correlated unified
# graph can answer a question that spans both — "when a feature is implemented,
# how is it made available in kubernetes and where?".
#
#   - azure-chatgpt-spa-service        (the UI / app): its docker-compose.yml
#     BUILDS the UI docker image
#       viasat-ai-platform.docker.artifactory.viasat.com/gpt/services/openai/azure-openai-spa-service
#   - azure-chatgpt-spa-service-deploy (the deploy infra): a kustomize base whose
#     deployment-base.yaml DEPLOYS that same image in kubernetes (port 3000),
#     wired in via base/kustomization.yaml.
#
# The docker image reference is the entity that links the two repos: built in
# the app repo, deployed in kubernetes by the deploy repo via the kustomization.
# After merge, the memory's unified graph contains BOTH repos, and graphify's
# correlator surfaces shared entities across sources.
#
# NOTE: the memory worker runs graphify in --code-only mode (local AST, no LLM
# key), so query_graph does structural/keyword matching, not natural-language
# reasoning. The bruno-memory-2repos collection therefore asserts the *guarantees*
# (memory ready, both repos merged, non-empty graph/answers); deep cross-repo
# content is reported for visibility without making the build flaky.
#
# These app/deploy repos are PRIVATE, so this suite clones over SSH with a deploy
# key. It is a thin wrapper over the shared containerized engine
# (run-integration.sh) pinned to the two-repo suite — same compose stack, same
# in-container runner (ci/run.sh), same key-injection path. Run-integration.sh
# itself defaults to the single-repo suite; this one is the two-repo counterpart.
#
#   ./tests/integration/run-memory-2repos.sh
#   SSH_KEY_FILE=~/.ssh/id_ed25519 ./tests/integration/run-memory-2repos.sh
#   PRIVATE=0 ./tests/integration/run-memory-2repos.sh       # public repos, no key
#   KEEP_STACK=1 ./tests/integration/run-memory-2repos.sh    # leave the stack up
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

# Two-repo correlation suite; private (SSH deploy key) unless overridden.
export SUITE=memory2
export PRIVATE="${PRIVATE:-1}"

exec "$HERE/run-integration.sh" "$@"
