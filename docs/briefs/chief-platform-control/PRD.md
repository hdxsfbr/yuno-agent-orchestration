# PRD: Chief of Staff Platform Control

(Andre, 2026-10-22. Kept as given; the parity matrix, design and waves are in this folder.)

**Status:** Proposed functionality, not a claim of existing implementation.

**Purpose/users:** Enable Andre and Yuno’s Chief of Staff to operate the entire platform through authenticated conversational tools with UI/API parity.

## Requirements
1. **Inventory and parity:** Inventory every existing UI/API action before implementation. Deliver a parity matrix mapping action, authorization, existing backend capability, tool coverage, gaps, and acceptance test. Discover actual interfaces; do not invent endpoints. Completion requires all inventoried actions covered with equivalent permissions and outcomes.
2. **Observability:** List and inspect runs; provide live, paginated, redacted logs, current agent, step, command, heartbeat, last activity, and outstanding tool/model waits. Distinguish executing, waiting, blocked, unknown, and stalled using timestamped evidence and documented freshness thresholds. A “running” label never proves liveness; missing telemetry must not imply progress.
3. **Recovery:** Support cancel, pause, resume, specific-step retry, and steering. Return request identity and distinguish receipt from worker acknowledgement and applied outcome. Require cancellation acknowledgement before retry; if absent, expose unresolved state and prohibit overlapping writers. Preserve checkout/checkpoints. Enforce per-branch worker leases and reject stale workers’ writes.
4. **Dependencies and MRs:** Block dependent work until the prerequisite MR is merged and required CI is green for the relevant exact SHA. Automatically resume eligible work without spending builder loops polling. Expose MR head, diff, mergeability, exact-SHA checks and CI logs. Provide safe branch updates from main with expected-head checks; report conflicts, invalidate stale checks, and require CI on the updated SHA before dependent building proceeds.
5. **Administration:** Support agents CRUD/archive, models, prompts, and tool permissions; workflows CRUD, DAG validation, versioning, and approval gates; schedules CRUD, pause/resume, timezone, and next-run; projects CRUD and repository/intake, budgets, and integrations management. Validate invalid/cyclic workflows; keep active runs pinned to their workflow version. Provide persisted readback and configuration rollback.
6. **Safety:** Reuse backend authorization and business logic; no direct database bypass. Audit actor, request, redacted before/after state, approval evidence, and outcome, including denials. Require mutation idempotency and concurrency versions. Routine authorized operations need no unnecessary gates; destructive actions and permission escalation require explicit human approval. Never self-grant privileges, fabricate approvals, or disclose secrets.

## Acceptance
- Diagnose stalled !17 from heartbeat/log evidence; safely cancel/retry without duplicate writers.
- Block !24 until !17 merges with green CI; automatically update from main, verify updated-SHA CI, then resume.
- CRUD agents, workflows, and schedules from chat with persisted readback.
- Reject and audit unauthorized actions.

**Priority:** Observability/recovery → dependency control → admin parity.

**Non-goals:** Authorization bypass, autonomous approval, or unrelated platform redesign.
