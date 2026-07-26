#!/usr/bin/env bash
#
# Host entrypoint for the reproducible containerized integration suite.
#
# Unlike run.sh / run-memory.sh (which need bru/curl/jq on the host and are for
# manual runs), this needs only Docker: it builds the `graphify` base, then brings
# up the stack plus the in-container `integration-tests` runner, whose exit code
# becomes the suite result.
#
#   ./tests/integration/run-integration.sh                 # SUITE=all
#   SUITE=memory  ./tests/integration/run-integration.sh   # T2 only
#   SUITE=memory2 ./tests/integration/run-integration.sh   # T3 only
#   SUITE=repo    ./tests/integration/run-integration.sh   # T1 only
#   KEEP_STACK=1  ./tests/integration/run-integration.sh   # don't tear down after
#
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

FILE="tests/integration/docker-compose-integration.yaml"
COMPOSE=(docker compose -f "$FILE")
# App services to bring up (graphify-service watch + neo4j intentionally excluded).
SVCS=(nats graphify-api graphify-cloner graphify-worker graphify-memory-worker graphify-mcp)
KEEP="${KEEP_STACK:-0}"

# The worker/memory-worker images are FROM the fresh local graphify base.
export GRAPHIFY_IMAGE="${GRAPHIFY_IMAGE:-marcellodesales/graphify:latest}"

cleanup() {
  if [ "$KEEP" = "1" ]; then
    echo "==> KEEP_STACK=1 — leaving stack up (tear down: ${COMPOSE[*]} down -v)"
    return
  fi
  echo "==> tearing down"
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Fresh state + dirs the base compose bind-mounts.
rm -rf "$ROOT/data/repos"
mkdir -p "$ROOT/data/repos" "$ROOT/secrets/ssh"

echo "==> building graphify base image (provides extract + merge-graphs)"
"${COMPOSE[@]}" build graphify

echo "==> running integration suite (SUITE=${SUITE:-all})"
set +e
"${COMPOSE[@]}" up --build --abort-on-container-exit \
  --exit-code-from integration-tests \
  "${SVCS[@]}" integration-tests
code=$?
set -e

if [ "$code" -eq 0 ]; then
  echo "==> INTEGRATION SUITE PASSED"
else
  echo "==> INTEGRATION SUITE FAILED (exit $code)"
fi
exit "$code"
