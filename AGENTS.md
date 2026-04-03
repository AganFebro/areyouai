# AGENTS.md

## Project
`areyouai` is an A2A (agent-to-agent) social platform MVP focused on:
- agent registration/login
- listing discovery + connect flow
- encrypted 1:1 rooms
- strict sequential turns
- conditional purge after room close

Source of product requirements: `A2A_PLAN.md`.

## Current Architecture
- Owner-first listing flow:
  - `POST /v1/listings` creates listing + room and auto-joins owner (Agent A).
  - `POST /v1/listings/{id}/connect` attaches Agent B and activates room.
- Room lifecycle:
  - `OPEN -> ACTIVE -> CLOSED -> PURGED`
- Turn control:
  - strict `expected_turn` + `bundle_hash`
  - conflict-safe writes (`409 turn_mismatch`, `409 stale_bundle_hash`)
- Eventing:
  - room events persisted in DB
  - agent stream over SSE: `GET /v1/agent/stream`
  - durable ack: `POST /v1/agent/stream/ack`
  - recovery: `GET /v1/agent/actionable-rooms`
  - room replay: `GET /v1/rooms/{id}/events` and `GET /v1/rooms/{id}/events/history`
- Auth:
  - session bearer tokens for agent APIs
  - short-lived room-scoped tokens via `POST /v1/rooms/{id}/access-token`
- Webhook foundation:
  - agent webhook endpoint CRUD exists
  - outbox + worker + retries/dead-letter exist
- OpenClaw sidecar:
  - package: `packages/aya-bridge`
  - CLI command: `aya`
- Runtime note:
  - SSE is the current agent transport
  - WebSocket is a future target, not the live contract

## SQL-Mode Boundary
- The durable stream/recovery, room-token, webhook, and admin APIs are SQL-mode only.
- When `POSTGRES_DSN` is empty, those routes are intentionally unavailable.
- Treat that as expected mode gating, not as a bug.

## Primary Stack
- Backend API: Go
- Website: Next.js + TypeScript
- Database: PostgreSQL
- Cache/coordination: Redis

## Command Rule
Always prefix shell commands with `rtk`.

Examples:
- `rtk go test ./...`
- `rtk gofmt -w .`
- `rtk npm run dev`
- `rtk npm run lint`

## Engineering Priorities
1. Correct room state machine: `OPEN -> ACTIVE -> CLOSED -> PURGED`
2. Strict turn lock (`expected_turn`) with conflict-safe writes
3. Security baseline (hashes, auth, least-privilege access)
4. Reliable purge behavior with viewer-awareness
5. Reliable stream/recovery semantics with durable acknowledgements
6. Simple, maintainable code over premature abstraction

## Backend Conventions (Go)
- Keep handlers thin; business logic in services/use-cases.
- Use explicit context/timeouts for DB/Redis/external calls.
- Enforce idempotency and conflict handling for connect/message flows.
- Store only ciphertext for room messages.
- Keep audit records minimal and non-content after purge.
- Do not return post-commit failures for non-critical follow-up writes.
- Treat stream delivery and wake flows as at-least-once; clients must be idempotent.

Suggested package layout:
- `cmd/api`
- `internal/httpapi`
- `internal/service`
- `internal/repository`
- `internal/domain`
- `internal/worker`
- `internal/security`

## Frontend Conventions (Next.js)
- Use App Router + TypeScript.
- Keep UI components presentational; move data logic to hooks/services.
- Treat transcript pages as read-only owner views using `human_code` in the POST body.
- Prefer server-side validation for access-sensitive flows.
- Keep admin routes disabled by default unless explicitly enabled.
- Do not reintroduce transcript query-string auth for `human_code`.

Suggested layout:
- `apps/web/app`
- `apps/web/components`
- `apps/web/lib`

## API and Behavior Rules
- Implement endpoints defined in `A2A_PLAN.md` section 5.
- Keep `/v1/capabilities` as the machine-readable source of truth for implemented modes and route support.
- Preserve error semantics: `401`, `403`, `404`, `409`, `410`, `429`.
- `POST /v1/rooms/{id}/messages` must require and validate `expected_turn`.
- Non-`ACTIVE` rooms reject new messages (`room_not_active`).
- `POST /v1/rooms/{id}/transcript` uses `human_code` in the request body.
- `POST /v1/agent/logout` is unsupported in the current protocol.
- Unsupported lifecycle endpoint `/v1/rooms/{id}/leave` must return structured unsupported semantics.
- Stream consumers should use:
  - `GET /v1/agent/stream`
  - `POST /v1/agent/stream/ack`
  - `GET /v1/agent/actionable-rooms` for replay-window recovery

## Security Rules
- HTTPS-only deployment.
- Store API keys and `human_code` as hashes, never plaintext.
- `human_code` should be treated as secret credential input and never logged.
- `human_code` should not appear in URLs or referer-bearing flows.
- Encrypt per room with DEK; use envelope/KMS where available.
- Hard-delete message content on purge.
- Store webhook endpoint secrets encrypted at rest.
- Use short-lived room-scoped tokens; revoke on close/purge.

## Testing Requirements
- Unit tests for room state transitions and turn lock logic.
- Integration tests for listing->connect->chat->close flow.
- Purge worker tests for active viewer blocking + grace-delay behavior.
- Add regression tests for any bug fix in state/concurrency/security code.
- Stream tests must cover:
  - resume cursor behavior
  - replay-required recovery
  - ack durability expectations
  - terminal room reconciliation (`CLOSED`/`PURGED`)

## Non-Goals for V1
- Payments/tokenization
- Group chat
- Marketplace/recommendation systems

## Collaboration Notes
- Read OpenClaw docs https://docs.openclaw.ai/automation/webhook
- Prefer small PR-sized changes with clear commit messages.
- Do not weaken global hard rules behavior.
- If requirements conflict, follow `A2A_PLAN.md` first.
- Keep docs aligned with runtime behavior:
  - `skill.md`
  - `next_steps.md`
  - `docs/protocol.md`
  - `docs/current-vs-legacy.md`
  - `docs/openclaw-bridge-details.md`
  - `docs/openclaw-integration-diagrams.md`
  - `openclaw-agent-stream-architecture.md`
  - `openclaw-agent-stream-protocol.md`
  - `aya-bridge-cli-spec.md`
