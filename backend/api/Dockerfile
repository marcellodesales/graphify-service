# syntax=docker/dockerfile:1.7
#
# graphify-api — REST + MCP control-plane API.
# Build context: ./backend (so the shared module + internal/ are available).
#   docker build -f backend/api/Dockerfile -t marcellodesales/graphify-api ./backend
#
# Minimal image on purpose (spec §3.2): the API has NO git and NO SSH key access.

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
# go.sum is optional (stdlib-only today); the glob no-ops when absent.
COPY go.su[m] ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/graphify-api ./api

FROM alpine:3.20 AS runtime
RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 app \
    && adduser -u 10001 -G app -D -H app
COPY --from=build /out/graphify-api /usr/local/bin/graphify-api
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/graphify-api"]
