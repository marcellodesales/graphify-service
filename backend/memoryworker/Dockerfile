# syntax=docker/dockerfile:1.7
#
# graphify-memory-worker — NATS-driven worker for the memory abstraction
# (multi-source unified graph).
# Build context: ./backend
#   docker build -f backend/memoryworker/Dockerfile -t marcellodesales/graphify-memory-worker ./backend
#
# The final image is built FROM the graphify runtime image, which already ships
# the `graphify` binary on PATH. The worker shells out to `graphify extract`,
# `graphify merge-graphs`, and git. Build the graphify image first, e.g.:
#   docker compose build graphify           # produces marcellodesales/graphify:latest
# Override the base with --build-arg GRAPHIFY_IMAGE=...

# Global ARG must precede the first FROM to be usable in a later FROM.
ARG GRAPHIFY_IMAGE=marcellodesales/graphify:latest

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY go.su[m] ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/graphify-memory-worker ./memoryworker

FROM ${GRAPHIFY_IMAGE} AS runtime
# COPY runs as root during build; the graphify image otherwise runs as uid 10001.
USER root
# The graphify base (python:3.12-slim) ships no VCS tooling, but this worker is
# self-contained: unlike the single-repo pipeline (where graphify-cloner clones),
# it clones each git resource AND commits the memory repo itself. So it needs git
# + openssh-client (the latter for deploy-key SSH clones) on PATH.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git openssh-client \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/graphify-memory-worker /usr/local/bin/graphify-memory-worker
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/graphify-memory-worker"]
