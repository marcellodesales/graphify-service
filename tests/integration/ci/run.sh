#!/usr/bin/env bash
#
# In-container integration runner (the `integration-tests` compose service).
#
# The whole tests/integration/ tree is bind-mounted read-only at /suite, so this
# script and every .bru spec are editable without rebuilding the image — the run
# is reproducible from the volume. Orchestration (create → poll to ready) is pure
# shell here; the actual assertions live in the declarative Bruno collections
# (bruno-memory, bruno-memory-2repos, bruno) and are executed with `bru run`.
#
# Services are reached over the compose network by DNS name (graphify-api:8080),
# NOT via host ports. SUITE selects which levels run.
#
#   SUITE=all      T2 memory + T3 two-repo correlation + T1 repository API (default)
#   SUITE=memory   T2 single-repo memory only
#   SUITE=memory2  T3 two-repo correlation only
#   SUITE=repo     T1 repository API only
#
set -euo pipefail

BASE_URL="${BASE_URL:-http://graphify-api:8080}"
CLONER_URL="${CLONER_URL:-http://graphify-cloner:8080}"
WORKER_URL="${WORKER_URL:-http://graphify-worker:8080}"
SUITE="${SUITE:-all}"
SAMPLE_REPO="${SAMPLE_REPO:-https://github.com/githubtraining/hellogitworld}"
APP_REPO="${APP_REPO:-https://github.com/marcellodesales/azure-chatgpt-spa-service}"
DEPLOY_REPO="${DEPLOY_REPO:-https://github.com/marcellodesales/azure-chatgpt-spa-service-deploy}"
QUESTION="${MEMORY_QUESTION:-main}"
READY_TRIES="${READY_TRIES:-200}"   # * 2s = up to ~400s per pipeline
READY_SLEEP="${READY_SLEEP:-2}"

cd "$(dirname "$0")/.."   # /suite  (tests/integration)

log()  { echo "==> $*"; }
die()  { echo "ERROR: $*" >&2; exit 1; }
slug() { basename "${1%.git}"; }

# ── helpers ────────────────────────────────────────────────────────────────
wait_ready() { # <service-url>
  local url="$1" i
  log "waiting for $url/readyz"
  for ((i = 1; i <= 90; i++)); do
    if curl -fsS "$url/readyz" >/dev/null 2>&1; then
      log "$url is ready"
      return 0
    fi
    sleep 2
  done
  die "service never became ready: $url"
}

create_memory() { # <name> -> memory id (stdout)
  local mid
  mid="$(curl -fsS -X POST "$BASE_URL/api/v1/memories" \
    -H 'content-type: application/json' \
    -d "{\"name\":\"$1\"}" | jq -r .id)"
  [ -n "$mid" ] && [ "$mid" != null ] || die "failed to create memory ($1)"
  echo "$mid"
}

add_resource() { # <memory-id> <git-url> -> resource id (stdout)
  local rid
  rid="$(curl -fsS -X POST "$BASE_URL/api/v1/memories/$1/resources" \
    -H 'content-type: application/json' \
    -d "{\"gitRepoUrl\":\"$2\"}" | jq -r .resourceId)"
  [ -n "$rid" ] && [ "$rid" != null ] || die "failed to add resource $2 to memory $1"
  echo "$rid"
}

poll_memory() { # <memory-id>  -> returns when ready, dies on failed/timeout
  local mid="$1" st i
  for ((i = 1; i <= READY_TRIES; i++)); do
    st="$(curl -fsS "$BASE_URL/api/v1/memories/$mid" | jq -r .status)"
    echo "    [$i/$READY_TRIES] memory=$mid status=$st"
    case "$st" in
      ready)  return 0 ;;
      failed) curl -fsS "$BASE_URL/api/v1/memories/$mid" | jq . ; die "memory $mid failed" ;;
    esac
    sleep "$READY_SLEEP"
  done
  die "timeout waiting for memory $mid to become ready"
}

# ── suites ─────────────────────────────────────────────────────────────────
run_memory() {
  log "T2 — single-repo memory: $SAMPLE_REPO"
  local mid rid
  mid="$(create_memory t2-memory)"
  rid="$(add_resource "$mid" "$SAMPLE_REPO")"
  log "memory=$mid resource=$rid"
  poll_memory "$mid"
  log "asserting bruno-memory collection"
  ( cd bruno-memory && bru run . --env local \
    --env-var "baseUrl=$BASE_URL" \
    --env-var "memoryId=$mid" \
    --env-var "resourceId=$rid" \
    --env-var "question=$QUESTION" )
}

run_memory2() {
  log "T3 — two-repo correlation: $APP_REPO + $DEPLOY_REPO"
  local mid rid1 rid2
  mid="$(create_memory t3-2repos)"
  rid1="$(add_resource "$mid" "$APP_REPO")"
  rid2="$(add_resource "$mid" "$DEPLOY_REPO")"
  log "memory=$mid resources=$rid1,$rid2"
  poll_memory "$mid"
  # Generic memory contract still holds for the unified graph.
  log "asserting bruno-memory collection (unified graph)"
  ( cd bruno-memory && bru run . --env local \
    --env-var "baseUrl=$BASE_URL" \
    --env-var "memoryId=$mid" \
    --env-var "resourceId=$rid1" \
    --env-var "question=$QUESTION" )
  # Correlation-specific assertions.
  log "asserting bruno-memory-2repos collection (correlation)"
  ( cd bruno-memory-2repos && bru run . --env local \
    --env-var "baseUrl=$BASE_URL" \
    --env-var "memoryId=$mid" \
    --env-var "appSlug=$(slug "$APP_REPO")" \
    --env-var "deploySlug=$(slug "$DEPLOY_REPO")" )
}

run_repo() {
  log "T1 — repository API: $SAMPLE_REPO"
  local id st i
  id="$(curl -fsS -X POST "$BASE_URL/api/v1/repositories" \
    -H 'content-type: application/json' \
    -d "{\"githubRepoUrl\":\"$SAMPLE_REPO\"}" | jq -r .id)"
  [ -n "$id" ] && [ "$id" != null ] || die "failed to submit repository"
  for ((i = 1; i <= READY_TRIES; i++)); do
    st="$(curl -fsS "$BASE_URL/api/v1/repositories/$id" | jq -r .status)"
    echo "    [$i/$READY_TRIES] repo=$id status=$st"
    case "$st" in
      ready)  break ;;
      failed) curl -fsS "$BASE_URL/api/v1/repositories/$id" | jq . ; die "repo $id failed" ;;
    esac
    sleep "$READY_SLEEP"
  done
  log "asserting bruno collection"
  ( cd bruno && bru run . --env local \
    --env-var "baseUrl=$BASE_URL" \
    --env-var "baseUrlCloner=$CLONER_URL" \
    --env-var "baseUrlWorker=$WORKER_URL" \
    --env-var "sampleRepo=$SAMPLE_REPO" \
    --env-var "refId=$id" )
}

# ── main ───────────────────────────────────────────────────────────────────
wait_ready "$BASE_URL"

case "$SUITE" in
  memory)  run_memory ;;
  memory2) run_memory2 ;;
  repo)    run_repo ;;
  all)     run_memory; run_memory2; run_repo ;;
  *)       die "unknown SUITE=$SUITE (want: all|memory|memory2|repo)" ;;
esac

log "integration suite PASSED (SUITE=$SUITE)"
