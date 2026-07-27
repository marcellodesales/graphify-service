#!/usr/bin/env bash
#
# Host entrypoint for the reproducible containerized integration suite —
# SINGLE-REPO scope (SUITE=memory by default).
#
# This runs the ONE-repo memory flow against a PUBLIC sample repo, so it needs no
# SSH key. For the TWO-repo private correlation suite, use its dedicated runner:
#   ./tests/integration/run-memory-2repos.sh
# (that wrapper just re-invokes this script with SUITE=memory2 + the deploy key).
#
# Unlike run.sh / run-memory.sh (which need bru/curl/jq on the host and are for
# manual runs), this needs only Docker: it builds the `graphify` base, then brings
# up the stack plus the in-container `integration-tests` runner, whose exit code
# becomes the suite result.
#
#   ./tests/integration/run-integration.sh                    # SUITE=memory (1 repo)
#   SUITE=memory2    ./tests/integration/run-integration.sh   # T3 two-repo together (needs key)
#   SUITE=memory2seq ./tests/integration/run-integration.sh   # T3 two-repo one-by-one (needs key)
#   SUITE=repo       ./tests/integration/run-integration.sh   # T1 repository API
#   SUITE=all        ./tests/integration/run-integration.sh   # everything
#   KEEP_STACK=1  ./tests/integration/run-integration.sh   # don't tear down after
#
# Private repos (memory2/all only): PRIVATE=1 (default) clones the app/deploy
# repos over SSH using a deploy key copied from SSH_KEY_FILE (default
# ~/.ssh/id_rsa) into a gitignored, read-only mount. The key is never committed
# and never baked into an image. If a private suite runs and the key file is
# absent, the run fails loudly rather than skipping the private repos. The
# default SUITE=memory is public and ignores the key entirely.
#
#   SSH_KEY_FILE=~/.ssh/id_ed25519 SUITE=memory2 ./tests/integration/run-integration.sh
#   PRIVATE=0 SUITE=memory2 ./tests/integration/run-integration.sh   # public, no key
#
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

# Explicit -f override chain (base → test override → integration runner). We do
# NOT use compose `include:`: it requires imported files to be disjoint, and both
# the base and the test override define graphify-worker, which older Docker Compose
# (e.g. the CI runner) rejects with "conflicts with imported resource". The -f
# chain merges same-named services reliably on every version. Order matters: later
# files override earlier ones. All three are repo-root-relative (we cd $ROOT above),
# so the project directory is the repo root and every relative path resolves there.
COMPOSE=(docker compose
  -f docker-compose.yaml
  -f tests/integration/docker-compose.test.yaml
  -f tests/integration/docker-compose-integration.yaml)
# App services to bring up (graphify-service watch + neo4j intentionally excluded).
SVCS=(nats graphify-api graphify-cloner graphify-worker graphify-memory-worker graphify-mcp)
KEEP="${KEEP_STACK:-0}"

# Default scope is the single-repo memory suite (public, keyless). The two-repo
# private correlation suite is driven by run-memory-2repos.sh (SUITE=memory2).
SUITE="${SUITE:-memory}"
export SUITE

PRIVATE="${PRIVATE:-1}"
export PRIVATE
SSH_KEY_FILE="${SSH_KEY_FILE:-$HOME/.ssh/id_rsa}"
SECRETS_DIR="$ROOT/tests/integration/.secrets"

# Only the suites that clone the private app/deploy repos need the deploy key.
NEEDS_KEY=0
case "$SUITE" in memory2|memory2seq|all) [ "$PRIVATE" = "1" ] && NEEDS_KEY=1 ;; esac

# The worker/memory-worker images are FROM the fresh local graphify base.
export GRAPHIFY_IMAGE="${GRAPHIFY_IMAGE:-marcellodesales/graphify:latest}"

cleanup() {
  # Always remove the injected key copy, even on failure.
  rm -rf "$SECRETS_DIR"
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
# The service containers run as uid 10001 (see backend/*/Dockerfile) but this
# bind-mounted dir is created by the host user. On Linux (CI) bind mounts preserve
# host uid/gid, so uid 10001 can't create the memories/ root inside it and the
# stack dies with "mkdir /graphify-service/repos: permission denied". (Docker
# Desktop on macOS masks this — mounts appear writable regardless — which is why
# it only bites in CI.) Make the ephemeral test dir group/other-writable so the
# container user can populate it. It's throwaway state, wiped at the next run.
chmod 0777 "$ROOT/data/repos" 2>/dev/null || true

# Inject the private deploy key into the gitignored mount (0600). The runner
# reads it from /run/ssh/ssh_key and sends it in the add-resource body; it is
# never written into the repo tree or the image.
rm -rf "$SECRETS_DIR"; mkdir -p "$SECRETS_DIR"
if [ -f "$SSH_KEY_FILE" ]; then
  install -m 600 "$SSH_KEY_FILE" "$SECRETS_DIR/ssh_key"
  echo "==> injected SSH deploy key from $SSH_KEY_FILE (read-only mount, gitignored, not committed)"
elif [ "$NEEDS_KEY" = "1" ]; then
  echo "==> WARNING: SUITE=$SUITE needs a private deploy key but none at $SSH_KEY_FILE — the run will fail." >&2
  echo "==>          Set SSH_KEY_FILE=<path> or run with PRIVATE=0 for public repos." >&2
fi

echo "==> building graphify base image (provides extract + merge-graphs)"
"${COMPOSE[@]}" build graphify

echo "==> running integration suite (SUITE=$SUITE)"
set +e
"${COMPOSE[@]}" up --build --abort-on-container-exit \
  --exit-code-from integration-tests \
  "${SVCS[@]}" integration-tests
code=$?
set -e

# Visual feedback: render each merged memory graph with the terminal community
# visualizer (`graphify graph-summary`). The graphs are bind-mounted at
# ./data/repos/memories/<id>/graphify-out/graph.json; a one-off graphify-bearing
# container reads them over that same volume. Best-effort — never changes the
# suite's exit code (the assertions already ran inside integration-tests).
echo "==> graph summaries (graphify graph-summary over the shared volume)"
# NOTE: use `sh -c`, NOT `sh -lc`. A login shell re-sources /etc/profile, which
# resets PATH to a debian default and drops the graphify venv (/opt/venv/bin)
# that the image's ENV PATH provides — hence "graphify: not found". We also
# prepend the venv defensively so this holds regardless of the base image's
# profile. Paths are printed in full (absolute, as mounted in the container).
#
# The whole block is captured so it can be both echoed to the console AND written
# as a markdown artifact ($GRAPH_SUMMARY_OUT) that CI folds into the job's
# $GITHUB_STEP_SUMMARY and the sticky PR comment.
summary_raw="$(
  "${COMPOSE[@]}" run --rm --no-deps -T --entrypoint sh graphify-memory-worker -c '
    export PATH="/opt/venv/bin:$PATH"
    found=0
    for mem in /graphify-service/repos/memories/*; do
      [ -d "$mem" ] || continue
      merged="$mem/graphify-out/graph.json"
      [ -f "$merged" ] || continue
      found=1
      echo "════════ memory $(basename "$mem") ════════"
      # BEFORE the merge: each per-resource input graph (git clones + uploads).
      for g in "$mem"/git/*/*/*/graphify-out/graph.json "$mem"/files/*/graphify-out/graph.json; do
        [ -f "$g" ] || continue
        echo "── before-merge input: $g ──"
        graphify graph-summary "$g" || true
      done
      # AFTER the merge: the unified graph.
      echo "── after-merge (unified): $merged ──"
      graphify graph-summary "$merged" || true
    done
    [ "$found" = 1 ] || echo "(no memory graphs found to summarize)"
  ' 2>&1
)" || summary_raw="(graph summary step failed with exit $?)"
printf '%s\n' "$summary_raw"

# Emit a markdown artifact for CI (step summary + sticky PR comment). Best-effort:
# never affects the suite exit code.
if [ -n "${GRAPH_SUMMARY_OUT:-}" ]; then
  {
    echo "### Before/after-merge graph structure — \`SUITE=$SUITE\`"
    echo
    echo '```text'
    printf '%s\n' "$summary_raw"
    echo '```'
  } >"$GRAPH_SUMMARY_OUT" 2>/dev/null \
    && echo "==> wrote graph summary artifact: $GRAPH_SUMMARY_OUT" \
    || echo "==> could not write graph summary artifact ($GRAPH_SUMMARY_OUT)"
fi

if [ "$code" -eq 0 ]; then
  echo "==> INTEGRATION SUITE PASSED"
else
  echo "==> INTEGRATION SUITE FAILED (exit $code)"
fi
exit "$code"
