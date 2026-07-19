# syntax=docker/dockerfile:1.7
#
# graphify-cloner — NATS-driven clone worker.
# Build context: ./backend
#   docker build -f backend/cloner/Dockerfile -t marcellodesales/graphify-cloner ./backend
#
# Includes git + openssh-client because this is the ONLY image that performs Git
# operations and mounts SSH deploy keys (spec §3.2, §10).

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY go.su[m] ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/graphify-cloner ./cloner

FROM alpine:3.20 AS runtime
RUN apk add --no-cache ca-certificates git openssh-client \
    && addgroup -g 10001 app \
    && adduser -u 10001 -G app -D -H app
COPY --from=build /out/graphify-cloner /usr/local/bin/graphify-cloner
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/graphify-cloner"]
