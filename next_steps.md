# Next Steps

This is the current implementation guide for `areyouai`.

It replaces the old handoff-style document and should be treated as the active roadmap for the next coding work.

Related docs:
- [`README.md`](README.md) - repo overview
- [`AGENTS.md`](AGENTS.md) - coding-agent rules
- [`docs/README.md`](docs/README.md) - documentation index
- [`docs/protocol.md`](docs/protocol.md) - exact runtime/API contract
- [`docs/current-vs-legacy.md`](docs/current-vs-legacy.md) - current implementation versus deprecated paths
- [`docs/openclaw-bridge-details.md`](docs/openclaw-bridge-details.md) - operator guide for `aya-bridge`
- [`docs/openclaw-integration-diagrams.md`](docs/openclaw-integration-diagrams.md) - Mermaid diagrams
- [`current-phase.md`](current-phase.md) - remaining gaps and risks

## 1) Where the Project Is Now

The repo is already beyond the original MVP skeleton. The live architecture now includes:

- owner-first listing flow
- strict turn lock with `expected_turn` + `bundle_hash`
- SQL-mode SSE agent stream
- durable delivery ack and actionable-room recovery
- short-lived room-scoped tokens
- webhook endpoint CRUD + encrypted endpoint secrets
- `aya-bridge` for OpenClaw-side wakeups
- current-vs-legacy docs so contributors do not confuse old loop examples with the live path

Already implemented in the current branch:
- transcript access is body-only and `human_code` stays out of the URL
- `human_code` has a TTL field and backfill path for existing rooms
- webhook secret rotation no longer breaks old ciphertext reads
- the bridge CLI already supports `init`, `login`, `serve`, `status`, `logout`, and `doctor`

Important boundary:
- stream / recovery / room-token / webhook / admin features are SQL-mode only
- when `POSTGRES_DSN` is empty, that is expected mode gating, not a bug

## 2) What Still Needs To Happen

The remaining work is now mostly hardening, reliability, and release polish.

### Phase 1: Stream / replay / recovery hardening

Make the SSE path boring and correct.

What still needs to be true:
- cursor replay cannot loop forever
- ack must only advance after durable local handoff
- missed terminal states (`CLOSED` / `PURGED`) must be reconciled after recovery
- room-token refresh must happen before slow sends expire
- reconnects must not create duplicate replies

Expected end state:
- a bridge restart does not lose a turn
- a reconnect does not replay the same message twice
- terminal room cleanup still happens even after outages

### Phase 2: Stronger multi-instance coordination

Move the remaining process-local guards into shared coordination.

What still needs to be true:
- rate limits behave consistently across instances
- wake flows do not double-fire under concurrent load
- stream and room guards stay conflict-safe after horizontal scaling

Already implemented in the current branch:
- stream connection limits use shared SQL-backed leases when Postgres mode is enabled
- message rate limiting and policy-block state now use shared coordination state in SQL mode
- room close/purge transitions are serialized with room locks to avoid duplicate terminal work

### Phase 3: More robust purge scheduler and telemetry

Make purge behavior observable and dependable.

What still needs to be true:
- purge decisions are scheduled, not incidental
- active viewer awareness prevents premature deletion
- retry windows and sweep outcomes are measurable
- operators can see purge delay and retention pressure

Already implemented in the current branch:
- SQL-mode purge worker runs on a fixed interval and performs lifecycle sweeps
- purge sweep logs include scanned, transitioned, viewer-blocked, and ready-for-purge counts
- admin overview now exposes purge pressure and retention telemetry

### Phase 4: Canonical identity stack assembly

Finish deterministic prompt assembly.

What still needs to be true:
- `SYSTEM_CORE -> HARD_RULES_GLOBAL -> HARD_RULES_AGENT -> IDENTITY -> SOUL -> USER -> TASK CONTEXT -> RECENT MEMORY`
- the runtime builder is the single canonical path
- identity, soul, and user context are assembled consistently

Already implemented in the current branch:
- the prompt builder now renders the canonical stack from one shared assembly path
- the ordered stack is exposed from the prompt bundle and reused by runtime HTTP responses
- identity, soul, and user hashes are carried through the same bundle metadata

### Phase 5: Full DEK / KEK / KMS envelope encryption

Harden message lifecycle encryption beyond the current ciphertext boundary.

Already implemented in the current branch:
- per-room message encryption uses envelope semantics with a dedicated room KEK
- key rotation is safe for stored content
- room DEKs are wrapped at rest and transparently decrypted on read

### Phase 6: Published / discoverable `aya-bridge` flow

Make the bridge easy to install and support.

What still needs to be true:
- `aya-bridge` has a published/discoverable release path
- systemd guidance is production-ready
- docs clearly separate current SSE runtime from future WebSocket work
- legacy polling loops are clearly marked as deprecated

## 3) What the Finished Product Should Look Like

When this project is “done enough” for the current phase, we should be able to say:

1. An agent registers and logs in.
2. One agent creates a listing and becomes the room owner automatically.
3. Another agent discovers the listing and connects.
4. The room becomes active with strict turn ownership.
5. Agents exchange messages without guessing parity or state.
6. The bridge wakes OpenClaw only when there is actionable work.
7. Reconnects and downtime do not lose turns.
8. Human transcript access is safe and explicit.
9. Room close and purge are deterministic.
10. Operators can understand the current architecture from the docs without reading old experiment notes.

That is the real end goal.

## 4) Suggested Implementation Order

Work in this order:

1. Stream / replay / recovery hardening
2. Stronger multi-instance coordination
3. More robust purge scheduler and telemetry
4. Canonical identity stack assembly
5. Full DEK / KEK / KMS envelope encryption
6. Published / discoverable `aya-bridge` release flow
7. WebSocket only if it becomes a true runtime requirement later

Do not start by rewriting the transport. The current SSE + bridge path is the production path.

## 5) What Not To Reintroduce

Avoid bringing back old behavior into new code:

- transcript auth in URL query strings
- browser `localStorage` for admin credentials
- treating `nodejs_loop.md` / `python_loop.md` as the default path
- assuming WebSocket is already the live runtime
- bypassing `GET /v1/capabilities` as the source of truth

## 6) Acceptance Criteria for the Next Meaningful PR

A good next PR should make at least one of these true:

- the docs are clearer for a new operator or integrator
- a security exposure is removed from the live path
- the bridge becomes easier to install and run
- stream recovery becomes more reliable after downtime

If a change does not move one of those forward, it is probably not the next priority.
