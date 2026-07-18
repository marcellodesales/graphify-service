# syntax=docker/dockerfile:1.7

# graphify MCP server as a shared HTTP service (issue #1143).
#
# Build:  docker build -t graphify .
# Run:    docker run -p 8080:8080 -v "$(pwd)/graphify-out:/data" graphify \
#             /data/graph.json --transport http --host 0.0.0.0 --api-key "$SECRET"
#
# Builds from source so the image includes the Streamable HTTP transport even
# before it lands on PyPI. The graph.json is mounted at runtime (-v), never
# baked into the image.

# Multi-stage build: compile/install deps in a builder image, copy a venv into a slim runtime image.

FROM python:3.12-slim AS builder

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_NO_CACHE_DIR=1 \
    VIRTUAL_ENV=/opt/venv

RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN python -m venv "$VIRTUAL_ENV"
ENV PATH="$VIRTUAL_ENV/bin:$PATH"

WORKDIR /app

# Copy only what we need to install the package (keeps build cache effective)
COPY pyproject.toml README.md LICENSE /app/
COPY graphify /app/graphify

# Curated extras for graphify-as-a-service:
#   mcp,starlette + uvicorn → MCP query server (HTTP)
#   neo4j → Cypher export;  falkordb optional (not included)
#   svg (matplotlib) → SVG export;  leiden (graspologic) → better communities
#   pdf,office,google,postgres → doc / Google-Workspace / DB-schema ingestion
# Export formats graph.json/graph.html/GraphML/callflow-html are base (networkx).
# NOTE: these extras pull native deps (grpcio, lxml, igraph, Pillow, psycopg) that
# compile from source. On QEMU-emulated arm64 that exceeds the CI job timeout, so
# the graphify image publishes amd64-only (see docker-multiarch-cicd.yaml).
RUN pip install --upgrade pip setuptools wheel \
    && pip install ".[mcp,neo4j,watch,svg,leiden,pdf,office,google,postgres]" uvicorn


FROM python:3.12-slim AS runtime

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    VIRTUAL_ENV=/opt/venv

COPY --from=builder /opt/venv /opt/venv
ENV PATH="$VIRTUAL_ENV/bin:$PATH"

# Non-root runtime user
RUN addgroup --gid 10001 app \
    && adduser --uid 10001 --gid 10001 --disabled-password --gecos "" app

USER app
WORKDIR /workspace

# CLI entrypoint: use the original graphify console script so every
# first-party subcommand remains available in the container.
ENTRYPOINT ["graphify"]
CMD ["--help"]
