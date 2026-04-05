# Bugs Discovered

This file records bugs discovered during review/debugging in this workspace.

Status legend:
- `open`: still needs a code change.
- `fixed`: already fixed in the current workspace, kept here as a record.

## Open Bugs

## Fixed Bugs

### 1. Replay recovery could lose actionable rooms

Status: `fixed`

Severity: `P1`

Locations:
- `packages/aya-bridge/src/bridge.js`
- `packages/aya-bridge/test/run.js`

Problem:
- `processRecovery()` used to clear `last_acknowledged_delivery_id` before `GET /v1/agent/actionable-rooms` succeeded.
- A failed recovery attempt could make the next reconnect omit `last_delivery_id` and skip actionable rooms.

How to fix:
- Keep the previous resume cursor until actionable-room recovery succeeds.
- Clear and persist the cursor only after recovery data has been fetched and queued successfully.
- The bridge now keeps the old cursor on recovery failure and clears it only after success, with a regression test covering the failure path.

### 2. Context fetch markers were written too early

Status: `fixed`

Severity: `P1`

Locations:
- `internal/httpapi/sql_handlers.go`
- `internal/service/a2a/service.go`
- `internal/httpapi/sql_integration_test.go`

Problem:
- `/v1/rooms/{id}/context` used to persist the read marker before the agent had successfully received the bundle.
- That could mark `read_by_opponent` true on failed responses or disconnects.

How to fix:
- `/context` now returns the fetched `turn_index` and an explicit `/context/ack` path.
- The agent must POST `/v1/rooms/{id}/context/ack` after it successfully parses the bundle.

### 3. Oversize messages now return a dedicated error

Status: `fixed`

Severity: `P2`

Locations:
- `internal/security/policy.go`
- `internal/service/a2a/service.go`
- `internal/httpapi/api.go`
- `internal/httpapi/sql_handlers.go`
- `internal/httpapi/sql_integration_test.go`

Problem:
- Oversize message rejections were previously collapsed into a generic `policy blocked` response.
- Clients could not distinguish payload length failures from actual policy violations.

How to fix:
- Oversize payloads now return `413 payload_too_large` with `max_chars: 8192`.
- Generic `policy blocked` remains reserved for actual policy violations.
- Added regression coverage for both the API mapping and SQL-mode message send path.

### 4. Normal reconnecting disconnects are downgraded

Status: `fixed`

Severity: `P3`

Locations:
- `packages/aya-bridge/src/bridge.js`
- `packages/aya-bridge/test/run.js`

Problem:
- The bridge logged expected stream disconnects like `terminated` as warnings.
- Normal reconnect churn looked like a real failure in logs.

How to fix:
- Expected disconnects now log at `info`.
- Unexpected disconnects still log at `warn`.
- Added a regression test that simulates a `terminated` stream read and asserts it is downgraded.
- Transcript read receipts now come only from acked context fetches, not request start.
- Added regression coverage for ack success and ack failure paths.

### 5. Markdown transcript links allowed unsafe URL schemes

Status: `fixed`

Severity: `P1`

Locations:
- `apps/web/components/markdown-message.tsx`

Problem:
- Transcript markdown links previously rendered arbitrary `href` values directly into `<a>`.
- A `javascript:` or `data:` URL could execute browser-side code when a viewer clicked it.

How to fix:
- Whitelist safe schemes only.
- The current workspace now allows only `http:`, `https:`, `mailto:`, and `tel:` and renders everything else as plain text.

### 6. Viewer SSE did not enforce heartbeat freshness

Status: `fixed`

Severity: `P1`

Locations:
- `internal/httpapi/sql_handlers.go`

Problem:
- `/v1/rooms/{id}/viewer-events` previously checked only that the viewer row existed and was not left.
- A stale viewer token could continue receiving typing events after heartbeat expiry.

How to fix:
- Check `LastHeartbeatAt` against the heartbeat timeout when opening the stream.
- Re-check freshness periodically while the SSE is open.
- The current workspace now enforces this and closes stale streams.

### 7. Typing snapshot and subscription had a race

Status: `fixed`

Severity: `P2`

Locations:
- `internal/httpapi/typing_hub.go`
- `internal/httpapi/sql_handlers.go`

Problem:
- Viewer typing SSE took a snapshot before the subscription was fully established.
- A `typing.start` could be lost in that gap or duplicated around connect time.

How to fix:
- Subscribe and snapshot under the same lock.
- The current workspace adds `SubscribeWithSnapshot()` and uses it from `/viewer-events`.

### 8. Main viewer UI never consumed typing SSE

Status: `fixed`

Severity: `P2`

Locations:
- `apps/web/components/human-room-tester.tsx`
- `apps/web/app/globals.css`

Problem:
- The viewer page polled transcript data and heartbeats only.
- It never opened `/viewer-events`, so typing indicators never reached the DOM.

How to fix:
- Open the authenticated typing SSE from the browser, track active typing actors, and render them in the main transcript UI.
- The current workspace now does this with a streamed `fetch()` reader and visible typing state.

### 9. `/context` used to fail when fetch-marker persistence failed

Status: `fixed`

Severity: `P1`

Locations:
- `internal/httpapi/sql_handlers.go`

Problem:
- A failing room-context write blocked prompt-bundle retrieval entirely.
- That made `/context` unusable even though the prompt bundle itself could still be generated.

How to fix:
- Treat fetch-marker persistence as best-effort.
- The current workspace now logs `room_context_sync_failed` and still returns the prompt bundle.

### 10. Room-context fetch markers could be lost by blind overwrite

Status: `fixed`

Severity: `P2`

Locations:
- `internal/repository/postgres/store.go`
- `internal/service/a2a/service.go`

Problem:
- Room-context updates used a read-modify-write followed by a blind upsert.
- Concurrent `/context`, send, or close flows could overwrite each other and drop `LastContextFetchTurnByAgent`.

How to fix:
- Use optimistic versioned writes at the store layer.
- Retry and re-merge from fresh state in the service on conflict.
- The current workspace now does this and includes regression coverage.

### 11. `/transcript` used to fail on auxiliary room-context lookup errors

Status: `fixed`

Severity: `P2`

Locations:
- `internal/service/a2a/service.go`

Problem:
- Transcript generation became dependent on `GetRoomContext()` succeeding.
- A room-context read failure turned an otherwise readable transcript into a `500`.

How to fix:
- Treat room-context lookup as optional metadata for transcript rendering.
- The current workspace now returns the transcript with an empty read-receipt map when the auxiliary lookup fails.
