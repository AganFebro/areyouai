# OpenCode Agent Implementation Log

This file documents the implementation work completed by the OpenCode agent session.

## Session Summary

Completed two priority phases from `next_implementation_handoff.md`:
1. `aya-bridge` onboarding and production install flow
2. Security hardening for credential exposure gaps

---

## Phase 1: `aya-bridge` Onboarding

### Goal
Make the OpenClaw-side bridge easy to install and operate without a public port, reverse proxy, or manual polling loop.

### What Was Implemented

#### 1. Updated `skill.md` (v1.3.0 → v1.4.0)

Added new section `## 12) OpenClaw Bridge (aya)` containing:
- What the bridge does / does not do
- Repo-local install path (package not yet published to npm)
- Quickstart: `aya init` → `aya login` → `aya serve`
- `~/.areyouai/` file layout
- Production systemd unit example
- Operational checklist table
- Token refresh / wake queue notes
- Transport note: SSE is current, WebSocket is future

File: `skill.md` (lines 620-745)

#### 2. Updated `packages/aya-bridge/README.md`

Expanded from 96 → 180 lines with:
- Clear "what it does / doesn't do" sections
- Transport note (SSE current, WebSocket future)
- Repo-local install examples
- Quickstart section
- Production systemd unit with User/Group/WorkingDirectory
- Operational checklist table
- Token refresh / wake queue details

File: `packages/aya-bridge/README.md`

### Key Decisions

1. **Install path**: Documented repo-local install (`npm install -g ./packages/aya-bridge`), not npm registry, since package is not published yet.

2. **Transport**: Explicitly documented that current runtime is SSE, WebSocket is future target. This prevents integrators from wiring the wrong client.

3. **Systemd**: Production-ready example with all required fields (User, Group, WorkingDirectory, Environment, Restart policy).

### Tests Verified
- `packages/aya-bridge/test/run.js`: All 3 tests passed
- `internal/httpapi/router_test.go`: Capabilities endpoint tests passed
- All 53 Go tests passed

---

## Phase 2: Security Hardening

### Goal
Close the remaining credential exposure gaps that are still live in the current phase.

### Security Audit Findings

| Risk Level | Issue | Location |
|------------|-------|----------|
| HIGH | `human_code` in URL query string | `apps/web/components/human-room-tester.tsx:87`, `internal/httpapi/api.go:756`, `internal/httpapi/sql_handlers.go:771` |
| HIGH | Admin token in localStorage | `apps/web/components/admin-dashboard.tsx:58,127,132` |
| MEDIUM | No `human_code` TTL/revocation | `internal/repository/contracts.go`, `internal/service/a2a/service.go` |
| MITIGATED | Query string logging | `internal/httpapi/access_log.go:188` already redacts sensitive params |

### What Was Implemented

#### 1. Move `human_code` from URL Query String to POST Body

**Backend Changes:**

| File | Change |
|------|--------|
| `internal/httpapi/api.go:748` | Changed transcript endpoint from `GET` to `POST`, added `transcriptRequest` struct |
| `internal/httpapi/sql_handlers.go:766` | Changed transcript endpoint from `GET` to `POST`, reads `human_code` from request body |
| `internal/httpapi/capabilities.go:72` | Updated endpoint method to `POST` |

**Frontend Changes:**

| File | Change |
|------|--------|
| `apps/web/components/human-room-tester.tsx:80-109` | Updated fetch to POST with JSON body containing `human_code` |

**Test Updates:**

| File | Change |
|------|--------|
| `internal/httpapi/api_test.go:192-199` | Updated transcript tests to use POST |
| `internal/httpapi/sql_integration_test.go:235` | Updated integration test to use POST |

#### 2. Add `human_code` TTL (24 Hours)

**Database Migration:**

| File | Change |
|------|--------|
| `migrations/000009_human_code_ttl.up.sql` | `ALTER TABLE rooms ADD COLUMN human_code_expires_at TIMESTAMPTZ` |
| `migrations/000009_human_code_ttl.down.sql` | `ALTER TABLE rooms DROP COLUMN human_code_expires_at` |

**Repository Layer:**

| File | Change |
|------|--------|
| `internal/repository/contracts.go:131` | Added `HumanCodeExpiresAt *time.Time` to `Room` struct |
| `internal/repository/contracts.go:313` | Added `HumanCodeExpiresAt *time.Time` to `CreateRoomInput` |
| `internal/repository/postgres/store.go:265,306,793` | Updated SELECT queries to include `human_code_expires_at` |
| `internal/repository/postgres/store.go:294-302,781-790` | Updated INSERT queries with new column |
| `internal/repository/postgres/store.go:983-1008` | Updated `scanRoom()` to scan new column |
| `internal/repository/postgres/store.go:1088-1095` | Added `nullableTime()` helper function |

**Service Layer:**

| File | Change |
|------|--------|
| `internal/service/a2a/service.go:39` | Added constant `humanCodeTTL = 24 * time.Hour` |
| `internal/service/a2a/service.go:527-536` | Set `HumanCodeExpiresAt` on room creation (listing create) |
| `internal/service/a2a/service.go:645-656` | Set `HumanCodeExpiresAt` on room creation (connect) |
| `internal/service/a2a/service.go:1415-1418` | Check TTL expiry in `Transcript()` |
| `internal/service/a2a/service.go:1448-1451` | Check TTL expiry in `ViewerJoin()` |

**In-Memory Mode:**

| File | Change |
|------|--------|
| `internal/httpapi/app.go:63-79` | Added `HumanCodeExpiresAt *time.Time` to `room` struct |
| `internal/httpapi/api.go:17` | Added constant `humanCodeTTL = 24 * time.Hour` |
| `internal/httpapi/api.go:38-41` | Added `ptrTime()` helper |
| `internal/httpapi/api.go:291-306` | Set TTL on room creation (listing create) |
| `internal/httpapi/api.go:419-434` | Set TTL on room creation (connect) |
| `internal/httpapi/api.go:790-797` | Check TTL expiry in transcript handler |
| `internal/httpapi/api.go:848-855` | Check TTL expiry in viewer join handler |

#### 3. Admin Token: localStorage → sessionStorage

| File | Change |
|------|--------|
| `apps/web/components/admin-dashboard.tsx:58` | Changed `localStorage.getItem()` → `sessionStorage.getItem()` |
| `apps/web/components/admin-dashboard.tsx:127` | Changed `localStorage.setItem()` → `sessionStorage.setItem()` |
| `apps/web/components/admin-dashboard.tsx:132` | Changed `localStorage.removeItem()` → `sessionStorage.removeItem()` |

**Why sessionStorage?**
- Cleared when browser/tab closes (reduced persistence)
- Not accessible across tabs (reduced exposure)
- Still accessible to XSS, but reduces long-term credential leakage risk

#### 4. Documentation Updates

| File | Change |
|------|--------|
| `skill.md:248-258` | Added note about 24-hour `human_code` TTL |
| `skill.md:560-598` | Added new section `### POST /v1/rooms/{id}/transcript` |
| `docs/protocol.md:109` | Added `human_code` expiry note |
| `docs/protocol.md:444-484` | Added section `## 14) POST /v1/rooms/{id}/transcript` |
| `docs/openapi.yaml:169-191` | Changed transcript endpoint from GET to POST with body |

### Migration Required

Before deploying, run migration `000009`:

```bash
rtk go run ./cmd/migrate
```

### Tests Verified

```
Go test: 53 passed in 17 packages
Frontend build: ✓ Compiled successfully
npm run build: ✓ Generating static pages
```

### Breaking Changes

1. **Transcript endpoint changed from `GET` to `POST`**
   - Clients must update to send `human_code` in POST body
   - Old `GET` requests will return `405 Method Not Allowed`

2. **`human_code` now expires after 24 hours**
   - Existing `human_code` values (created before this change) will have `NULL` expiry
   - NULL expiry = no expiry (backward compatible)
   - New `human_code` values will have 24-hour TTL

---

## Files Changed Summary

### Phase 1: aya-bridge Onboarding
- `skill.md` (version bump + new section)
- `packages/aya-bridge/README.md` (expanded)

### Phase 2: Security Hardening
- `migrations/000009_human_code_ttl.up.sql` (new)
- `migrations/000009_human_code_ttl.down.sql` (new)
- `internal/repository/contracts.go`
- `internal/repository/postgres/store.go`
- `internal/service/a2a/service.go`
- `internal/httpapi/app.go`
- `internal/httpapi/api.go`
- `internal/httpapi/sql_handlers.go`
- `internal/httpapi/capabilities.go`
- `internal/httpapi/api_test.go`
- `internal/httpapi/sql_integration_test.go`
- `apps/web/components/human-room-tester.tsx`
- `apps/web/components/admin-dashboard.tsx`
- `docs/protocol.md`
- `docs/openapi.yaml`

---

## Remaining Work

From `next_implementation_handoff.md`:

| Priority | Item | Status |
|----------|------|--------|
| 1 | `aya-bridge` onboarding | ✅ Complete |
| 2 | Security hardening | ✅ Complete |
| 3 | WebSocket transport | ⏳ Not started (deferred) |

### Future Considerations

1. **npm publish**: Once package is published, update docs to use `npm install -g @febro28/aya-bridge`

2. **`human_code` revocation**: Current implementation has TTL but no explicit revocation mechanism. Future work may add:
   - Owner-initiated revocation endpoint
   - Rotation capability
   - Per-room revocation list

3. **WebSocket transport**: Deferred until SSE proves insufficient or explicit decision to migrate.

---

## Session Info

- **Agent**: OpenCode (glm-5)
- **Date**: 2026-04-03
- **Working Directory**: `/home/febrian/areyouai`
- **Command Prefix**: `rtk`
