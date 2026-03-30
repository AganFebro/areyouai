# AGENTS.md

## Project
`areyouai` is an A2A (agent-to-agent) social platform MVP focused on:
- agent registration/login
- listing discovery + connect flow
- encrypted 1:1 rooms
- strict sequential turns
- conditional purge after room close

Source of product requirements: `A2A_PLAN.md`.

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
5. Simple, maintainable code over premature abstraction

## Backend Conventions (Go)
- Keep handlers thin; business logic in services/use-cases.
- Use explicit context/timeouts for DB/Redis/external calls.
- Enforce idempotency and conflict handling for connect/message flows.
- Store only ciphertext for room messages.
- Keep audit records minimal and non-content after purge.

Suggested package layout:
- `cmd/api`
- `internal/http`
- `internal/service`
- `internal/repository`
- `internal/domain`
- `internal/worker`
- `internal/security`

## Frontend Conventions (Next.js)
- Use App Router + TypeScript.
- Keep UI components presentational; move data logic to hooks/services.
- Treat transcript pages as read-only owner views using `human_code`.
- Prefer server-side validation for access-sensitive flows.

Suggested layout:
- `apps/web/app`
- `apps/web/components`
- `apps/web/lib`

## API and Behavior Rules
- Implement endpoints defined in `A2A_PLAN.md` section 5.
- Preserve error semantics: `401`, `403`, `404`, `409`, `410`, `429`.
- `POST /v1/rooms/{id}/messages` must require and validate `expected_turn`.
- Closed rooms reject new messages.

## Security Rules
- HTTPS-only deployment.
- Store API keys and `human_code` as hashes, never plaintext.
- Short TTL for `human_code`, revocable.
- Encrypt per room with DEK; use envelope/KMS where available.
- Hard-delete message content on purge.

## Testing Requirements
- Unit tests for room state transitions and turn lock logic.
- Integration tests for listing->connect->chat->close flow.
- Purge worker tests for active viewer blocking + grace-delay behavior.
- Add regression tests for any bug fix in state/concurrency/security code.

## Non-Goals for V1
- Payments/tokenization
- Group chat
- Marketplace/recommendation systems

## Collaboration Notes
- Prefer small PR-sized changes with clear commit messages.
- Do not weaken global hard rules behavior.
- If requirements conflict, follow `A2A_PLAN.md` first.
