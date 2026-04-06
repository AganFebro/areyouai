# Bug Findings

This file records the detailed findings from the `bug-hunt-swarm` investigation into the live room-path failures.

Scope of this note:
- `GET /v1/rooms/{id}/state`
- `GET /v1/rooms/{id}/context`
- adjacent caller/runtime issues discovered during the investigation

This is a read-only diagnosis note. It does not claim every item is fully reproduced on the VPS unless explicitly stated.

Status legend:
- `open`: evidence-backed bug that still needs a fix
- `candidate`: plausible bug with strong code-path evidence but missing direct live proof
- `fixed`: already fixed in the current workspace

## Environment Notes

Evidence already gathered before this note:
- `chat_listings.room_id` exists
- `rooms.topic` exists
- `rooms.human_code_expires_at` exists
- `room_scoped_tokens.room_id` exists
- sampled `pg_stat_activity` did not show blocking queries at the moment checked
- `chat_listings.tags = null` rows were found and repaired manually
- the listing search null-tags bug is already fixed in the current workspace
- local Go tests passed

## Open Bugs

### 1. Room reconciliation is serialized behind a single service-wide mutex

Status: `fixed`

Severity: `P1`

Locations:
- `internal/service/a2a/service.go`

Relevant code:
- `reconcileRoom()` takes `s.mu.Lock()` in [service.go](/home/febrian/areyouai/internal/service/a2a/service.go#L1892)
- `/context` calls `GetPromptBundle()`, which calls `reconcileRoom()` in [service.go](/home/febrian/areyouai/internal/service/a2a/service.go#L1264)
- `/state` calls `GetRoomState()`, which calls `reconcileRoom()` in [service.go](/home/febrian/areyouai/internal/service/a2a/service.go#L1574)
- closed-room reconcile can load all messages and purge under this path in [service.go](/home/febrian/areyouai/internal/service/a2a/service.go#L1995)

Problem:
- The service uses one global mutex for room reconciliation, not a per-room lock.
- That means one slow reconcile path can stall unrelated `/state` and `/context` requests for other rooms inside the same API process.
- The most expensive branch is the closed/purge-eligible path, where the code loads all room messages before purging.

Why this matters:
- The symptom can appear as a timeout even when Postgres itself is not blocked.
- Sampling `pg_stat_activity` may look clean because the wait happens inside the Go process on `s.mu`, not only in the database.
- This also makes the blast radius larger than a single bad room.

Supporting evidence:
- `reconcileRoom()` takes the mutex for the whole reconcile path in [service.go](/home/febrian/areyouai/internal/service/a2a/service.go#L1892)
- The reconcile path can do DB transactions, row locking, viewer counting, message loading, purge work, and stream/webhook follow-up while still under the service lock
- Both `/state` and `/context` always pass through that shared reconcile path before returning

Missing evidence:
- No live trace yet showing one request waiting behind another on this mutex

Fastest proof step:
- Trigger `/v1/rooms/{id}/state` on a room that is `CLOSED` and old enough to be purge-eligible
- In parallel, hit `/v1/rooms/{other}/state` or `/v1/rooms/{other}/context`
- If the second request blocks until the first completes, the global mutex is the cause

Recommended fix:
- Remove the global service mutex from room reconciliation
- Use per-room coordination only where state transitions actually need serialization
- Keep rate-limit maps or other in-memory accounting behind their own narrower synchronization instead of reusing a global room-state lock

Resolution:
- `reconcileRoom()` now uses a per-room mutex keyed by room ID, so unrelated `/state` and `/context` requests no longer block each other inside one API process.

### 2. `/context` now uses bounded recent messages

Status: `fixed`

Severity: `P1`

Locations:
- `internal/service/a2a/service.go`
- `internal/httpapi/sql_handlers.go`
- `internal/repository/postgres/store.go`

Relevant code:
- `/context` handler calls `GetPromptBundle()` in [sql_handlers.go](/home/febrian/areyouai/internal/httpapi/sql_handlers.go#L634)
- `GetPromptBundle()` calls `buildBundleForRoom()` in [service.go](/home/febrian/areyouai/internal/service/a2a/service.go#L1264)
- `buildBundleForRoom()` now fetches only bounded recent messages through `ListRecentRoomMessages()` in [service.go](/home/febrian/areyouai/internal/service/a2a/service.go#L1302)
- `ListRecentRoomMessages()` selects only the newest bounded slice in [store.go](/home/febrian/areyouai/internal/repository/postgres/store.go#L404)

Problem:
- Every `/context` call used to reload the full transcript for the room.
- The service then decrypted every message before building the prompt bundle.
- This made `/context` O(total messages in room), even though callers use it as a lightweight pre-send read.

Why this matters:
- Long-lived rooms will get slower over time.
- A room with a large transcript can hit timeouts on `/context` even if all other API reads are healthy.
- The symptom will disproportionately affect automation loops that fetch context before every send.

Supporting evidence:
- `buildBundleForRoom()` now caps recent history to the prompt-builder window before decrypt/render work
- `decryptRoomMessages()` still walks the provided slice, but the slice is now bounded in [service.go](/home/febrian/areyouai/internal/service/a2a/service.go#L2801)
- The endpoint contract still encourages frequent `/context` fetches before send, so keeping the window bounded matters

Missing evidence:
- No live latency sample has yet been captured for the specific failing room after the fix

Fastest proof step:
- Compare `/v1/rooms/{id}/context` latency on a long room versus a fresh room with few messages
- If latency stays flat after the bounded fetch, this bug is resolved

Resolution:
- `buildBundleForRoom()` now uses bounded recent messages instead of the full transcript
- The bounded fetch is capped to the prompt-builder window, and a regression test covers the limit

## Candidate Bugs

### 3. Room-token auth no longer writes on every read

Status: `fixed`

Severity: `P2`

Locations:
- `internal/service/a2a/service.go`
- `internal/repository/postgres/store.go`

Relevant code:
- `AuthRoomAccess()` now refreshes room token expiry only when it is near expiry in [service.go](/home/febrian/areyouai/internal/service/a2a/service.go#L263)
- `TouchRoomScopedToken()` still performs `UPDATE room_scoped_tokens` in [store.go](/home/febrian/areyouai/internal/repository/postgres/store.go#L779), but only on the refresh path

Problem:
- `GET /state` and `GET /context` used to update `room_scoped_tokens` on every room-token read.
- That created avoidable row contention under concurrent automation.

Resolution:
- Room-token expiry is now refreshed only when the token is within the refresh threshold.
- Fresh read requests no longer perform a write just to authenticate.

Why this matters:
- Public symptoms can look like endpoint instability even though the issue is per-token write contention.
- This is easy to miss because the route looks like a read but still mutates storage.

Supporting evidence:
- The auth path now gates `TouchRoomScopedToken()` behind a near-expiry check
- Both `/state` and `/context` still go through `authRoomAccess()`, but only near-expiry tokens refresh

Missing evidence:
- No live latency sample yet showing the lower contention after deployment

Fastest proof step:
- Re-run `/state` or `/context` repeatedly with a fresh room token
- Confirm the token row is not updated until it nears expiry

### 4. `/context` and `/context/ack` now have separate observability labels

Status: `fixed`

Severity: `P3`

Locations:
- `internal/httpapi/sql_handlers.go`
- `internal/service/a2a/service.go`
- `packages/aya-bridge/src/bridge.js`

Relevant code:
- `/context` now advertises `context_ack_required` and `context_ack_path` in [sql_handlers.go](/home/febrian/areyouai/internal/httpapi/sql_handlers.go#L650)
- the HTTP access log and API request log now label `/context` as `room_context` and `/context/ack` as `room_context_ack` in [access_log.go](/home/febrian/areyouai/internal/httpapi/access_log.go#L81)
- `/context/ack` persists the read marker through `RecordRoomContextFetch()` in [service.go](/home/febrian/areyouai/internal/service/a2a/service.go#L1761)

Problem:
- The modern context flow is effectively a two-step operation:
  - `GET /context`
  - `POST /context/ack`
- The logs/metrics did not clearly separate those steps.

Why this matters:
- It weakens incident triage because the observed problem may be attributed to the wrong endpoint
- It can hide whether the real hot path is prompt generation or room-context persistence

Supporting evidence:
- The request logger now assigns distinct route labels for `/context` and `/context/ack`

Missing evidence:
- No live latency sample yet showing the two labels in a production dashboard

Fastest proof step:
- Inspect `api_request_logs.route_name` or the access log line for `room_context` vs `room_context_ack`

Resolution:
- `GET /context` and `POST /context/ack` now write distinct route labels into request logs and the access log line, so the two steps can be timed separately.

### 5. `/context` now tolerates auxiliary room-context read errors

Status: `fixed`

Severity: `P3`

Locations:
- `internal/service/a2a/service.go`

Relevant code:
- `buildBundleForRoom()` now treats non-`ErrNotFound` `GetRoomContext()` failures as best-effort metadata misses in [service.go](/home/febrian/areyouai/internal/service/a2a/service.go#L1313)

Problem:
- A transient or permission-related `room_context_state` read failure used to turn `/context` into a hard failure.
- That was brittle because the live room itself could still be readable and usable.

Resolution:
- `buildBundleForRoom()` now ignores auxiliary `room_context_state` read failures and continues building the prompt bundle.
- The failure is recorded best-effort as `room_context_read_failed`.

Why this matters:
- Auxiliary state outages can take out the main prompt-read path
- This is similar in shape to the transcript regression that was already fixed earlier for room-context reads

Supporting evidence:
- The prompt bundle path now keeps going when `GetRoomContext()` fails

Missing evidence:
- No live latency sample yet showing the degraded-but-usable path in production

Fastest proof step:
- Force `GetRoomContext()` to fail and verify `/context` still returns a prompt bundle

## Additional Adjacent Bugs

### 6. DeepSeek helper scripts expect `context.state`, but `/context` does not return it

Status: `open`

Severity: `P3`

Locations:
- `scripts/deepseek_agents.js`
- `scripts/testing/deepseek_template_agents.js`
- `internal/httpapi/sql_handlers.go`

Relevant code:
- scripts read `context.state` in [deepseek_agents.js](/home/febrian/areyouai/scripts/deepseek_agents.js#L77) and [deepseek_template_agents.js](/home/febrian/areyouai/scripts/testing/deepseek_template_agents.js#L84)
- `/context` does not include `state`; it returns `room_id`, hashes, `turn_index`, `next_turn`, `next_actor_id`, and prompt fields in [sql_handlers.go](/home/febrian/areyouai/internal/httpapi/sql_handlers.go#L646)

Problem:
- These helper scripts check a field that the endpoint never returns.
- The script currently falls through because missing `state` becomes `""`, but that means the caller is not actually using authoritative room state from `/context`.

Why this matters:
- It is a client contract bug
- It can produce confusing behavior during room close/purge handling

Recommended fix:
- Either remove the `context.state` check from the scripts
- Or fetch `/state` explicitly when the caller needs room state

### 7. Bridge wake path adds extra API work around the context flow

Status: `candidate`

Severity: `P3`

Locations:
- `packages/aya-bridge/src/bridge.js`

Relevant code:
- `wakeOpenClaw()` sends typing `start`, performs the wake, and sends typing `stop` in [bridge.js](/home/febrian/areyouai/packages/aya-bridge/src/bridge.js#L666)

Problem:
- The bridge adds extra room API traffic around the wake path.
- When operators describe the whole wake cycle as “context timeout,” the actual slow step may be typing, context, context ack, or the downstream wake POST.

Why this matters:
- It is primarily an observability issue
- It can make production debugging slower and lead to false blame on `/context`

Recommended fix:
- Add per-step timing logs for:
  - typing start
  - `GET /context`
  - `POST /context/ack`
  - wake POST
  - typing stop

## Fixed Bugs Relevant To This Investigation

### 8. `GET /v1/listings/search` was previously broken by `tags = null`

Status: `fixed`

Severity: `P2`

Locations:
- `internal/repository/postgres/store.go`
- `internal/httpapi/api.go`
- `internal/httpapi/sql_integration_test.go`

Problem:
- Older rows had `chat_listings.tags = null`
- Search logic assumed tags were a JSON array of strings

Resolution:
- Existing bad rows were repaired manually on the VPS
- Current workspace now normalizes missing tags to `[]` and hardens search against legacy malformed rows

## Recommended Proof Order

If continuing this investigation, the most efficient order is:

1. Check the failing room’s shape:
   - `state`
   - `closed_at`
   - message count
2. If the room is `CLOSED` and old enough:
   - test for global reconcile lock serialization with concurrent `/state` or `/context` requests
3. If the room is `ACTIVE` but large:
   - compare `/context` latency against a short room
4. If room-scoped tokens are involved:
   - sample for `UPDATE room_scoped_tokens`
5. Only after that:
   - separate GET `/context` from POST `/context/ack`

## Current Ranking

1. Global reconcile mutex causing room-read serialization
2. `/context` doing unbounded full-history load and decrypt
3. Room-token auth turning reads into writes
4. Context GET and ACK being conflated operationally
5. Auxiliary room-context reads still hard-failing `/context`
