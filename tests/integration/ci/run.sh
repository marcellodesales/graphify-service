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
#   SUITE=all       T2 memory + T3 two-repo correlation + T1 repository API (default)
#   SUITE=memory    T2 single-repo memory only
#   SUITE=memory2   T3 two-repo correlation, both repos added together (one merge)
#   SUITE=memory2seq T3 two-repo correlation, repos added one-by-one (append re-merge)
#   SUITE=repo      T1 repository API only
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

# Private-repo access. PRIVATE=1 (default) makes the correlation suite clone its
# repos over SSH with a deploy key. The key is a runtime mount (never committed,
# never baked into the image); run-integration.sh copies $SSH_KEY_FILE (default
# ~/.ssh/id_rsa) into it. If PRIVATE=1 and no key is present, the run FAILS —
# private repos are not silently skipped. Set PRIVATE=0 to use public repos.
PRIVATE="${PRIVATE:-1}"
SSH_KEY="${SSH_KEY:-/run/ssh/ssh_key}"

cd "$(dirname "$0")/.."   # /suite  (tests/integration)

log()  { echo "==> $*"; }
die()  { echo "ERROR: $*" >&2; exit 1; }
slug() { basename "${1%.git}"; }

# to_ssh_url normalizes any git URL to scp-like SSH form (git@host:owner/repo.git)
# so the worker's GIT_SSH_COMMAND (deploy key) actually applies — an https:// URL
# would ignore the key and prompt for a password.
to_ssh_url() { # <git-url> -> git@host:owner/repo.git
  local u="$1" rest host path
  case "$u" in
    git@*|ssh://*) echo "$u"; return ;;
  esac
  rest="${u#http://}"; rest="${rest#https://}"
  host="${rest%%/*}"; path="${rest#*/}"; path="${path%.git}"
  echo "git@${host}:${path}.git"
}

# ssh_host_of extracts the hostname from any supported git URL form.
ssh_host_of() { # <git-url> -> host
  local u="$1" x
  case "$u" in
    git@*)   x="${u#git@}";   echo "${x%%:*}";  return ;;
    ssh://*) x="${u#ssh://}"; x="${x#*@}";       echo "${x%%/*}"; return ;;
  esac
  x="${u#http://}"; x="${x#https://}"; echo "${x%%/*}"
}

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

# add_key provisions a memory-scoped SSH deploy key ONCE and returns its keyId.
# This is Feature A end-to-end: instead of posting the key material with every
# git-add (inline sshKey), the caller adds the key here, then references it by id
# across any number of private git-adds (and it stays rotatable via PUT). The key
# material never enters the manifest — only a fingerprint + timestamps are stored.
add_key() { # <memory-id> <git-url-for-host> -> key id (stdout)
  local mid="$1" url="$2" sshurl host khfile body kid
  [ -s "$SSH_KEY" ] || die "a private repo requires an SSH deploy key at $SSH_KEY, but none was mounted. Provide one via: SSH_KEY_FILE=~/.ssh/id_rsa ./tests/integration/run-integration.sh   (or set PRIVATE=0 for public repos)."
  sshurl="$(to_ssh_url "$url")"
  host="$(ssh_host_of "$sshurl")"
  khfile="$(mktemp)"
  # StrictHostKeyChecking=yes on the worker means we must supply known_hosts.
  ssh-keyscan -T 10 "$host" >"$khfile" 2>/dev/null || true
  [ -s "$khfile" ] || { rm -f "$khfile"; die "ssh-keyscan produced no host key for $host (network/DNS?)"; }
  # jq --rawfile safely JSON-encodes the multi-line key/known_hosts material.
  body="$(jq -n --arg n "deploy-$host" --rawfile k "$SSH_KEY" --rawfile h "$khfile" \
    '{name:$n, sshKey:$k, knownHosts:$h}')"
  rm -f "$khfile"
  kid="$(curl -fsS -X POST "$BASE_URL/api/v1/memories/$mid/keys" \
    -H 'content-type: application/json' -d "$body" | jq -r .id)"
  [ -n "$kid" ] && [ "$kid" != null ] || die "failed to provision deploy key for $host in memory $mid"
  echo "$kid"
}

add_resource() { # <memory-id> <git-url> [keyId] -> resource id (stdout)
  local mid="$1" url="$2" keyid="${3:-}" body rid
  if [ -n "$keyid" ]; then
    # Private: reference the already-provisioned memory-scoped key by id. The
    # clone URL is normalized to SSH form so the worker's deploy key applies.
    body="$(jq -n --arg u "$(to_ssh_url "$url")" --arg k "$keyid" '{gitRepoUrl:$u, keyId:$k}')"
  else
    body="$(jq -n --arg u "$url" '{gitRepoUrl:$u}')"
  fi
  rid="$(curl -fsS -X POST "$BASE_URL/api/v1/memories/$mid/resources" \
    -H 'content-type: application/json' -d "$body" | jq -r .resourceId)"
  [ -n "$rid" ] && [ "$rid" != null ] || die "failed to add resource $url to memory $mid"
  echo "$rid"
}

# report_memory prints the work-status (Feature B) and the FULL graph structure
# (Feature C) for a memory, as visual feedback in the CI log. The optional second
# arg is a phase label (e.g. "after first merge") so before/after-merge prints are
# self-describing. The ci container has no python/graphify, so the community
# visualizer is reimplemented in jq here directly from GET .../graph; the real
# `graphify graph-summary` also runs host-side in run-integration.sh over the
# bind-mounted graphs.
report_memory() { # <memory-id> [phase-label]
  local mid="$1" label="${2:-}"
  log "work status ($mid)${label:+ — $label}:"
  curl -fsS "$BASE_URL/api/v1/memories/$mid/status" \
    | jq '{status, stage, lastOperation: .lastOperation.status, resources: [.resources[] | {id, kind, status, op: .lastOperation.status}]}' \
    || log "status unavailable"
  log "graph structure ($mid)${label:+ — $label}:"
  # Full per-repo + per-community ASCII, rendered from the raw merged graph.
  curl -fsS "$BASE_URL/api/v1/memories/$mid/graph" \
    | jq -r '
        def repo_of: .repo // ((.id // "") | tostring | if test("::") then split("::")[0] else "(local)" end);
        (.nodes // []) as $n
        | (.links // .edges // []) as $e
        | ($n | map(repo_of) | unique) as $repos
        | ($n | map(.community) | map(select(. != null)) | unique) as $comms
        | ($n | group_by(repo_of) | map({repo:(.[0]|repo_of), count:length}) | sort_by(-.count)) as $perrepo
        | (($perrepo | map(.count) | max) // 0) as $rmax0
        | (if $rmax0 == 0 then 1 else $rmax0 end) as $rmax
        | ($n | map(select(.community != null)) | group_by(.community)
            | map({name:(.[0].community_name // (.[0].community | tostring)), size:length,
                   repos:(map(repo_of) | unique),
                   labels:(map(.label // .local_id // .id) | map(select(. != null)) | .[0:5])})
            | sort_by(-.size)) as $percomm
        | "  Graph summary\n  totals: nodes=\($n|length) edges=\($e|length) communities=\($comms|length) repos=\($repos|length)\n\n  Per-repo (node counts)\n"
          + ($perrepo | map("    \(.repo)  \(if .count>0 then ("█" * ([1,(((.count/$rmax)*30)|floor)]|max)) else "" end) \(.count)") | join("\n"))
          + "\n\n  Communities (largest first)\n"
          + (if ($percomm|length)==0 then "    (none)"
             else ($percomm | map("    #\(.name) (size=\(.size), repos=\(.repos|join(","))): \(.labels|join(", "))") | join("\n")) end)
      ' \
    || log "graph unavailable (not merged yet?)"
}

poll_resource_ready() { # <memory-id> <resource-id> -> returns when that resource is ready
  local mid="$1" rid="$2" st i
  for ((i = 1; i <= READY_TRIES; i++)); do
    st="$(curl -fsS "$BASE_URL/api/v1/memories/$mid/resources/$rid" | jq -r .status)"
    echo "    [$i/$READY_TRIES] resource=$rid status=$st"
    case "$st" in
      ready)  return 0 ;;
      failed) curl -fsS "$BASE_URL/api/v1/memories/$mid/resources/$rid" | jq . ; die "resource $rid failed" ;;
    esac
    sleep "$READY_SLEEP"
  done
  die "timeout waiting for resource $rid to become ready"
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
  report_memory "$mid" "after merge (single repo)"
  log "asserting bruno-memory collection"
  ( cd bruno-memory && bru run . --env local \
    --env-var "baseUrl=$BASE_URL" \
    --env-var "memoryId=$mid" \
    --env-var "resourceId=$rid" \
    --env-var "question=$QUESTION" )
}

# assert_two_repo_memory <memory-id> <resource-id-for-question>
# Shared assertions for a ready two-repo memory: the generic memory contract on
# the unified graph, then the cross-repo correlation collection. Independent of
# HOW the resources were added (together or one-by-one), so both suites reuse it.
assert_two_repo_memory() {
  local mid="$1" rid="$2"
  log "asserting bruno-memory collection (unified graph)"
  ( cd bruno-memory && bru run . --env local \
    --env-var "baseUrl=$BASE_URL" \
    --env-var "memoryId=$mid" \
    --env-var "resourceId=$rid" \
    --env-var "question=$QUESTION" )
  log "asserting bruno-memory-2repos collection (correlation)"
  ( cd bruno-memory-2repos && bru run . --env local \
    --env-var "baseUrl=$BASE_URL" \
    --env-var "memoryId=$mid" \
    --env-var "appSlug=$(slug "$APP_REPO")" \
    --env-var "deploySlug=$(slug "$DEPLOY_REPO")" )
}

run_memory2() {
  log "T3 — two-repo correlation, added TOGETHER (PRIVATE=$PRIVATE): $APP_REPO + $DEPLOY_REPO"
  local mid rid1 rid2 keyid=""
  mid="$(create_memory t3-2repos)"
  # Provision ONE deploy key for the memory and reference it from both git-adds
  # (both repos are on the same host) — proves the keyId flow + key reuse.
  if [ "$PRIVATE" = "1" ]; then
    keyid="$(add_key "$mid" "$APP_REPO")"
    log "provisioned deploy key=$keyid (reused across both repos)"
  fi
  rid1="$(add_resource "$mid" "$APP_REPO" "$keyid")"
  rid2="$(add_resource "$mid" "$DEPLOY_REPO" "$keyid")"
  log "memory=$mid resources=$rid1,$rid2"
  poll_memory "$mid"
  report_memory "$mid" "after merge (app + deploy together)"
  assert_two_repo_memory "$mid" "$rid1"
}

# Sequential variant: add ONE repo, wait for the memory to reach ready, then add
# the second. Appending a resource to a ready memory flips it ingesting→ready
# again (the worker re-merges on the new ready set), so the final unified graph
# and correlation are identical to the together case — exercised with the SAME
# APIs, no backend change. Proves incremental growth of a memory.
run_memory2_seq() {
  log "T3 — two-repo correlation, added ONE-BY-ONE (PRIVATE=$PRIVATE): $APP_REPO then $DEPLOY_REPO"
  local mid rid1 rid2 keyid=""
  mid="$(create_memory t3-2repos-seq)"
  if [ "$PRIVATE" = "1" ]; then
    keyid="$(add_key "$mid" "$APP_REPO")"
    log "provisioned deploy key=$keyid (reused across both repos)"
  fi
  rid1="$(add_resource "$mid" "$APP_REPO" "$keyid")"
  log "memory=$mid resource=$rid1 (app) — waiting for first merge"
  poll_memory "$mid"
  # BEFORE the re-merge: the graph is the single-repo (app-only) merge. Printing
  # it here and again after the deploy repo lands gives a clear before/after view
  # of how appending a second source grows the unified graph.
  report_memory "$mid" "after first merge (app only)"
  rid2="$(add_resource "$mid" "$DEPLOY_REPO" "$keyid")"
  log "memory=$mid resource=$rid2 (deploy) appended — waiting for re-merge"
  # Gate on the new resource reaching ready FIRST. The worker flips the memory
  # off 'ready' (→ingesting) when it starts ingesting this resource, which
  # necessarily precedes the resource becoming ready — so once the deploy
  # resource is ready, a subsequent poll_memory can't catch the stale first-merge
  # 'ready'; it waits for the genuine re-merge over both sources.
  poll_resource_ready "$mid" "$rid2"
  poll_memory "$mid"
  report_memory "$mid" "after re-merge (app + deploy)"
  assert_two_repo_memory "$mid" "$rid1"
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
  memory)     run_memory ;;
  memory2)    run_memory2 ;;
  memory2seq) run_memory2_seq ;;
  repo)       run_repo ;;
  all)        run_memory; run_memory2; run_repo ;;
  *)          die "unknown SUITE=$SUITE (want: all|memory|memory2|memory2seq|repo)" ;;
esac

log "integration suite PASSED (SUITE=$SUITE)"
