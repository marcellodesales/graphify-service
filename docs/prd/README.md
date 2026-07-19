# Graphify-as-a-Service — PRDs & Feature Index

This directory holds the product requirement documents (PRDs) for the Go control
plane. The authoritative technical spec is
[`../FEATURES-BACKEND-SERVICE.md`](../FEATURES-BACKEND-SERVICE.md); these PRDs
expand specific capability areas and separate **requirements** (product
behavior) from **test cases** (how we prove the behavior).

## Requirements vs test cases

| ID | Requirement | PRD |
|---|---|---|
| R1 | Submit a repo (URL, ref/sha, keys) → deterministic reference ID | [FEATURES-BACKEND-SERVICE §5–6](../FEATURES-BACKEND-SERVICE.md) (built) |
| R2 | Async pipeline: clone → graphify → ready, per-phase progress | [PRD-001](PRD-001-async-pipeline-status-protocol.md) |
| R5 | Consistent status/verification protocol across all microservices | [PRD-001](PRD-001-async-pipeline-status-protocol.md) |
| R3 | Clone service resolves default branch / graphify-out / existing graph | [PRD-002](PRD-002-clone-and-repo-resolution.md) |
| R6 | SSH-key onboarding for private repos (public-first) | [PRD-002](PRD-002-clone-and-repo-resolution.md) |
| R4 | Artifact inventory + zip download + output filtering | [PRD-003](PRD-003-artifacts-output-filtering-client.md) |
| R7 | Local client that materializes results into the working dir | [PRD-003](PRD-003-artifacts-output-filtering-client.md) |
| R8 | MCP-against-repo microservice + front-MCP composition/proxy | [PRD-004](PRD-004-mcp-repo-composition.md) |

| ID | Test case | PRD |
|---|---|---|
| T1 | Bruno HTTP integration: submit → poll → download zip (live public clone) | [PRD-005](PRD-005-integration-testing.md) |
| T2 | Queue + per-service verification-endpoint checks | [PRD-005](PRD-005-integration-testing.md) |
| T3 | Bruno MCP integration: stand MCP against a repo, ask questions | [PRD-005](PRD-005-integration-testing.md) |

## Delivery status

- **Built:** R1 (submit + metadata), Phase-1 REST, per-service Docker images + CICD.
- **In progress (this round):** R2, R3 (clone execution), R4, R5, T1.
- **Documented, not built:** R6 (private-repo keys — public-first), R7 (client),
  R8 + T3 (MCP), T2 beyond basic status checks.

## Cross-cutting principle: one reference ID, one protocol

Every microservice (api, cloner, worker, and later the MCP service) can be
interrogated for the **same reference ID** and answers with the **same status
envelope** (see PRD-001 §Status protocol). The API is the single source of truth
and aggregates the per-service phase into an overall status.
