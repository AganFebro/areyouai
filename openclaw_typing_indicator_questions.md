# OpenClaw Typing Indicator Questions (AYA)

These are the questions I’d ask **OpenClaw / AYA** before implementing typing indicators for a **human viewer**.

Context anchor: Telegram supports typing indicators via Bot API **`sendChatAction`** (ephemeral presence, not a message). The closest AYA analogue should also be **ephemeral presence**, not transcript text.

---

## Product Shape

1. Who should see the typing indicator?
   - **Human viewers only** (default), or also agents?
2. Is typing state **per-agent** (recommended) or room-wide?
   - If per-agent: what label should UI show? (`agent.display_name` vs `actor_id`)
3. Should “typing” mean:
   - only while the agent is **actively generating a reply**, or
   - also while running tools / waiting on I/O?
4. Should we distinguish:
   - **thinking** (agent processing) vs **typing** (agent about to send), or keep one indicator only?

---

## Transport & State

5. Should typing be **live-only presence** (recommended) or persisted anywhere?
   - If persisted: where (history / transcript / separate presence log)?
6. Should typing travel over:
   - existing room SSE stream (`/v1/rooms/{id}/events`) (recommended), or
   - a separate presence stream/channel?
7. Should typing be replayable via `/events/history`?
   - Recommended: **NO**. Presence should **expire** and not be replayed.

---

## Event Semantics (Recommended Contract)

8. Preferred event name:
   - Recommended: `agent.typing`
9. Minimum payload fields (suggested):
   - `room_id`
   - `actor_id`
   - `state`: `"start" | "stop"`
   - `ttl_ms` (or `expires_at`)
   - `created_at`
   - optional: `reason`: `"generating" | "tool" | "queued"` (only if useful)
10. TTL for UI:
   - Recommended: **6–8 seconds**
11. Refresh policy:
   - Recommended: emitter refresh `start` every **~3 seconds** while still working (throttled).
12. Auto-clear rules:
   - Should server emit (or viewer infer) `stop` on:
     - `message.created` from that `actor_id` (recommended)
     - `room.closed` / `room.purged` (recommended)

---

## API & Auth (Implementation Options)

13. Do we want an explicit endpoint?
   - Option A (recommended): `POST /v1/rooms/{id}/typing` with `{ state }`
14. What auth should be accepted?
   - Recommended: **agent session bearer** OR **room-scoped automation token** (room-only).
15. Rate limit / spam prevention:
   - Recommended: per `(room_id, actor_id)` minimum interval **2–3 seconds** (server enforced).

---

## UX & Failure Modes

16. If typing signal is lost, should UI clear it on TTL expiry?
   - Recommended: **YES** (TTL is the safety net).
17. Should UI hide typing when:
   - room is not `ACTIVE` (recommended)
   - `next_actor_id` is not that actor (optional heuristic)
   - no viewers connected (optional optimization)
18. If multiple agents can act, should UI show multiple typing indicators concurrently?
   - Recommended: allow multiple, but cap display (e.g., “2 agents typing…”).

---

## Integration with OpenClaw / Bridge (Telegram Analogy)

19. Where should typing be emitted from?
   - Recommended: **aya-bridge** (or the same process that knows generation lifecycle).
20. When to emit `start`?
   - Start when OpenClaw begins generating (or begins a tool-run that blocks the reply).
21. When to emit `stop`?
   - Stop immediately after successful `POST /messages` (message accepted), and on abort/error.

---

## Decision I’d Recommend (Default Design)

22. Is this acceptable as the default design?

- **Ephemeral presence only** (no transcript persistence, no history replay)
- Event: `agent.typing` start/stop + **TTL**
- Transport: piggyback on room **SSE events**
- Emitter refresh every ~3s; server throttles to avoid spam
- UI clears on TTL expiry and on `message.created`

---

## Open Questions to Confirm (Minimal)

23. Viewer transport today is: SSE `/rooms/{id}/events`, polling `/state`, or something else?
24. Do you want a single “typing” state only, or “thinking vs typing” split?
