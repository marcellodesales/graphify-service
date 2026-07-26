# Integration-test runner image.
#
# Just enough to (a) orchestrate the flow with bash + curl + jq and (b) run the
# declarative Bruno collections with the Bruno CLI. The suite itself is NOT baked
# in — tests/integration/ is volume-mounted at /suite (read-only) by the
# integration-tests service, so editing a .bru spec or ci/run.sh needs no rebuild.
FROM node:20-bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends bash curl jq ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g @usebruno/cli@3.5.3

WORKDIR /suite

# Overridden by the compose service `command:` (kept explicit there so the
# "command is a set of shell commands" lives with the mounted suite).
CMD ["bash", "ci/run.sh"]
