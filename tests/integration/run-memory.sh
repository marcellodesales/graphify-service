#!/usr/bin/env bash
# Integration test T2 (memory abstraction): create a memory → add one or more
# public git sources → let the memory worker clone+extract+merge → poll until
# ready → fetch the merged unified graph → ask a question via the memory /query
# endpoint, which composes with the shared graphify-mcp server (project_path =
# the memory dir, so it resolves memories/<id>/graphify-out/graph.json).
#
# This proves the "memory-aware MCP" path end to end using the EXISTING
# graphify-mcp service — no memory-specific MCP server: the one stateless server
# answers Q&A against any memory on the shared repos volume.
#
#   ./tests/integration/run-memory.sh
#   KEEP_STACK=1 ./tests/integration/run-memory.sh     # leave the stack up
#   SAMPLE_REPO=https://github.com/o/r ./tests/integration/run-memory.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

COMPOSE=(docker compose -f docker-compose.yaml -f tests/integration/docker-compose.test.yaml)
REPO="${SAMPLE_REPO:-https://github.com/githubtraining/hellogitworld}"
QUESTION="${MEMORY_QUESTION:-main}"
KEEP="${KEEP_STACK:-0}"

# Host ports (kept off the common 8080-8084 to avoid local conflicts).
export GRAPHIFY_API_PORT="${GRAPHIFY_API_PORT:-18080}"
export GRAPHIFY_CLONER_PORT="${GRAPHIFY_CLONER_PORT:-18081}"
export GRAPHIFY_WORKER_PORT="${GRAPHIFY_WORKER_PORT:-18082}"
export GRAPHIFY_MCP_PORT="${GRAPHIFY_MCP_PORT:-18083}"
export GRAPHIFY_MEMORY_WORKER_PORT="${GRAPHIFY_MEMORY_WORKER_PORT:-18084}"
BASE="${BASE_URL:-http://localhost:${GRAPHIFY_API_PORT}}"

cleanup() {
  if [ "$KEEP" != "1" ]; then
    "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
    rm -rf "$ROOT/data/repos" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# Clean slate: the repos bind mount survives `down -v`.
rm -rf "$ROOT/data/repos" 2>/dev/null || true
mkdir -p "$ROOT/data/repos"
chmod -R 0777 "$ROOT/data" 2>/dev/null || true

echo "==> build graphify base (fresh source — provides 'graphify extract' + 'merge-graphs')"
"${COMPOSE[@]}" build graphify

echo "==> build + start stack (nats, api, memory-worker, mcp)"
"${COMPOSE[@]}" up -d --build nats graphify-api graphify-memory-worker graphify-mcp

echo "==> wait for API /readyz"
for i in $(seq 1 60); do
  if curl -fsS "$BASE/readyz" >/dev/null 2>&1; then break; fi
  sleep 2
  if [ "$i" = 60 ]; then echo "API not ready"; "${COMPOSE[@]}" logs graphify-api; exit 1; fi
done

echo "==> create memory"
MID="$(curl -fsS -X POST "$BASE/api/v1/memories" \
  -H 'content-type: application/json' \
  -d '{"name":"t2-memory","description":"integration test memory"}' | jq -r .id)"
echo "    memoryId=$MID"
[ -n "$MID" ] && [ "$MID" != "null" ] || { echo "no memory id"; exit 1; }

echo "==> add git source: $REPO"
RID="$(curl -fsS -X POST "$BASE/api/v1/memories/$MID/resources" \
  -H 'content-type: application/json' \
  -d "{\"gitRepoUrl\":\"$REPO\"}" | jq -r .resourceId)"
echo "    resourceId=$RID"

echo "==> poll until memory is ready (clone + extract + merge)"
for i in $(seq 1 150); do
  ST="$(curl -fsS "$BASE/api/v1/memories/$MID" | jq -r .status)"
  echo "    [$i] status=$ST"
  case "$ST" in
    ready) break ;;
    failed)
      echo "memory pipeline FAILED:"; curl -fsS "$BASE/api/v1/memories/$MID" | jq .
      "${COMPOSE[@]}" logs graphify-memory-worker; exit 1 ;;
  esac
  sleep 2
  if [ "$i" = 150 ]; then
    echo "timeout waiting for ready"; "${COMPOSE[@]}" logs graphify-memory-worker; exit 1
  fi
done

echo "==> fetch merged unified graph (must be non-empty JSON with nodes)"
GRAPH="$(curl -fsS "$BASE/api/v1/memories/$MID/graph")"
NODES="$(echo "$GRAPH" | jq '(.nodes // []) | length')"
echo "    merged graph node count=$NODES"
[ "$NODES" -gt 0 ] || { echo "merged graph has no nodes"; echo "$GRAPH" | head -c 400; exit 1; }

echo "==> query the memory via graphify-mcp composition (question: '$QUESTION')"
RESP="$(curl -fsS -X POST "$BASE/api/v1/memories/$MID/query" \
  -H 'content-type: application/json' \
  -d "{\"question\":\"$QUESTION\"}")"
echo "$RESP" | jq .

IS_ERR="$(echo "$RESP" | jq -r .isError)"
TOOL="$(echo "$RESP" | jq -r .tool)"
ANSWER="$(echo "$RESP" | jq -r .answer)"
[ "$IS_ERR" = "false" ] || { echo "query returned isError=true"; exit 1; }
[ "$TOOL" = "query_graph" ] || { echo "unexpected tool: $TOOL"; exit 1; }
[ -n "$ANSWER" ] && [ "$ANSWER" != "null" ] || { echo "empty answer"; exit 1; }

echo "==> graph_stats over the same memory (no question needed)"
STATS="$(curl -fsS -X POST "$BASE/api/v1/memories/$MID/query" \
  -H 'content-type: application/json' \
  -d '{"tool":"graph_stats"}')"
echo "$STATS" | jq -c '{tool, isError, answer: (.answer | .[0:120])}'
[ "$(echo "$STATS" | jq -r .isError)" = "false" ] || { echo "graph_stats isError=true"; exit 1; }

echo "==> T2 GREEN ✅  (memory $MID answered a graph query via graphify-mcp)"
