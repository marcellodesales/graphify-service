#!/usr/bin/env bash
# Integration test T1 (PRD-005): submit a live public repo → poll → download zip,
# then run the Bruno assertion collection against the reference.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

COMPOSE=(docker compose -f docker-compose.yaml -f tests/integration/docker-compose.test.yaml)
REPO="${SAMPLE_REPO:-https://github.com/githubtraining/hellogitworld}"
KEEP="${KEEP_STACK:-0}"

# Host ports (kept off the common 8080/8081/8082 to avoid local conflicts).
export GRAPHIFY_API_PORT="${GRAPHIFY_API_PORT:-18080}"
export GRAPHIFY_CLONER_PORT="${GRAPHIFY_CLONER_PORT:-18081}"
export GRAPHIFY_WORKER_PORT="${GRAPHIFY_WORKER_PORT:-18082}"
export GRAPHIFY_MCP_PORT="${GRAPHIFY_MCP_PORT:-18083}"
BASE="${BASE_URL:-http://localhost:${GRAPHIFY_API_PORT}}"

cleanup() {
  if [ "$KEEP" != "1" ]; then
    "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
    rm -rf "$ROOT/data/repos" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# Start from a clean slate: the repos bind mount survives `down -v`, and a stale
# `failed` record would be returned idempotently instead of re-running.
rm -rf "$ROOT/data/repos" 2>/dev/null || true

# The containers run as a non-root uid (10001). On Linux CI the bind-mounted
# host dir is owned by the runner, so the containers would get EACCES writing to
# /graphify-service/repos (API /readyz stays 503). Pre-create it world-writable.
# (On Docker Desktop for Mac this is a no-op — ownership is already mapped.)
mkdir -p "$ROOT/data/repos"
chmod -R 0777 "$ROOT/data" 2>/dev/null || true

echo "==> build graphify base (fresh source — provides 'graphify extract')"
"${COMPOSE[@]}" build graphify

echo "==> build + start stack (nats, api, cloner, worker, mcp)"
"${COMPOSE[@]}" up -d --build nats graphify-api graphify-cloner graphify-worker graphify-mcp

echo "==> wait for API /readyz"
for i in $(seq 1 60); do
  if curl -fsS "$BASE/readyz" >/dev/null 2>&1; then break; fi
  sleep 2
  if [ "$i" = 60 ]; then echo "API not ready"; "${COMPOSE[@]}" logs graphify-api; exit 1; fi
done

echo "==> submit $REPO"
ID="$(curl -fsS -X POST "$BASE/api/v1/repositories" \
  -H 'content-type: application/json' \
  -d "{\"githubRepoUrl\":\"$REPO\"}" | jq -r .id)"
echo "    refId=$ID"

echo "==> poll until ready"
for i in $(seq 1 120); do
  ST="$(curl -fsS "$BASE/api/v1/repositories/$ID" | jq -r .status)"
  echo "    [$i] status=$ST"
  case "$ST" in
    ready) break ;;
    failed)
      echo "pipeline FAILED:"; curl -fsS "$BASE/api/v1/repositories/$ID" | jq .
      "${COMPOSE[@]}" logs graphify-cloner graphify-worker; exit 1 ;;
  esac
  sleep 2
  if [ "$i" = 120 ]; then
    echo "timeout waiting for ready"; "${COMPOSE[@]}" logs graphify-cloner graphify-worker; exit 1
  fi
done

echo "==> run Bruno assertions"
BRU=(bru)
if ! command -v bru >/dev/null 2>&1; then BRU=(npx --yes @usebruno/cli); fi
( cd tests/integration/bruno && "${BRU[@]}" run . --env local --env-var "refId=$ID" )

echo "==> materialize produced artifacts to the resources dir"
OUTDIR="${RESOURCES_DIR:-$ROOT/tests/integration/output}/$ID"
mkdir -p "$OUTDIR"
for f in graph.json graph.html graph.graphml graph.svg GRAPH_REPORT.md manifest.json repository-callflow.html; do
  if curl -fsS "$BASE/api/v1/repositories/$ID/artifacts/$f" -o "$OUTDIR/$f" 2>/dev/null; then
    echo "   saved $f"
  fi
done
curl -fsS "$BASE/api/v1/repositories/$ID/download?format=zip" -o "$OUTDIR/graphify-$ID.zip" 2>/dev/null && echo "   saved graphify-$ID.zip"
echo "   resources dir: $OUTDIR"
ls -la "$OUTDIR" 2>/dev/null | sed 's/^/     /'

echo "==> T1 GREEN ✅"
