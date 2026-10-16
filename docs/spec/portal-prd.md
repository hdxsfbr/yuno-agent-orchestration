# Portal project brief (copied 2026-10-15 from the Gauntlet portal, verbatim page text)

Title: Gauntlet Portal
URL: https://portal.gauntletai.com/projects/2GaNjUiJQp6JkxmKdDAN
Source element: <main>
---
Back to Partner Projects

PROJECT BRIEF

AI Agent Orchestration Platform

Yuno

SILVER
Version 1
AI-SOLUTION

Last updated 10/12/2026

PRD

Teams need domain-specific AI agents that can research, make decisions, and take action across systems with controlled tool access, multi-step execution, agent-to-agent collaboration, memory boundaries, operating limits, and human oversight. Build a platform where users configure agents (roles, instructions, model settings, tool allowlists, limits, schedules, memory) without code changes and connect them into collaborative workflows. Agents must run on a real runtime, execute real tools, and cooperate to complete tasks autonomously, with a human approval gate before any risky or side-effecting action. At least one agent must be reachable through an external messaging channel (Slack, Telegram, or WhatsApp) — with verified inbound webhooks — so a human can interact with it conversationally. The platform must include a web UI for creating/editing agents and workflows, triggering runs (manual plus one recurring/delayed schedule), inspecting execution traces, and approving or rejecting actions.

PROBLEM & CONTEXT

BUSINESS CONTEXT

Deliver a working repository (with commit history) and a root README covering setup, configuration, and demo steps. Include an architecture diagram + decision record (ADR), a threat model and known-limitations section, API/OpenAPI docs, an AI_USAGE.md, an assumptions-and-decisions log, and a reproducible evaluation report (baseline + final). Provide a one-command test/eval path (make test, make run, make eval) and a .env.example with names only (no secrets). Deliver a 5–10 min walkthrough video showing configuration, a successful run, a handled failure, an approval gate, execution traces, evaluations, and a live messaging conversation. Timebox ~3 focused days; language is candidate's choice (Kotlin/Go suggested).
Evaluation weights: Runtime & orchestration 25%, Evaluation engineering 20%, Software engineering & architecture 15%, Tools/safety/security 15%, Architectural judgment & communication 10%, Product completeness 8%, Observability & operations 7%.

IMPACT METRICS

- 100% of runs terminate within configured step/time limits
- 100% of approval-gated actions blocked until explicitly approved
- 100% idempotency: repeated side-effect requests produce at most one effect
- ≥90% deterministic-evaluator pass rate on the public test split
- ≥80% end-to-end task success on the public test split
- API status endpoints respond within 500 ms at p95
- Zero secrets in logs, traces, fixtures, or the repository
- ≥20 versioned, split eval cases; ≥3 deterministic evaluators + ≥1 rubric-based

REQUIREMENTS & SUCCESS CRITERIA

FUNCTIONAL REQUIREMENTS

Agent CRUD (name, role, system prompt, model, tool allowlist, operating limits, channels); Agent configuration (schedules, run-scoped + conversational memory with retention/expiration policy, guardrails, interaction rules); Workflow builder to create/edit agents and multi-agent workflows with conditions/branching; ≥1 manual trigger plus ≥1 recurring or delayed schedule; ≥2 cooperating reasoning components with shared typed/validated run state; 3+ real tools (2 read-only/computational + 1 approval-gated side-effecting, ≥2 hitting real services/DBs/sandbox APIs); Human approval/rejection interface enforced before risky actions; Deterministic termination (step/time limits + cancellation), retry/backoff, idempotency; External channel integration (WhatsApp, Telegram, or Slack) with verified inbound webhooks and persisted, UI-visible message history; Run inspection with correlated traces, tool calls, and token/cost metrics; Reproducible evaluation runner (≥20 versioned split cases, ≥3 deterministic + ≥1 rubric evaluator); Working end-to-end demo with 2+ agents executing a real task.

PERFORMANCE BENCHMARKS

Agent CRUD (name, role, system prompt, model, tool allowlist, operating limits, channels); Agent configuration (schedules, run-scoped + conversational memory with retention/expiration policy, guardrails, interaction rules); Workflow builder to create/edit agents and multi-agent workflows with conditions/branching; ≥1 manual trigger plus ≥1 recurring or delayed schedule; ≥2 cooperating reasoning components with shared typed/validated run state; 3+ real tools (2 read-only/computational + 1 approval-gated side-effecting, ≥2 hitting real services/DBs/sandbox APIs); Human approval/rejection interface enforced before risky actions; Deterministic termination (step/time limits + cancellation), retry/backoff, idempotency; External channel integration (WhatsApp, Telegram, or Slack) with verified inbound webhooks and persisted, UI-visible message history; Run inspection with correlated traces, tool calls, and token/cost metrics; Reproducible evaluation runner (≥20 versioned split cases, ≥3 deterministic + ≥1 rubric evaluator); Working end-to-end demo with 2+ agents executing a real task.

CODE QUALITY EXPECTATIONS

Agent CRUD (name, role, system prompt, model, tool allowlist, operating limits, channels); Agent configuration (schedules, run-scoped + conversational memory with retention/expiration policy, guardrails, interaction rules); Workflow builder to create/edit agents and multi-agent workflows with conditions/branching; ≥1 manual trigger plus ≥1 recurring or delayed schedule; ≥2 cooperating reasoning components with shared typed/validated run state; 3+ real tools (2 read-only/computational + 1 approval-gated side-effecting, ≥2 hitting real services/DBs/sandbox APIs); Human approval/rejection interface enforced before risky actions; Deterministic termination (step/time limits + cancellation), retry/backoff, idempotency; External channel integration (WhatsApp, Telegram, or Slack) with verified inbound webhooks and persisted, UI-visible message history; Run inspection with correlated traces, tool calls, and token/cost metrics; Reproducible evaluation runner (≥20 versioned split cases, ≥3 deterministic + ≥1 rubric evaluator); Working end-to-end demo with 2+ agents executing a real task.

TIME CONSTRAINTS

2 weeks

TECHNICAL CONTACT

ben.lee@y.uno

TECHNOLOGY

REQUIRED LANGUAGES

Candidate's choice — must justify the decision in the README/ADR. A production-capable backend language with a documented, standard build/dependency toolchain is required (Kotlin or Go suggested, not mandated).

AI / ML FRAMEWORKS

Optional — frameworks are a candidate decision; evaluate cost vs. benefit and justify in the ADR. Any model provider and orchestration approach is allowed, but the primary workflow must use real tools and models (no framework example-reproductions, no mocked calls). At least two cooperating reasoning/decision-making components are required.

DEV TOOLS

Candidate's choice for frontend and backend stack. Must include: standard build/dependency management, unit + integration tests, an HTTP API with a machine-readable contract (OpenAPI or equivalent), persistent state for configs and run history, and a one-command test/eval path (make test, make run, make eval).

CLOUD PLATFORMS

Optional — the project must run fully local with a single setup command (containerized or one-command local dependencies), reviewable without access to the candidate's private environment. A deterministic local fallback is mandatory; no cloud dependency may be required to evaluate.

OTHER REQUIREMENTS

Agents must communicate asynchronously via shared, typed/validated run state. Message and run history must be persisted and visible in the UI (status, state transitions, tool calls, metrics). At least one agent must be connected to WhatsApp, Telegram, or Slack, with inbound source verification via the channel's signing/token/webhook mechanism. The chosen runtime must actually execute agent logic — not a UI mockup. Provide 3+ tools (2 read-only/computational + 1 approval-gated side-effecting; ≥2 hitting real local services/DBs/sandbox APIs), each with explicit schemas, timeouts, error handling, and untrusted-content handling. Enforce deterministic termination (step/time limits + cancellation), retry/backoff on tool/model failure, idempotency on side effects, and a human approval gate before risky actions. Treat retrieved content as untrusted (prompt-injection safe); never store/expose chain-of-thought; commit no secrets.

OFF-LIMITS TECH

Nothing

SUBMISSION & AI POLICY

AI USAGE DOCUMENTATION

Required

REQUIRED DELIVERABLES

Source Code
Technical Documentation
Demo Video
AI Usage Log

ATTACHMENTS

AI-Agent-Orchestration-Platform.pdf

LINKS

No links available.

Submit this project

Submit directly from this page. Your submission is tied to this project version.

Open Submission Interface 

Tab Context:
- Executed on tabId: 1889834613
- Available tabs:
  • tabId 1889834613: "Gauntlet Portal" ("https://portal.gauntletai.com/projects/2GaNjUiJQp6JkxmKdDAN")