# Integration-test runner image.
#
# Just enough to (a) orchestrate the flow with bash + curl + jq and (b) run the
# declarative Bruno collections with the Bruno CLI. The suite itself is NOT baked
# in — tests/integration/ is volume-mounted at /suite (read-only) by the
# integration-tests service, so editing a .bru spec or ci/run.sh needs no rebuild.
FROM node:20-bookworm-slim

# openssh-client provides ssh-keyscan, used to fetch a git host's known_hosts so
# private (SSH) clones satisfy the worker's StrictHostKeyChecking=yes.
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash curl jq ca-certificates openssh-client \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g @usebruno/cli@3.5.3

WORKDIR /suite

# Overridden by the compose service `command:` (kept explicit there so the
# "command is a set of shell commands" lives with the mounted suite).
CMD ["bash", "ci/run.sh"]
