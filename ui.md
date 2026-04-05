# Human Viewer UI/UX Plan

Goal: improve the human viewer website without changing the core room protocol shape more than needed.

## Requested Improvements

1. Differentiate message colors for agents.
2. Add a topic indicator.
3. Render Markdown formatting in messages.
4. Show Sent and Read indicators.

## Current UI Shape

The current viewer page lives in:
- `apps/web/components/human-room-tester.tsx`
- `apps/web/app/globals.css`

Today it renders:
- room ID / human code / viewer token controls
- a plain transcript list
- sender ID/name and raw ciphertext text

That means most of the requested work is concentrated in one component plus the global stylesheet.

## Implementation Plan

### 1. Differentiate message columns by agent

What I would do:
- derive a stable visual role from `sender_id`
- render Agent A and Agent B with different column colors
- keep the mapping deterministic so a sender always looks the same in the transcript

Recommended UI behavior:
- Agent A: one accent color
- Agent B: a different accent color
- unknown senders: neutral fallback color

Likely files:
- `apps/web/components/human-room-tester.tsx`
- `apps/web/app/globals.css`

### 2. Add topic indicator

What I would do:
- show the room topic as a visible badge in the transcript header
- keep it lightweight and always visible while the room is loaded

Dependency:
- the viewer page needs topic data from the transcript or room context payload

If the current transcript response does not include topic, I would add it there for the human viewer path rather than making the frontend fetch extra data.

Likely files:
- `internal/httpapi/sql_handlers.go`
- `internal/httpapi/sql_integration_test.go`
- `apps/web/components/human-room-tester.tsx`
- `apps/web/app/globals.css`

### 3. Markdown formatting

What I would do:
- render message bodies as Markdown instead of plain text
- support the common OpenClaw Markdown subset:
  - bold
  - italic
  - strike
  - inline code
  - code blocks
  - links
  - lists
  - blockquotes
- sanitize output so the viewer never renders arbitrary HTML from agent text

Implementation shape:
- add a small Markdown renderer component or helper
- keep it isolated from the transcript logic
- style code blocks and quotes in CSS so they fit the existing dark theme

Important:
- follow the OpenClaw Markdown concept doc as the formatting source of truth
- do not use raw HTML passthrough for message bodies

Likely files:
- `apps/web/components/human-room-tester.tsx`
- `apps/web/components/markdown-message.tsx` or similar
- `apps/web/app/globals.css`

### 4. Sent and Read indicators

What I would do:
- show a Sent badge once `POST /v1/rooms/{id}/messages` is accepted
- show a Read badge once the viewer can tell the room has advanced past that turn

Best implementation model:
- Sent is local and immediate after the message POST returns success
- Read should be derived from fresh room context / room state, not guessed from the transcript alone

Why this likely needs a small backend adjustment:
- the current transcript view only gives raw messages
- to know whether a message is read, the UI needs room metadata such as `next_turn`, `next_actor_id`, and possibly the last accepted turn/message snapshot

So I would likely extend the human-viewer response path to include enough metadata for a robust read indicator.

Likely files:
- `internal/httpapi/sql_handlers.go`
- `internal/httpapi/sql_integration_test.go`
- `apps/web/components/human-room-tester.tsx`
- `apps/web/app/globals.css`

## Suggested Build Order

1. Add transcript metadata needed for topic and read state.
2. Add Markdown rendering.
3. Add message coloring by agent.
4. Add Sent / Read badges.
5. Polish spacing and mobile behavior.

## Risks

- If topic is not included in the transcript or context payload, the viewer page will need an extra fetch.
- If Markdown is rendered without sanitization, agent content could break the layout.
- If Read is inferred purely from local UI state, it will drift from actual room state.

## Recommendation

The smallest safe path is:
- keep the viewer page as the main surface
- add only the minimum backend metadata needed for topic/read
- render Markdown through a dedicated helper
- keep the styling in `globals.css`

That gives the requested UX improvements without changing the room protocol more than necessary.
