# PRD-001 — Async pipeline & cross-service status protocol

**Requirements:** R2 (async clone → graphify → ready), R5 (consistent status
protocol across microservices).

## Problem

A submit must return immediately with a reference ID (R1, built). The heavy work
(clone, graphify) happens asynchronously across separate worker microservices.
The client — and each microservice — must be able to ask "what phase is
reference `<id>` in?" and get a consistent, machine-readable answer, so a client
can poll until `ready` and then fetch results.

## Requirements

### R2 — Async pipeline

1. `POST /api/v1/repositories` persists `queued` metadata and **publishes**
   `graphify.repository.clone.requested.v1` to NATS JetStream, then returns
   `202 { id, status, statusUrl, artifactsUrl }`.
2. The **clone worker** consumes clone-requested, transitions
   `queued → cloning → cloned`, and publishes `…cloned.v1`.
3. The **graphify worker** consumes cloned, transitions
   `cloned → graphifying → ready`, and publishes `…graph.ready.v1`.
4. Failures transition to `failed` with a `stage` + safe message and publish the
   matching `…failed.v1` event.
5. All transitions are compare-and-set (spec §7.2); duplicate NATS deliveries are
   idempotent (`Nats-Msg-Id` = deterministic key).

State machine and events are defined in FEATURES-BACKEND-SERVICE §7–8.

### R5 — Status / verification protocol

Every microservice exposes the **same** status envelope for a reference ID, so
any service can be interrogated and a client always parses one shape:

```
GET /status/{id}          # on each service (api :8080, cloner, worker)
GET /api/v1/repositories/{id}   # API's rich view (superset)
```

Status envelope (returned by every service):

```json
{
  "id": "f5ec…",
  "service": "api|cloner|worker",
  "phase": "queued|cloning|cloned|graphifying|ready|failed|unknown",
  "knownAt": "2026-07-18T00:00:00Z",
  "detail": "optional human-readable note",
  "resolvedSha": "…"
}
```

- **api** answers from the authoritative metadata (single source of truth) and
  additionally **aggregates**: its `phase` is the overall pipeline phase.
- **cloner / worker** answer from the same shared metadata for that `id` (they
  read `metadata.json`); `service` names which microservice replied. A worker
  that has never seen the id returns `phase: "unknown"`.
- `phase` values are the exact `Status` strings from the state machine, so all
  services speak the same vocabulary.

### Overall-status aggregation (API)

| metadata.status | overall phase | terminal | next client action |
|---|---|---|---|
| queued/cloning/cloned/graphifying | that value | no | keep polling |
| ready | ready | yes | GET artifacts / download zip |
| failed | failed | yes | read `failure` |

## Non-goals (this PRD)

- Cancellation, retry endpoints, TTL/expiry (future states in spec §7.1).
- Distributed locks beyond in-process + compare-and-set + idempotent events.

## Verification

- Unit: state transitions, idempotent event handling, status envelope encoding.
- Integration (T1/T2): submit → each service's `/status/{id}` reports coherent
  phases; overall reaches `ready`; duplicate publishes cause no double work.
