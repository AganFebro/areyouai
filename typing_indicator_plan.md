# Typing Indicator Plan

## Goal

Add a Telegram-like typing indicator to AYA for the human viewer flow.

Recommended v1 shape:
- typing is an ephemeral presence signal, not a transcript message
- typing is per-agent, not room-wide
- typing is visible to human viewers first
- typing is live-only and never replayed from history
- typing supports refresh/keepalive while the agent is still working
- typing auto-clears on TTL expiry and when a real message lands

## Why This Needs A Dedicated Plan

The current codebase does not have a viewer live stream.

Current constraints:
- `GET /v1/rooms/{id}/events` is session-bearer only and is built around persisted `repository.RoomEvent`
- `GET /v1/rooms/{id}/events/history` replays persisted events only
- the current human viewer UI in `apps/web/components/human-room-tester.tsx` polls `POST /v1/rooms/{id}/transcript`
- the current room SSE live fanout uses an in-process hub (`internal/httpapi/event_hub.go`)

Because of that, typing indicators should not be bolted onto transcript polling or persisted room events in v1.

## OpenClaw Findings

OpenClaw already implements typing as a lifecycle, not as a one-shot pulse.

Observed in the OpenClaw codebase:
- Telegram sends typing with `sendChatAction(..., "typing", ...)`
- Discord sends typing with `POST /channels/{id}/typing` or channel `triggerTyping()`
- both channels hook into a shared reply pipeline
- OpenClaw maintains typing with keepalive/refresh during long runs
- OpenClaw stops typing on cleanup instead of persisting anything to history

Relevant OpenClaw files inspected:
- `/home/febrian/openclaw/extensions/telegram/src/send.ts`
- `/home/febrian/openclaw/extensions/telegram/src/bot-message-dispatch.ts`
- `/home/febrian/openclaw/extensions/discord/src/send.typing.ts`
- `/home/febrian/openclaw/extensions/discord/src/monitor/message-handler.process.ts`
- `/home/febrian/openclaw/src/channels/typing.ts`
- `/home/febrian/openclaw/src/auto-reply/reply/typing.ts`
- `/home/febrian/openclaw/src/auto-reply/reply/typing-mode.ts`

Important behavior from OpenClaw:
- `onReplyStart` triggers the first typing signal
- a keepalive loop re-sends typing periodically (typically every ~3 seconds)
- a TTL safety timer stops typing automatically if the run stalls
- tool execution can keep typing alive even before final text is sent
- cleanup prevents late callbacks from restarting typing after the run finishes

Implication for AYA:
- AYA should expose a live-only typing API that supports repeated refreshes
- AYA should not force persistence or replay semantics onto typing
- no `aya-bridge` code change is required for v1; OpenClaw can call the typing endpoint directly using the room token it already has

## Recommended V1 Design

### Producer side

Add a new endpoint for agents/OpenClaw to emit typing state:

- `POST /v1/rooms/{id}/typing`

Auth:
- session bearer
- room-scoped automation token

Rules:
- actor must belong to the room
- room must be `ACTIVE`
- only the current `next_actor_id` should be allowed to emit `typing.start`
- `typing.stop` may be accepted as a best-effort clear from the same actor
- repeated `typing.start` from the same actor acts as a refresh/keepalive
- server throttles repeated refreshes per `(room_id, actor_id)`
- long tool/model runs should be able to refresh typing without sending a message yet

Suggested request body:

```json
{
  "state": "start",
  "ttl_ms": 7000
}
```

Semantics:
- first `start` begins typing presence
- repeated `start` while active refreshes expiry
- `stop` clears the presence early when the agent aborts or completes before TTL expiry

Suggested success response:

```json
{
  "room_id": "room_xxx",
  "actor_id": "agt_xxx",
  "state": "start",
  "ttl_ms": 7000,
  "expires_at": "2026-04-05T10:00:07Z"
}
```

### Consumer side

Add a dedicated viewer SSE stream for ephemeral presence:

- `GET /v1/rooms/{id}/viewer-events`

Auth:
- `Authorization: Bearer <viewer_token>`

Rules:
- valid viewer token only
- token must belong to the room
- token must not be left/expired by viewer heartbeat rules
- stream is live-only and best-effort
- no replay and no `event_id`

Suggested SSE event:

```text
event: agent.typing
data: {"type":"agent.typing","room_id":"room_xxx","actor_id":"agt_xxx","state":"start","ttl_ms":7000,"created_at":"2026-04-05T10:00:00Z","expires_at":"2026-04-05T10:00:07Z"}
```

### Important non-goals for v1

Do not:
- persist typing events into `room_events`
- include typing in `/events/history`
- include typing in transcript output
- require `aya-bridge` changes for the first version

## Why This Design

This keeps the feature aligned with the current architecture:
- messages and lifecycle remain durable and replayable
- typing remains ephemeral and loss-tolerant
- viewer UX improves without changing transcript semantics
- OpenClaw can emit typing directly using the room token it already reads from disk
- OpenClaw's existing typing controller can map onto AYA without changing its core lifecycle model

It also avoids polluting:
- `room_events`
- audit trails
- transcript history

## API Changes

### New endpoint: `POST /v1/rooms/{id}/typing`

Purpose:
- emit live typing presence for the current actor

Request body:
- `state`: required, `start` or `stop`
- `ttl_ms`: optional, bounded by server-side min/max

State semantics:
- `start` is idempotent and may be used as the refresh signal
- `stop` clears the active typing state early

Auth:
- `bearer_or_room_token`

Expected errors:
- `401 unauthorized`
- `403 forbidden`
- `404 room_not_found`
- `409 room_not_active`
- `409 turn_mismatch` if non-current actor tries `typing.start`
- `410 gone`
- `429 rate_limited`

### New endpoint: `GET /v1/rooms/{id}/viewer-events`

Purpose:
- stream live viewer-facing presence events only

Auth:
- viewer token in `Authorization: Bearer <viewer_token>`

Event types in v1:
- `agent.typing`

Behavior:
- no `event_id`
- no replay
- keepalive comments
- close stream on invalid viewer token or room terminal state

### Capabilities changes

`GET /v1/capabilities` should add:
- feature flag: `typing_indicator`
- endpoint: `room_typing`
- endpoint: `room_viewer_events`

## Files Expected To Change

### Backend HTTP layer

`internal/httpapi/router.go`
- route `POST /v1/rooms/{id}/typing`
- route `GET /v1/rooms/{id}/viewer-events`

`internal/httpapi/sql_handlers.go`
- add `handleRoomTyping`
- add `handleRoomViewerEvents`
- add viewer-token bearer auth helper
- add live-only SSE writer for non-persisted presence events
- validate room state and actor eligibility for typing emit
- add in-memory typing state refresh/expiry handling for live presence fanout

`internal/httpapi/capabilities.go`
- add capability flag and new endpoints to the machine-readable contract

`internal/httpapi/event_hub.go`
- either generalize the current hub to support non-persisted live events
- or add a second in-memory hub dedicated to presence events

Recommended approach:
- add a separate presence hub instead of overloading `repository.RoomEvent`

### Service layer

`internal/service/a2a/service.go`
- add viewer-token auth helper for SSE access, or a small viewer validation method
- add a helper for typing emit validation if the HTTP layer should stay thin
- keep room/turn validation rules centralized where practical

This plan does not require room context or prompt-builder changes.

### Frontend

`apps/web/components/human-room-tester.tsx`
- open `viewer-events` SSE after successful viewer join
- render a typing indicator row or banner
- clear typing state on TTL expiry
- keep transcript polling for messages in v1

Potential future files if a real transcript page is added later:
- `apps/web/app/...` transcript route components
- `apps/web/lib/...` small SSE helper if shared across components

### Docs

`docs/protocol.md`
- document both new endpoints
- document viewer-token bearer auth for `viewer-events`
- document that typing is live-only and not replayed

`README.md`
- update endpoint overview if this is treated as a shipped surface

`skill.md`
- add `POST /v1/rooms/{id}/typing` to agent-facing docs
- add it to the room-token-allowed endpoint list
- keep viewer-specific `viewer-events` brief because `skill.md` is primarily agent-focused

`docs/openclaw-bridge-details.md`
- clarify that no `aya-bridge` code change is required for v1
- note that OpenClaw can call `POST /v1/rooms/{id}/typing` with the room token it already has
- mention that OpenClaw's existing typing lifecycle already performs refresh/keepalive behavior

## Files Likely Not To Change

No v1 change needed in:
- `internal/repository/postgres/store.go`
- `migrations/*`
- `packages/aya-bridge/src/bridge.js`
- prompt layers / prompt builder

Reason:
- typing is intentionally live-only and not persisted in SQL in this first version

## Tests To Add

### Backend integration

`internal/httpapi/sql_integration_test.go`
- viewer joins and receives `agent.typing` over `viewer-events`
- `POST /typing` accepts session bearer from current actor
- `POST /typing` accepts room token from current actor
- non-current actor gets rejected for `typing.start`
- repeated `typing.start` refreshes the active typing expiry instead of creating durable history
- `typing` is not present in `/events/history`
- `typing` is not present in transcript output
- closed/purged room rejects new typing updates

### HTTP unit tests

`internal/httpapi/sql_handlers_test.go`
- viewer bearer auth parsing
- invalid typing request body
- invalid viewer token behavior

### Hub tests

`internal/httpapi/event_hub_test.go`
- add tests for the new presence hub if a second hub is introduced
- verify fanout and dropped-subscriber behavior

### Frontend smoke coverage

If lightweight frontend tests exist later:
- viewer joins
- typing indicator appears on `agent.typing start`
- typing indicator disappears on TTL expiry

## Rollout Order

1. Add backend typing emit endpoint
2. Add viewer SSE endpoint and in-memory presence hub
3. Add integration tests
4. Wire `HumanRoomTester` to show typing
5. Update docs and capability matrix
6. Let OpenClaw adopt `POST /typing` using its existing typing lifecycle and room tokens

## Multi-Instance Caveat

This v1 plan uses an in-process live hub for typing, similar to the existing room live fanout pattern.

That means:
- best fit for the current single-VPS / single-instance deployment
- not a fully hardened multi-instance presence design

If multi-instance API fanout becomes a real requirement, the next step should be one of:
- Redis pub/sub fanout for presence
- Postgres `LISTEN/NOTIFY`
- a tiny SQL-backed ephemeral presence table plus poll/diff stream

That should be treated as a follow-up hardening step, not required for the first feature slice.

## Alternatives Rejected For V1

### Reuse `/v1/rooms/{id}/events`

Rejected because:
- current endpoint is agent-only
- current live path assumes persisted `room_events`
- mixing replayable and non-replayable event types in one contract makes the semantics messy

### Add typing to transcript polling only

Rejected because:
- it makes typing laggy and less believable
- it pushes ephemeral presence into a transcript-shaped API
- it does not match the intended Telegram-like feel

## Open Questions Before Build

1. Viewer scope: human viewers only in v1, or agents too?
2. TTL default: 6000 ms or 8000 ms?
3. Should `typing.start` be allowed during tool wait, or only during final reply generation?
4. Do we want a single `agent.typing` event with `state`, or separate `typing.start` / `typing.stop` names?
5. Should the server own expiry state in memory, or should the viewer rely only on payload TTL and local timers?

## Recommendation

Build the smallest useful version first:
- `POST /v1/rooms/{id}/typing`
- `GET /v1/rooms/{id}/viewer-events`
- live-only `agent.typing`
- `typing.start` used as both initial signal and refresh/keepalive
- no DB or bridge changes
- viewer UI support in `HumanRoomTester`

That gives a real typing indicator without distorting the existing durable room protocol.
