# PRD-002 — Clone service & repository resolution

**Requirements:** R3 (resolve default branch / graphify-out / existing graph),
R6 (SSH-key onboarding — public-first).

## Problem

Given only a repository URL (e.g. `https://git.viasat.com/TIDE/viasat-tide-orion`),
the service must determine the concrete revision to operate on and where the
graph outputs live. Some repos already contain a committed `graphify-out/`
(so we can serve it directly); others must be graphified. Private repos need
credentials the user provides.

## Requirements

### R3 — Repository resolution

1. **Default selector**: when neither ref nor sha is given, resolve the remote's
   **default branch** and clone it shallow (`--depth 1`). Record `resolvedSha`.
2. **Existing graph detection**: after clone, detect a committed `graphify-out/`
   at the repo root. If present and non-empty, the pipeline may **short-circuit**
   graphification and serve the committed artifacts (configurable — see PRD-003).
   The human-facing location for the example is
   `https://git.viasat.com/TIDE/viasat-tide-orion/tree/<defaultBranch>/graphify-out`.
3. **Resolution metadata** recorded in `metadata.json.source`: host, ownerPath,
   repository, normalizedUrl, transport, defaultBranch, resolvedSha,
   `hasCommittedGraph` (bool), `graphOutPath` (relative).
4. The **API may call the clone service synchronously** for lightweight
   resolution (default branch, does graphify-out exist) via an internal
   `resolve` capability, separate from the heavy async clone. v1 keeps it simple:
   the async clone worker records resolution into metadata; the API reports it on
   `GET /repositories/{id}`. (A dedicated synchronous `resolve` endpoint is a
   future optimization.)

### R6 — Credentials / onboarding (public-first)

1. **v1 scope: public `github.com` repos only** — no credentials required. This
   is what T1 exercises.
2. Private repos use **mounted SSH deploy keys referenced by name** (`sshKeyRef`),
   never key bytes over the API (spec §10). Keys live under a credential root
   (`/run/secrets/graphify-ssh/<name>`), confined + symlink-safe.
3. **Onboarding**: users provide their own key. Two candidate mechanisms
   (decide before building R6):
   - **Named mounted key** (spec §10) — ops mounts the key, user passes
     `sshKeyRef`. Preferred; no secret ever transits the API.
   - **Key-registration endpoint** — a future authenticated endpoint that accepts
     a key and stores it in the credential root. Higher risk; deferred.
4. For local development/testing, the operator's `~/.ssh` key can be mounted into
   the cloner as a named ref (documented in the compose/test override), but the
   API contract still only takes `sshKeyRef`.

## Clone execution (informs the worker)

- Default: `git clone --depth 1 --single-branch --no-tags <url> <tmp>` then
  `git -C <tmp> rev-parse HEAD` (spec §9.2). Ref/sha variants per spec §9.3–9.4.
- Never through a shell; `GIT_SSH_COMMAND` with `StrictHostKeyChecking=yes` for SSH.
- Atomic publish: clone into `data/repos/.tmp/…`, then rename to
  `data/repos/<id>/repository`.

## Non-goals

- Submodules, LFS (spec §22.6). SHA-fetch on servers that disallow it (report
  clearly, no full-history fallback — spec §9.4). HTTPS token auth.

## Verification

- Unit: URL normalization already covered (giturl). Resolution metadata shape.
- Integration (T1): a public repo resolves its default branch + resolvedSha;
  `hasCommittedGraph` correctly true for a repo containing `graphify-out/`.
