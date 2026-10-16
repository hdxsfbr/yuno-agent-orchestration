# Wave 5 frontend brief: Chief of Staff in the UI

Spec: `docs/briefs/wave5/00-design-chief-of-staff.md` (Outputs section) and
`docs/DESIGN.md` 5.15. The backend (another agent, do not touch `internal/`) adds:

- runs with `trigger: "channel"` and `trigger_ref: "conversation:<id>"`;
- `tool_calls` with `run_id` null and `conversation_id` set (chat tool calls), served
  either at `GET /api/conversations/{id}/tool-calls` or inside the conversation's
  messages endpoint (the backend's report says which; code against both shapes
  defensively until then, with the client in `web/src/lib/api.ts`);
- `GET /api/tools` entries with `chat: true` for the six chat-capable tools;
- conversation messages with `run_id` set for platform reports (text starting with
  `[platform]`).

## Deliverables

1. Runs page (`web/src/pages/RunsPage.tsx`): trigger `channel` shown as a badge
   "Delegated" with a link to `/channels/<conversation id>` parsed from `trigger_ref`.
2. Run page (`RunDetailPage.tsx` / `components/run/RunHeader.tsx`): "Delegated from a
   conversation" with the same link.
3. Channels page (`ChannelsPage.tsx`, `components/channels/*`): platform report messages
   rendered as a system notice (not a user bubble) with a link to the run when
   `run_id` is set; chat tool calls listed under the transcript (tool, status, time,
   collapsed input/output with `JsonView`) when the endpoint exists.
4. Agent form (`components/agent/*` tools section): tools with `chat: true` carry a
   "Chat" badge and a one-line hint that these work in a conversation; the seeded Chief
   of Staff needs no special casing.
5. Types in `web/src/lib/api.ts` for the new fields; `useEventStream` consumers refresh
   the conversation on `tool_call` events that carry `conversation_id` (if the backend
   emits them; otherwise on the existing `conversation.message` event).
6. Vitest tests for 1, 3 and 4 (render with fixture data; no network). `make test-web`
   and `make lint` clean (eslint, tsc).

## Rules of the house

- Everything runs in Docker through the Makefile (`make test-web`, `make lint`). Nothing
  installed on the host. Do not touch `internal/`, `api/`, `docs/` or the Makefile.
- No developer commentary in product copy: labels and hints are for the end user.
- Match the existing components (Badge, Card, JsonView, QueryState) and the pages'
  patterns for live refresh.
- Report at the end: files changed, anything you interpreted, anything unverified.
