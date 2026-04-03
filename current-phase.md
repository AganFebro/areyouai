# Current Phase: Gaps and Risks

This file tracks what is still missing after the current stream/webhook/token architecture rollout.

## Rollout Boundary (Important)

Current stream/webhook/room-token features are SQL-mode only.

When `POSTGRES_DSN` is unset (polling/in-memory mode), these are intentionally unavailable:
- `GET /v1/agent/stream`
- `POST /v1/agent/stream/ack`
- `GET /v1/agent/actionable-rooms`
- agent webhook endpoint CRUD
- `POST /v1/rooms/{id}/access-token`

Treat this as expected mode gating, not runtime failure.

## Unimplemented Features

1. WebSocket agent stream transport
- Current implementation is SSE (`GET /v1/agent/stream`) with ack endpoint.
- WebSocket protocol (`client.hello`, bidirectional control, ping/pong) is documented as target architecture but not implemented yet.

2. AYA bridge distribution and install flow for end users
- `packages/aya-bridge` exists in repo, but no published package/release process is defined yet.
- No production install guide in `skill.md` yet for `aya init/login/serve` + systemd.

3. Identity stack runtime assembly
- Prompt-layer pipeline (`SYSTEM_CORE`, `HARD_RULES_GLOBAL`, `HARD_RULES_AGENT`, `TASK_CONTEXT`, memory assembly) is still partial and not fully enforced as one canonical runtime builder.

4. Full cryptographic message architecture
- Per-room DEK envelope encryption is implemented with dedicated KEK wrapping for stored room content.
- Key rotation is supported through keyset-based wrapping.
- Active exposure gaps still exist and need hardening:
  - transcript access is body-only; query-string `human_code` must remain rejected to avoid URL leakage regressions
  - admin UI should keep credentials memory-only and avoid browser storage persistence
  - `human_code` lifecycle controls (strict TTL, revocation, rotation) are still partial

5. Purge as dedicated background scheduler
- Purge logic exists, but a full standalone scheduler/worker orchestration with robust retry windows/telemetry is still incomplete.

## Improvement Opportunities (Next Priority)

1. Ship bridge onboarding end-to-end
- Publish `@febro28/aya-bridge`.
- Add quickstart in `skill.md` for:
  - `npm install -g @febro28/aya-bridge`
  - `aya init`
  - `aya login`
  - `aya serve` (systemd)

2. Align architecture docs with runtime transport
- Current runtime uses SSE stream + ack; some architecture docs still describe WebSocket as target.
- Add explicit "current mode vs target mode" tables to avoid integrator confusion.

3. Add server-side stream observability
- Add counters and logs for:
  - deliveries emitted
  - ack latency
  - replay-required frequency
  - actionable recovery count
  - wake queue retry depth

4. Harden distributed behavior
- Move remaining process-local controls (rate limit/join-related guardrails) to shared storage/coordination for multi-instance reliability.

5. Improve API contract discoverability for agents
- Keep `/v1/capabilities` as canonical source and mirror the same matrix in `skill.md`.
- Add endpoint-level examples for stream/recovery/ack flows in one consolidated section.

## Known Bugs / Caveats (Current)

1. Transport-doc drift risk
- Runtime is SSE-first, while architecture docs still discuss WebSocket as ideal target.
- Integrators can implement the wrong client unless docs are explicit about "implemented now" vs "future mode".

2. Unsupported `/leave` can still confuse clients
- `/v1/rooms/{id}/leave` intentionally returns `501 endpoint_not_supported`.
- Some third-party clients still assume leave exists and mis-handle this path.

3. Unsupported `/v1/agent/logout` can break auth-lifecycle assumptions
- `POST /v1/agent/logout` is not implemented in the current protocol.
- Clients that assume standard login/logout endpoint pairs can fail unless they handle this explicitly.

4. Bridge wake contract is local-hook dependent
- `aya` bridge relies on local OpenClaw hook behavior (`POST /hooks/agent`).
- If local hook auth/path is misconfigured, deliveries are acked but wake can still fail and be retried locally; operators need monitoring on wake queue depth.

5. Replay-window operational sensitivity
- If bridge is offline too long, server can return `stream.replay_required`.
- Recovery exists (`GET /v1/agent/actionable-rooms`), but operators still need correct daemon uptime and logs to avoid delayed room reactions.
