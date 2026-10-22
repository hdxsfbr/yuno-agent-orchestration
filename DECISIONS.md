# Assumptions and decisions log

Newest first. Each entry says what was decided, by whom, and why. Design rules the entries
refer to are in `docs/DESIGN.md`. Architecture-level choices also have an ADR under
`docs/adr/`.

## 2026-10-21

**D-087 A run's checkout has the default branch's history, and a run on a merge request
starts with the default branch merged in.** (Andre, 2026-10-21: "Runs updating an MR cannot
see the target branch, so agents cannot resolve conflicts.") Seen live: a Builder asked to
resolve the conflicts on !9 had a shallow clone of the request's branch with one commit, and
`git fetch origin main` failed for want of credentials; a Scout could not read history
either. The first step now fetches the run's branch and the default branch into the run
repository with the token, 100 commits deep and deeper until the two share history (at most
2000), and every checkout is built from there with `origin/<default branch>` and the
history, and no credential. For a run on a merge request the platform merges
`origin/<default branch>` into the request's branch in that fresh checkout before the first
turn: a clean merge is the checkout's start; a conflict leaves the markers, and the task
names the files to resolve. The run records the merged commit, and its push makes it the
second parent of the run's commit, so the request's branch gets a real merge the provider
sees as resolving the conflict (a plain tree commit could still conflict); the push refuses
while a changed file holds a marker, and refuses `paths` while the merge is pending.
Alternatives rejected: a read-only credential helper in the sandbox (the token must stay on
the platform side, D-037); the agent running the merge itself (it could not fetch, and the
platform would not know the merge's parent). Design: DESIGN 5.22 rule 13, 5.30 rule 10.

**D-086 A merge request carries only the run's changes, counted from the commit the run
cloned, merged per path onto the branch as it is.** (Andre, 2026-10-21: "Whole-tree commits
from stale checkouts silently delete other tickets' work.") Seen live: ticket T2 was merged
as !5, and a day later main had none of T2's files. A later ticket's run had cloned main
before T2 landed, and `open_merge_request` cloned main again, replaced its whole tree with
the checkout's and committed the difference, which deleted T2's files. The proposal now
takes the run's base, `runs.checkout_sha` (the commit the first step cloned, at the step's
start), and the run's changes are what differs between it and the checkout's working tree.
When the branch is still at the base they are one commit on top, as before; when it moved,
each path the run changed takes the run's version if the branch did not change it since,
needs nothing if the branch has the same content, and is a conflict otherwise. A conflict
pushes nothing and tells the agent which files conflict with which branch, so it can ask
the operator. Without one, the pushed head merges the branch's head with the run's own
commit (the base plus the run's changes), and that commit becomes the run's
`checkout_sha`, so a later push of the same run counts from it and does not conflict with
itself. A file the run never touched is never deleted, and a guard refuses a commit that
would delete or change one anyway, naming the paths. Links and tracked files under the
excluded directories, which the working-tree copy cannot see, keep the base's version. A
run that names a request keeps D-050's guard: a commit on that branch it did not make is a
person working on the same request, and the operator decides. Alternatives rejected: a
rebase of the run's commit (rewrites what a reviewer may have seen, and conflicts would
need resolving on the api side); line-level merges (a file both changed would hold
content neither side tested). Design: DESIGN 5.16 rule 10, 5.30 rule 9.

## 2026-10-20

**D-085 A tool call that waits for approval parks the step until the approval is decided.**
(Andre, 2026-10-20) Seen live: when nobody decided within `MCP_TOOL_WAIT`, the gate answered
`pending_approval` and the Integrator's draft step finished with `branch: "pending_approval"`
and no merge request URL; the pipeline went on to the manual-test approval with nothing to
test, and the merge request appeared two hours later, outside the run, when the operator
approved. The pending result now tells the agent to end its turn without finishing, and the
step waits for the approval with the question machinery of 5.24 (D-046): the visit parks, the
engine settles the decision as before (the effect under the same idempotency key, or the
rejection or expiry), and the step resumes with the outcome, as its next turn or in the
continuation's task: the call's result as JSON (the merge request's URL, number and branch),
`rejected` with the operator's reason, or `expired`. An expiry while the step waits no
longer fails the run; the agent decides. The gate, the keys, `auto_after` and a decision
within the wait are unchanged, and the Integrator's prompt and the pipelines' task text say to
end the turn on `pending_approval`. Alternative rejected: a longer wait in the MCP call
(OpenClaw's MCP request timeout of 600 s caps it, and an approval can take hours). DESIGN 5.3
rule 5.

**D-084 Escalations ask the operator instead of ending the run.** (Andre, 2026-10-20:
"those types of errors should be raised to HITL and not just crap out", and "we already
have something like that ... dont reinvent the wheel") A design_wrong escalation ended a
ticket run after 1h22m of work (T4) although the agent only needed a decision. Every
factory agent that can escalate now has `ask_operator` and one shared rule: ask with
concrete options and "Stop this ticket", carry out the answer, escalate only on "stop".
The Doc Auditor gets a `rework` verdict that sends the operator's decision back to the
Builder. No engine change: `ask_operator` (5.24) already parks the step and relays the
question through the Chief. The review-loop limit still ends a run; asking there too is
a separate change. Amends D-076 (a PRD gap pauses the waves) for the agent's own calls.

**D-083 Each run step works in its own private workspace; work moves through a per-run
git repository.** (Andre, 2026-10-20: "probe workspaceAccess none ... and if it holds,
build per-step private workspaces with hand-off through the run's branch") D-082 gave each
step a fresh container but still mounted the agent's whole workspace, so a step saw every
other active run of that agent. Agent factories elsewhere give each task its own sandbox
filesystem and hand work on through git. OpenClaw's `workspaceAccess: none` gives each
session a private workspace; the probe showed its path is computable, files placed there
before the first turn survive, and nothing of the agent's workspace is visible. The
checkout moves between steps as commits in a platform-side bare repository per run
(`<state>/yuno-runs/<run id>.git`), not as branches on the project's remote: no
unreviewed work-in-progress branches on GitLab and no token use per step. DESIGN 5.30.

**D-082 A fresh sandbox per session, and run folders deleted when no retry can need them.**
(Andre 2026-10-20) `agents.defaults.sandbox.scope` is `session` (it was `agent`): each run
visit has its own session key, so it gets a new container and nothing installed in one
reaches another run; `workspaceAccess` stays `rw`, and idle session containers are pruned
by `sandbox.prune` (`idleHours: 1`, `maxAgeDays: 1`, the keys of OpenClaw's sandbox
configuration reference). The Chief's Telegram session stays one long session in its own
container. The sandbox image gains `libgomp1` (the OpenMP runtime numpy, scikit-learn,
lightgbm and xgboost wheels load), in both the base and the python image. `runs/<run id>/`
is deleted from every agent workspace when a run ends `succeeded`, after its artifacts and
diff are collected; a `failed`, `timed_out` or `cancelled` run keeps its folders for 7 days
because Retry resumes on the earlier nodes' folders (the Builder's checkout), and an
hourly sweep (also at start) removes the expired ones, those of finished runs a hook
missed, and folders of unknown run ids older than 7 days. Alternative rejected:
keeping every folder until a size or age cap.

## 2026-10-19

**D-081 Approvals and project setup from Telegram through the Chief.** (Andre 2026-10-19)
The Chief of Staff gets `list_approvals`, `decide_approval` and `add_project`, and its prompt
routes requests to the delivery workflows (Plan pipeline, `revise_plan`, the Scout, the
Architect, the Builder; never the Ship pipeline by hand). `decide_approval` calls the
engine's `DecideApproval`, the web UI's path, with `decided_by chat:telegram`; it answers
only a chat token whose conversation is the calling agent's own on Telegram. `add_project`
runs the API's project creation; its input has no token member, and the server uses the
token of a project on the same host and provider in the same repository namespace (owner or
group path), else `PROJECT_GIT_TOKEN`, else tells the person to add the project once in the
web UI. Chat tools cannot be approval-gated (an approval pauses a run and a conversation
has none), so all three are computational or read-only, and the operator asking in their
own bound chat is the approval. Because model-written run text reaches the same session as
`[platform]` turns, a decision is bound to the person: `list_approvals` shows a 4-character
code per approval (HMAC of the id under a server secret), and `decide_approval` takes
`{approval_id, code, decision, reason}` and accepts it only when the code matches and
appears in the newest inbound message of the bound conversation that is not a `[platform]`
injection or the Chief's own. The injected payloads are also wrapped as untrusted content.
Consequence: anyone holding that chat can approve (threat model). Alternative rejected:
letting the model receive tokens in chat.

**D-080 Shipped workflow copies follow their template until the operator edits them.**
(Coordinator) The seed used to instantiate each shipped template once and never touch the
copy again, so a template that gained nodes in a later release never reached a running
stack. `workflows.seeded_hash` now holds the sha256 of the definition the seed last wrote;
every seed run applies the template (`ResetFromTemplate`, which bumps the version; runs pin
versions) to each copy whose definition still hashes to it, or that has no hash and is at
version 1 (the rule `retire.go` uses), unless the resolved definition already equals the
template's. An edited copy is kept and logged; `seed-reset` still restores it.
Alternative rejected: always resetting, which would overwrite operator edits.
Agents too: `agents.seeded_hash` (migration 0021) holds the sha256 of the role, prompt and
allowlist the seed last wrote; every seed run upgrades a shipped agent (the eight factory
agents, the Chief of Staff included, binding kept) whose current values hash to it, or that
has no hash, and clears its `materialized_hash`; an edited one is kept and logged. A NULL
hash counts as untouched because nothing distinguishes an edit (`agent_file_edits` holds
workspace files, not these fields) and the only operator, Andre, never edited them.

**D-079 Wave evals run against a fake forge.** (Coordinator, within D-071 to D-078's
plan, WAVES T3.5) Eval 028 needs issues, labels, merge requests, merges and a default
branch pipeline; a real GitLab project would make the suite slow, flaky and dependent on a
token. `forgefake` implements the GitLab subset in memory with real Git pushes through
`git http-backend`, only under the compose profile `eval` (DESIGN 5.29 rule 17).
Alternative rejected: mocking at the provider interface inside the api, which would skip
the HTTP and Git paths the evals exist to exercise.

**D-071 to D-078 The delivery process (the HDX Factory diagram).** (Andre, 2026-10-19:
"take the recommended answer to D1 to D8") D-071: the old templates (`dev_pipeline`,
`research_pipeline`, `review_panel`) and their nine agents are retired (kept under
`templates/legacy/`, not loaded); the factory's eight agents replace them and the Chief
of Staff stays bound to Telegram. D-072: the submitted state is the tag
`submitted-2026-10-19` (`fcd1eb6`) and work continues on `main`. D-073: a ticket's draft MR
call is approval-gated; the call that marks it ready runs under the manual-test approval
already given. D-074: a wave is shippable when every ticket's MR is merged and the
default branch's pipeline passed on a commit at or after the last merge. D-075: lessons
go to project notes and to the repository's `CLAUDE.md` through a small MR. D-076: a PRD
gap pauses the waves and the operator revises the plan (no automatic re-plan). D-077:
tickets are created with `wave:<n>`; releasing a wave adds the project's intake label.
D-078: eight factory agents (Analyst, Scout, Architect, Planner, Builder, Code Reviewer,
Doc Auditor, Integrator), Builder on sonnet and Code Reviewer on opus. Design: DESIGN
5.28 and 5.29; plan: `docs/briefs/delivery-process/`.

**D-070 Multi-file skills arrive by zip upload or from a public Git URL.** (Andre,
2026-10-19) Zip or folder upload, plus import from a Git repository path on an allowlisted
host (`SKILL_IMPORT_HOSTS`, default github.com and gitlab.com), shallow and capped; the api
never executes imported content, and scripts run only through `exec` in the sandbox.
Alternative: zip upload only (less surface). Design: DESIGN 5.27 rules 5 to 9.

**D-069 The skills library ships Anthropic's Apache-2.0 skills, OpenClaw's bundled MIT
skills and a few ClawHub skills.** (Andre, 2026-10-19) Vendored under `skills-library/`
with their licences, each read in full before it is committed; imported at start without
overwriting an existing skill of the same name, assigned to no agent. Anthropic's docx,
pdf, pptx and xlsx skills are excluded (their licence does not allow redistribution).
Alternative: write our own. Design: DESIGN 5.27 rule 10.

**D-068 Skills become bundles.** (coordinator, 2026-10-19, following D-070) A skill keeps
its `SKILL.md` body and gains files with paths and modes in `skill_files`, capped at 200
files, 2 MiB per file and 20 MiB per skill. Why: scripts and resources are what make
existing skills useful. Design: DESIGN 5.27 rules 5 and 8.

**D-067 Every agent workspace file is visible; generated files take an operator
addition, runtime files an override.** (Andre, 2026-10-19) SOUL.md, AGENTS.md and
MEMORY.md keep the platform's generated part with an "Operator additions" section below
it; IDENTITY.md, USER.md, BOOTSTRAP.md, HEARTBEAT.md, TOOLS.md and DREAMS.md are fully
editable. Edits are stored in Postgres and written on every materialize. Alternatives:
full override (can delete the rules the engine depends on), edit on disk only (lost on
the next sync). Design: DESIGN 5.27 rules 1 to 4.

**D-066 The platform restarts the gateway when a config patch needs a recovery restart,
and gateway calls wait out a restart.** (coordinator, 2026-10-19, from the final audit)
The pinned gateway answers config.patch with "persisted … but a recovery restart is
required" after agents are deleted and created in quick succession, and then refuses
every later patch; nothing restarted it, so agent saves failed and `make eval` fell to
6 of 17. The client now requests `gateway.restart.request` once (serialised), confirms
the patch with `config.get`, and retries any call refused as restarting or cut by a
disconnect for up to 60 s (read-only calls are resent after a disconnect; writes are
not). Why: the patch had persisted, so confirming it is correct, and a restart in place
takes seconds. Alternative: pin a newer gateway (unknown behaviour, no time to probe it).

**D-065 Final audit dispositions.** (Andre, 2026-10-19: "do it") Fixed: the gateway
restart above, MEMORY.md re-rendered after every materialize, memory tools offered when
memory is on, `run_status` and provider error bodies wrapped as untrusted, the agent
`channels` field read-only, eval reports separating expected outcome from succeeded,
and the README, ADR and threat model rewritten against the code. Disclosed rather than
fixed (`docs/prd-compliance.md` deviations 35 to 39): the hand-off between agents goes
through shared state in one worker; an expired conversation stays in the gateway's own
session; a retry or resumed run can repeat a `deploy_release`; the API is open without
`API_TOKEN`; the Chief looks runs up by id only. Why: each is a design change of a day or
more, and none is a requirement either brief states as a number.

## 2026-10-18

**D-064 The review panel ships with three new seeded reviewer agents and the Reviewer
as lead.** (Andre, 2026-10-18, the recommended answer of DESIGN 5.26's third open
line) `make seed` creates Security Reviewer, Performance Reviewer and Maintainability
Reviewer (Sonnet, read-only tools); the `review_panel` template runs them in parallel on
the Coder's change and the existing Reviewer merges their verdicts before the gate and
the Deployer. Why: three focused reviews are what a panel is for, and D-062 needs
distinct agents in the branches. Alternative: a two-reviewer panel with Reviewer and
Critic and no new agents. Design: DESIGN 5.26 rule 14.

**D-063 A branch's wait does not stop the run's clock, and approval nodes cannot be
inside a branch.** (Andre, 2026-10-18, the recommended answer of DESIGN 5.26's second
open line) An approval or question inside a branch pauses that node only; the run stays
`running` and the time limit keeps counting while the siblings work; the human gate
goes after the join. Why: the run is working while one branch waits, so excluding the
wait would be wrong, and a gate that releases the worker cannot sit inside a region
whose siblings still need it. Alternative: stop the clock while every live branch waits
(a paused-visit count on the run row). Design: DESIGN 5.26 rules 2 and 7.

**D-062 One agent, one branch: a fork whose branches share an agent is refused at
save.** (Andre, 2026-10-18, the recommended answer of DESIGN 5.26's first open line)
Every agent has one OpenClaw workspace, so two visits of it at once would write the same
`runs/<run id>/` and the same checkout; the validation message names both branches. The
same agent before the fork and inside one branch is fine. Alternative: a workspace per
visit (5.14 rule 5, 5.6 rule 13), out of this wave's size. Design: DESIGN 5.26 rule 2.

**D-061 Past a join, the checkout flows only from the branch the join names, and the
generated diff comes only from that branch.** (Claude, coordinator, 2026-10-18) The
copy-forward of 5.6 rule 13 leaves `runs/<run id>/repo/` out of every branch visit's
contribution unless its branch is the join's `checkout`; a join without one passes the
pre-fork checkout; `diff.patch` is generated for the named branch's visits only. Why: a
file-by-file merge of two working trees is a corrupted repository, and one `diff.patch`
row that flips between branches tells the operator nothing. Alternative: a real merge
of branch checkouts, out of scope (D-048). Design: DESIGN 5.26 rules 10 and 11.

**D-060 A fork runs its branches as concurrent walks inside one worker, recorded as
`run_branches` rows, and is the run's re-entry point until its join passes.** (Claude,
coordinator, 2026-10-18) The run's `current_node` stays on the fork while branches run;
each branch keeps its own position on its row and writes the state key-wise; the first
branch to fail stops the siblings and fails the run (D-048); a branch parked on a
question leaves the run running until every branch has ended or parked, then the fork
releases the worker and the decision re-queues the run through the fork's visit; a
claim, a retry or a re-queue re-enters the fork and runs only the branches that are not
done; at most 5 branches per fork. Why: no second queue or worker pool, crash recovery
and retry fall out of the existing resumed path, and the operator sees one row per
branch. Cost: a branch's approval wait does not stop the run's clock (D-063). Design:
DESIGN 5.26 rules 3 to 9.

**D-059 A declined question continues the step; the run does not fail.** (Andre,
2026-10-18, the recommended answer of DESIGN 5.24's second open line) When the operator
declines to answer, the agent reads `The operator declined to answer<: <reason>>.
Proceed without the answer and state what you assumed.` in the tool result or in the
continuation visit's task, and goes on. Why: a factory should not stop on a "no"; the
agent can decide and say what it assumed, and the operator sees the assumption in the
result. Alternative: failing the run with `question declined` (an operator who wants
that cancels the run). Design: DESIGN 5.24 rules 4 and 7.

**D-058 A question from a run without a conversation goes to the Telegram-bound agent's
most recent conversation, like a budget alert.** (Andre, 2026-10-18, the recommended
answer of DESIGN 5.24's first open line) A run started from the UI, a schedule or the
intake has no conversation to report to; its question is announced through the route of
5.19 rule 6 (`SendPlatformMessage`, idempotency key `report-<run id>-question-<id>`) so the operator
hears it where they are; without a binding or a conversation the inbox and the run page
are the surfaces. Why: a question is time-critical and the route exists; the Chief
relays the answer with `answer_question`. Alternative: the inbox only. Design: DESIGN
5.24 rule 6.

**D-057 Run artifacts are one row per run and path, replaced when the file changes; the
checkout diff comes from a private clone at the end of every agent visit; the store is a
named volume on the api.** (coordinator, 2026-10-18, writing DESIGN 5.25) `run_artifacts`
is unique on `(run_id, path)`: a file collected again with the same sha256 changes
nothing, a changed file updates the row in place (same id) and replaces the stored file,
so the run page shows the newest version of each deliverable and nothing accumulates.
`diff.patch` is produced after every agent visit of a project run by cloning the run's
branch into a private directory, fetching `checkout_sha`, copying the checkout's working
tree over it and running `git diff --cached` there, never inside the agent's checkout
(5.16 rule 10's reason: hooks or config an agent wrote would run on the api side). The
files live under `ARTIFACT_DIR` on the named volume `artifacts`, the first volume the api
mounts. Why: versions were out of scope (D-047) and a replace-in-place keeps ids stable
for the UI; the diff must not trust the checkout's `.git`; a volume survives the api's
recreation and is bounded by the per-run caps (200 files, 50 MiB) times the retention.
Alternatives: one row per visit and file (versions, more rows); the diff only at the
run's end (a failed later step would leave none, against D-047); the diff from the
checkout's own `.git` (unsafe). Design: DESIGN 5.25 rules 2 to 4.

**D-056 A question is an approval of kind `question`: one pending per visit, three per
step; a parked step continues as a new visit that does not count toward the loop limit;
a decision that arrives between the wait's end and the turn's end becomes the next
turn's message.** (coordinator, 2026-10-18, writing DESIGN 5.24) The question reuses the
approvals table, inbox, sweeper, expiry and announcement path (`options` and `answer`
are two new columns), with `POST /approvals/{id}/answer` as its decision. The engine
delivers a decision to the waiting call, else to the live visit, which sends it as its
next turn and keeps the agent's context, else re-queues the parked run, whose
continuation visit carries every decided question of the step in its task and is left
out of `max_loop_visits`. Why: a second table and inbox would duplicate 5.3 for one more
kind; a live visit that still holds its session is the cheapest place to deliver an
answer; a continuation visit that counted against the loop limit would turn the Dev
pipeline's three review rounds into two after one question. Alternatives: a
`questions` table (duplicates the inbox); resuming the parked visit's own OpenClaw
session (out of scope; D-046's new visit stands); failing the run when the wait ends
(D-046 rejected it). Design: DESIGN 5.24 rules 2, 3, 5 and 10.

**D-055 A project needs a daily or monthly budget cap before its issue intake can be
enabled.** (Andre, 2026-10-18, the recommended answer of DESIGN 5.23's open line)
`PUT /api/projects/{id}` with `intake.enabled: true` on a project without a cap is 422
`intake.enabled: set a daily or monthly budget before enabling intake`; the form says
the same sentence. Why: whoever may set labels on the repository can start runs on the
project, each spending up to the workflow's `max_cost_usd` and opening a merge request
behind the approval gate, and the project cap (5.19) is the only bound on that; a
warning sentence alone would let an intake run uncapped. Cost: one more step before
the first labelled issue is taken. Design: DESIGN 5.23 rule 1.

**D-054 The merged or closed report of a merge request goes once to the conversation of
the run that opened it, not to the runs that updated it.** (Andre, 2026-10-18, the
recommended answer of DESIGN 5.22's open line) 5.20 rule 4 is unchanged in this wave:
one `[platform]` line, keyed `report-<run id>-mr-<state>`, into the opening run's
conversation; a run that updated the request (5.22) gets no copy, and the Chief answers
`run_status` when asked. Why: with one operator the two conversations are the same in
practice, and a second destination would need a second key and a second claim per
updating run. Cost: a request opened from the UI and updated from Telegram reports its
merge nowhere on Telegram; revisit when a second operator exists. Design: DESIGN 5.22
rule 9.

**D-053 Issue intake creates the derived labels itself, swaps the label before it
comments, takes at most ten issues per project per poll, wraps the issue as untrusted
content, and adds `Closes #<n>` once.** (coordinator, 2026-10-18, writing DESIGN 5.23
for Andre's approval) The three derived labels (`<label>:running`, `:done`, `:failed`)
are created on the repository when missing, once per project per process start, so a
swap never names a label the provider lacks; the label is the state and goes first, the
comment is best effort and may repeat once after a crash; a poll creates at most 10 runs
per project (the rest wait two minutes); the issue's title and body reach the agents as
the workflow input's `task` inside one `<untrusted_content source="issue #<n>">` block
with closing tags escaped and the body cut at 16 KiB; `open_merge_request` appends
`Closes #<n>` to the description when it creates the request of an issue run, never on
an update. The intake workflow must opt in to a project and accept `{"task": string}`,
checked when the settings are saved; a workflow that stops validating later puts the
trigger's message in `intake_last_error` and takes nothing until fixed. Why: the
provider's own label mechanics differ (GitHub refuses unknown labels on some paths),
and the issue is a request anyone with label rights can write. Cost: label colours and
names are the platform's; a repository that already uses `yuno:*` labels for something
else should pick another intake label. Design: DESIGN 5.23 rules 1 to 5 and 7.

**D-052 The run row is the issue intake's record: `trigger = issue`, `trigger_ref =
<project id>#<n>`, `reported_at` means "posted on the issue"; one live run per issue is
a partial unique index.** (coordinator, 2026-10-18, writing DESIGN 5.23) No
`issue_intakes` table: the issue URL derives from the project's repository URL and the
number, the state lives in the labels on the provider, and the run's `reported_at`
(unused by the channel report for a non-channel run; its queries filter on `trigger =
channel`) records that the outcome was posted on the issue. `runs_issue_live_idx ON runs
(trigger_ref) WHERE trigger = 'issue' AND status IN (queued, running, waiting_approval)`
keeps one live run per issue, so a crash between the run's creation and the label swap
is recovered by the next poll finding the live run, and a retry (5.21) of an issue run
while another live run has the issue is refused. The roadmap's "unique `trigger_ref`"
becomes "unique while live", since re-labelling after `:failed` creates a new run with
the same ref (D-045). Why: one table fewer, and the mechanism is the one 5.22 uses for
merge requests. Cost: the issue's title is not stored apart from the task text, and the
Intake card shows the poll, not a list of issues. Design: DESIGN 5.23 rules 3, 5 and 6.

**D-051 `read_merge_request` reads any request of the project from the provider, in a run
or in a conversation, and answers one untrusted block.** (coordinator, 2026-10-18,
writing DESIGN 5.22) The tool takes a number (`12`, `!12`, `#12`) or the request's URL,
defaults to the run's own or named request in a run, needs `project` and `number` in a
conversation, and reads the provider rather than the record, so a request the platform
did not open can be read too; the description, the unresolved threads (path, line,
author, body) and the pipeline status come back as JSON inside one `<untrusted_content
source="read_merge_request">` block, cut to the output cap by dropping the newest threads
and counting them. It is chat-capable, so the seeded Chief of Staff carries it, and it
joins the Coder's and the Reviewer's allowlists. Why: D-044 wants the threads behind a
tool call; what people wrote on the provider is data to the model (5.7 rule 5), and the
Chief should be able to say what a request is about. Design: DESIGN 5.22 rule 6.

**D-050 A push onto a merge request's branch is refused when the branch moved since the
run cloned it; the platform never merges or rebases.** (coordinator, 2026-10-18, writing
DESIGN 5.22, closing wave 8 review finding L4 for these runs) Every project run records
the clone's `HEAD` as `runs.checkout_sha`, updated after each push the run makes;
`open_merge_request` on a run that names a request compares the branch head on the
provider with it and, when they differ, answers the model that the branch has new
commits and nothing was pushed, so a commit someone else pushed meanwhile is never
replaced by the checkout's tree. The operator starts a new run, which clones the branch
as it is. Why: a flattened replacement silently reverts other people's work; a merge or
a rebase on the api side would need conflict handling the platform does not have. Cost:
a long run on a busy branch can end without pushing; the same-run update path of 5.20
(a run pushing onto the request it opened) keeps L4 as a recorded limitation, since the
run did not clone that branch. Design: DESIGN 5.22 rules 2 and 4.

**D-049 A run names the merge request it updates by the record's id; one live run per
request; the request's own run must have ended; a schedule on a final request is
disabled.** (coordinator, 2026-10-18, writing DESIGN 5.22) `RunCreate`, `ScheduleInput`
and the trigger request carry `merge_request_id` (the store's id; the Chief's
`delegate_task` takes a number or a URL and resolves it against the project), checked at
queue time after the project check: the record must exist, belong to the run's project
and be open, and no live run may be updating it, including the run that opened it, with
one 422 text per case; `runs_merge_request_live_idx` (partial unique on
`merge_request_id` for queued, running and waiting runs) enforces the last one and makes
a retry while another run updates the request a 409. The updating run gets no record of
its own (`merge_requests.run_id` stays the opening run), and `Run.merge_request` is
filled from the named record. A schedule whose request turns merged or closed is disabled
at the tick with the state in `last_error` (no later occurrence can succeed), while a
transient "being updated" refusal advances like a rate limit. Why: two runs pushing onto
one branch would overwrite each other through the tree replacement, and the record's id
is what the UI and the API already hold. Cost: iterating on a request whose opening run
is still live waits until it ends. Design: DESIGN 5.22 rules 1, 7 and 8.

## 2026-10-17

**D-048 Parallel steps are explicit `fork` and `join` nodes; a failed branch fails the
run; a review panel template is the example.** (Andre, 2026-10-17, the recommended
answers of `docs/ROADMAP.md` 5.26) Fan-out happens only at a `fork` node that names its
`join`, so the first-true-edge rule of every other node (5.6 rule 4) stays; a branch
that fails ends the run and cancels its siblings in this version, and a new
`review_panel` template ships alongside the unchanged Dev pipeline. Why: local
validation of the branch region and no change to how existing graphs run. Cost: no
partial fan-in and no parallel review in the Dev pipeline until a later decision.
Design: DESIGN 5.26 when its wave starts.

**D-047 Run artifacts are collected at the end of every visit, the checkout diff is
one of them, and they live 30 days.** (Andre, 2026-10-17, the recommended answers of
`docs/ROADMAP.md` 5.25) Files under `runs/<run id>/out/` are collected after each
visit, so a later failure still leaves the earlier deliverables; a project run gets
`diff.patch` automatically; `ARTIFACT_TTL_DAYS` defaults to 30. Design: DESIGN 5.25
when its wave starts.

**D-046 A question to the operator blocks the tool call, then parks the visit and
resumes it with the answer; Telegram answers go through a Chief tool.** (Andre,
2026-10-17, the recommended answers of `docs/ROADMAP.md` 5.24) `ask_operator` waits
like a gated tool and, past the wait, the visit is parked and re-run with the answer
appended (the resumed path of 5.21); the operator answers in the inbox or by replying
to the Chief, which calls `answer_question`. Why: a factory cannot lose a question
after nine minutes, and no new channel command is needed. Design: DESIGN 5.24 when
its wave starts.

**D-045 Issue intake polls the provider, drives a label state machine on the issue,
and runs once per issue.** (Andre, 2026-10-17, the recommended answers of
`docs/ROADMAP.md` 5.23) A per-project poll lists open issues with the intake label,
no inbound endpoint and no webhook secret; the label moves `yuno` → `yuno:running` →
`yuno:done` or `yuno:failed`, and re-adding `yuno` is the explicit retry. Why: both
providers behave the same and the state is visible where the issue lives. Design:
DESIGN 5.23 when its wave starts.

**D-044 A run iterating on a merge request reads its threads through a tool, and a
request closed before the clone is refused at queue time only.** (Andre, 2026-10-17,
the recommended answers of `docs/ROADMAP.md` 5.22) `read_merge_request` returns the
unresolved threads when the agent asks, so the task stays short; a request that was
merged or closed between the request and the clone is refused when queued and
otherwise runs. Design: DESIGN 5.22 when its wave starts.

**D-043 A retry continues the same run with an attempt counter, from the node it
stopped at; cancelled runs can be retried.** (Andre, 2026-10-17) `POST
/api/runs/{id}/retry` re-queues a `failed`, `timed_out` or `cancelled` run from its
`current_node`, keeping state, counters and the checkout, with `runs.retries` and the
crash counter `attempt` reset per retry; limits apply to the whole run and are raised
on the retry when exhausted; the time limit restarts. Why: one run id keeps one trace,
one report-back thread and (with D-042) one merge request; a new run linked by
`retry_of` would open a second request on a second branch. Cost: a retried visit has
new idempotency keys, so `deploy_release` and `gitlab_create_mr` repeat on a retry of
their node, as on a loop revisit. Design: DESIGN 5.21.

**D-042 Merge requests are records synced from the provider; merging is an operator
action in the UI; the final state is reported into the conversation once.** (Andre,
2026-10-17) A `merge_requests` table is written by `open_merge_request` and refreshed
every two minutes (state, pipeline, unresolved threads); a run has one request, and a
later `open_merge_request` in the same run pushes onto its branch instead of opening a
second one; merge is `POST /api/merge-requests/{id}/merge` behind a confirmation, not
a Deployer tool in this wave; merged or closed is reported once into the run's
conversation. Why: the factory's loop closes where the work was asked for, and a chat
tool with `require_approval` is refused by 5.15 rule 7. Cost: polling, no webhooks;
GitHub thread counts need GraphQL access on the token. Design: DESIGN 5.20.

**D-041 Project budgets refuse new runs at the cap and alert the Chief's latest
conversation; running runs finish.** (Andre, 2026-10-17) Optional daily and monthly
caps per project in UTC calendar windows; a trigger on a project at or over a cap is
refused (429 `project_budget_reached`), running runs are never aborted by the cap, and
the warning (80 %) and reached alerts go to the newest conversation of the agent bound
to Telegram, like report-backs, or to a log line and the UI without a binding. Why:
refusal at queue time is deterministic and bounds the overshoot by the running runs'
own caps; the report-back route exists and needs no alert chat id. Design: DESIGN
5.19.

**D-040 Project instructions are the operator's text only; project notes are a shared
scope any agent of a run on the project may write.** (Andre, 2026-10-17) A project
carries an `instructions` text rendered into every visit's task on it; the checkout's
own `AGENTS.md` is not injected (repository text is untrusted; the agent reads it
itself); `memory_save` and `memory_recall` take `scope: "project"` in a run on a
project, stored in their own `project_facts` table because `memory_facts` is keyed per
agent, and any agent with memory enabled reads and writes them. As built: the shipped
Coder and Reviewer carry `memory_save` and `memory_recall` with memory on, so the Dev
pipeline saves and reads project notes out of the box. Design: DESIGN 5.18.


**D-039 A schedule names the project its runs work on.** (Andre, 2026-10-17)
`schedules.project_id` (migration 0011) is checked when the schedule is created or
updated with the rule a run request's `project_id` follows (D-038), through the
engine's own check: required for a workflow that opts in, refused for one that does
not, an unknown or deleted id refused, each a 422 detail on `project_id`. The tick
passes the id in the trigger request, so the engine checks it again and snapshots the
project onto the run like a manual request's. The schedule form shows the Run dialog's
project selector when the chosen workflow opts in and the Schedules list names the
project. Why: the seeded Dev pipeline opts in, so every schedule of it failed at its
first firing with `project_id: required` in `last_error`, the limitation D-038 left;
refusing the schedule when it is saved tells the operator at once rather than at the
first tick, and the project is part of what the schedule runs, not something each
occurrence rediscovers. Cost: a project deleted after the schedule was saved refuses
the next occurrence (`last_error` names it) and the schedule's next update until
another project is chosen; an update that moves a schedule to another workflow drops
its project unless it names one. Design: `docs/DESIGN.md` 5.9 rule 6.

**D-038 Projects are records created in the UI; a run names its project; tokens are
encrypted at rest; `PROJECT_*` only bootstraps.** (Andre, 2026-10-17) The single
environment-configured project of D-037 becomes a `projects` table an operator fills in
on a Projects tab: name, https URL, provider (inferred from the host when left empty),
default branch, sandbox network opt-in, description, and a git token stored AES-GCM
encrypted under `PROJECT_TOKEN_KEY` (generated into `.env` like the other platform
tokens) and never returned by the API (`token_set` says one is stored). A run request,
the Run dialog, the quick-run dialog and the Chief of Staff's `delegate_task` name the
project; a workflow that opts in (`project: true`) requires one and one that does not
refuses one; the run keeps a snapshot of the settings and reads the token from the row
at clone and push time, so a deleted project finishes its runs and a replaced token
applies to runs in flight. `open_merge_request` is registered always and takes the
project from the run. A Check action lists the repository through the provider's API
and reports the default branch and whether the token can push, without a clone. At
start the api turns a configured `PROJECT_*` into a project once, named after the
repository path, so the current deployment keeps working; after that the page is the
truth. Why: the operator wants several repositories and wants to say "on the playground
project" to the Chief, and a token in `.env` cannot be rotated or added without a
redeploy; records with a write-only, encrypted token keep the credential on the api
side (D-037's rule) while making projects a product object. Cost: one more secret in
`.env` (`PROJECT_TOKEN_KEY`), whose loss makes every stored token unreadable (the check
says so and the operator enters the tokens again); schedules do not name a project yet,
so a scheduled run of a project workflow fails its validation until they do (D-039
adds it). Design: `docs/DESIGN.md` 5.17.

**D-037 Projects are git repositories configured by environment variables; toolchains
come from the repository and run in prebuilt generic images; sandbox network is a
per-project opt-in; no agent clones or pushes.** (Andre, 2026-10-17) One project per
platform, set by `PROJECT_REPO_URL`, `PROJECT_GIT_TOKEN`, `PROJECT_DEFAULT_BRANCH`,
`PROJECT_PROVIDER` and `PROJECT_SANDBOX_NETWORK`; a workflow or quick run opts in with
`project: true` (the seeded Dev pipeline does). The engine clones the default branch at
depth 1 into the first agent's run directory before its turn and the copy-forward hands
the checkout on; the toolchain is read from the manifest (go.mod, package.json plus a
lockfile, pyproject.toml, requirements*.txt, uv.lock) and selects one of three shipped
images (`yuno-sandbox-{python,node,go}`) with nothing project-specific inside; the
materialiser sets `agents.entries.<id>.sandbox` for the visit's duration with scope
`session`, because the gateway keeps a hot agent-scoped container on its old image.
Sandbox network stays off unless `PROJECT_SANDBOX_NETWORK=true`, which exists so agents
can install the project's dependencies themselves; the no-network rule of D-012 was a
design choice, not a brief requirement, and it is now a per-project choice recorded in
the threat model. The token stays on the api side: git receives it as an
`http.extraheader` through the environment, the checkout's config holds the bare URL,
and the Deployer opens the merge request through the platform tool `open_merge_request`
(GitLab, then GitHub behind the same interface), which commits and pushes from the api.
Why: a demo that writes loose files into a run directory shows the mechanism, not the
job; a run that starts from a real repository with its own toolchain and ends in a merge
request is what the pipeline is for, and keeping every credential and every push on the
platform side keeps the model out of the trust boundary. Cost: one more container per
node visit of a project run (session scope), three more images to build, and with
network on the sandbox can exfiltrate, so the opt-in is for trusted repositories only.
Design: `docs/DESIGN.md` 5.16.

## 2026-10-16

**D-036 One coordinator agent on Telegram; chat-time tools identified by a chat token.**
(Andre, 2026-10-16) The person talks to one agent, the Chief of Staff, which delegates
to the others as platform runs and relays their outcomes; it is seeded, bound when
nothing else is, and talks through OpenClaw's native chat. Because the gateway's MCP
client carries no identity, chat tool calls carry a per-agent `chat_token` the way run
calls carry `run_token`; only six tools accept it. Delegation needs no approval by
default (the delegated run's side effects keep theirs); approvals are decided in the
UI and the chat is only told; both templates are instantiated at seed so workflows can
be named. Why: the demo had six peers and no front door; a coordinator that lives
inside the platform's runs keeps every delegated task traced, limited and gated, which
OpenClaw's own sub-agent spawning would bypass. Design: `docs/DESIGN.md` 5.15.

**D-035 Public deployment is a plain Docker host behind Caddy; CI deploys through
runners we register.** (Andre, 2026-10-16) The stack needs the Docker socket, a network
namespace shared between api and gateway, a bind mount both write and 4 GB for the
gateway, so Railway, App Platform and the other managed-container products cannot run it;
a DigitalOcean droplet runs the same compose file with a `public` profile that adds Caddy
for TLS, with every other port bound to loopback because Docker publishes past ufw.
`.gitlab-ci.yml` tests on a `yuno-ci` shell runner and deploys from a `yuno-prod` shell
runner on the droplet, manual on `main` by default and automatic with `DEPLOY_AUTO=true`;
pipelines exist only while `YUNO_CI=true`, so the repository shows no stuck pipelines
before a runner is registered. Why: the approved vendor list offers DigitalOcean, Vultr and
Railway for hosting, and only a VM fits; a shell runner is the only executor that can
bind-mount the checkout into the tool containers. See `docs/deploy.md`.

**D-034 Runtime reconciliation details.** (Claude, from the fix pass) An agent deleted
while one of its node visits is running stays in the runtime until the next full sync,
so the run finishes on its snapshot (DESIGN 5.6 edge case); the api re-materialises
out-of-date agents every 30 s besides the full sync at start and reconnect, so agents
seeded by `make run` are usable at once. The UI keeps the API token in the browser's
local storage and sends it only as a header; the sign-in dialog cannot be dismissed
while the API refuses the token; streams reconnect when the token changes. Why: the
simplest behaviours that keep the runtime's agents equal to the platform's and the
approval gate closed to the network. Cost: a deleted busy agent lingers for one run.

**D-033 Disclosed deviations from the audit, kept on purpose.** (Andre, 2026-10-16, on the
coordinator's recommendation) The runtime keeps its own session transcripts, thinking
included, inside its state directory; the platform never stores or shows reasoning and
that is the brief's "never store/expose chain-of-thought" as read here. Schedules target
workflows; a per-agent schedule is that agent's quick-run workflow. The deterministic
local fallback is the evaluation path (scripted model); UI runs need a model key. The
evaluation suite replays scripted runs and judges outcomes against expectations; the
real-model run is the demo. Guardrails and limits cover runs, not chat conversations
(the sender allowlist bounds those). The termination evaluator tolerates one extra step
and the cost limit may overshoot by one call. Idempotency keys include the node visit by
design, so a loop revisit is a new effect. Why: each is a deliberate reading of the
brief recorded in `docs/prd-compliance.md` with its reason; changing them now would
trade verified behaviour for unverified behaviour on the last day.

**D-032 The demo and the submission run Telegram in webhook mode through a public tunnel
that exposes only the webhook path.** (Andre) `make tunnel` starts a cloudflared quick
tunnel to a second nginx listener that serves `/telegram-webhook` and nothing else;
`PUBLIC_BASE_URL` and `TELEGRAM_MODE=webhook` go in `.env`, `openclaw-init` refuses
webhook mode without an https URL and a 32-character secret, the gateway registers the
URL with Telegram, and a wrong secret is answered 401 (smoke asserts it, and asserts the
registered URL). Polling stays supported for local use. Why: the brief says "verified
inbound webhooks"; polling verifies by token but shows no verification. Cost: a quick
tunnel's URL changes when it restarts, so `make tunnel` runs before `make run` on the
demo machine.

**D-031 The UI carries the API token.** (Andre) When `API_TOKEN` is set the UI asks for
it once (stored in the browser's local storage), sends it on every request and stream,
and signs out on request; the OpenAPI document declares the bearer scheme. Why: the
approval gate is only a gate if the API is not open to the network it listens on.
Cost: still a single shared token, no users or roles (known limitation).

**D-030 Deleting an agent deletes it in the runtime; the api reconciles at start.**
(Claude, from the audit) The gateway's heap crash came from 133 agents evals had left
behind. Deletion now removes the gateway entry and the workspace, and at api start
every stale gateway agent is deleted and every unmaterialised platform agent is pushed
(so `make seed` agents are usable at once). Why: the runtime's state must mirror the
platform's. Cost: a deleted agent's sandbox containers are removed by the gateway's
own prune, not immediately.

**D-029 Schedule firings carry an occurrence key; a fired once-schedule stays fired.**
(Claude, from the wave-3 review) Each scheduled run's `trigger_ref` is
`<schedule id>@<occurrence time>`, unique among schedule runs, so a crash between
creating the run and advancing the schedule cannot fire the occurrence twice. A delay
schedule's occurrence is its `run_at`: re-enabling one that already fired does nothing
until it gets a new `run_at`. Why: exactly-once per occurrence is the scheduler's whole
promise (DESIGN 5.9). Cost: "run it again" is a new time, not a toggle; the Schedules
page's edit form makes that one field.

**D-028 Exec guardrail patterns cover host exec and approvals, not the sandbox.**
(Claude, from agent B) OpenClaw's exec approvals apply to host (`elevated`) exec only;
sandboxed commands run without a gateway hook. Patterns therefore shape the exec policy,
auto-deny matching exec approvals, and block platform tools in the MCP gate, but a
matching command inside the sandbox runs. Why: the sandbox has no network and only the
agent's workspace, so the blast radius is the run directory; a `before_tool_call` plugin
hook is the fix if the demand appears. Cost: the guardrail is weaker than the design's
T5.11.4 reads; the README says so.

**D-027 Eval splits are 17 public and 7 private; the model grades only the rubric.**
(Claude, from agent D) `make eval` runs the public split offline; the private cases need
the network, a configured service or a human and run with `make eval-final`
(`EVAL_SPLIT=all`). `EVAL_PROVIDER=anthropic` changes who grades the rubric, never how
runs are driven (always the scripted model); a grader failure scores 0 with the reason,
no silent fallback. The schedule case uses an agentless workflow because a schedule
cannot carry the scripted model override. Why: `make eval` must stay offline and
deterministic; the rubric is the only place a real model adds judgement. Cost: the
channel-triggered reply is not an eval case (it needs a real Telegram message); the
conversation mirroring is covered by unit tests instead.

**D-026 Telegram stays closed until an agent is bound; binding is a config patch plus an
in-place gateway restart.** (Claude, from agent A) The baseline writes
`channels.telegram.dmPolicy: "disabled"`; `PUT /api/channels/telegram/binding` patches
`bindings[]` (dmScope per-channel-peer) and `dmPolicy: allowlist`, then asks
`gateway.restart.request`, which restarts the gateway in place (same container, about
five seconds, the api reconnects); unbinding closes the channel again. The
`channel_bindings` table is the truth and a keeper re-asserts it at every api start and
reconnect. Why: `enabled: false` would stop polling and Telegram would hand the held
messages to the next bound agent; `dmPolicy: disabled` drops them. Cost: a message sent
while unbound is dropped without a platform log line (OpenClaw drops it before logging);
the Channels page shows the channel as closed.

**D-025 The Telegram channel never answers from OpenClaw's default agent.** (Claude,
from the first live probe) With Telegram enabled and no binding, the gateway routed
Andre's first two messages to its built-in `main` agent, which replied on Opus. The
platform must make that impossible: a message is answered only by the platform agent
bound to the channel, otherwise dropped and logged. Why: an unbound channel talking to
an unmanaged agent on a paid model is both a cost and a trust problem. Cost: before the
first binding the bot stays silent. Mechanism chosen in wave 3 (DESIGN 5.8 rule 7).

## 2026-10-15

**D-024 Task templates render missing values as empty; earlier outputs arrive as a context
block.** (Claude, from agent A) `{{.state.reviewer.feedback}}` before the reviewer has run
renders as an empty string rather than `<no value>` or an error, so one template serves
the first and the looped visit; `input_from` appends `Context from earlier nodes:` with
each named node's output as compact JSON (`start` is the run input). Why: Go
`text/template`'s `missingkey=zero` does not cover decoded JSON maps (verified). Cost: a
typo in a state path is silent at save time; the save-time render still catches syntax.

**D-023 Template rows are read-only and hidden from the workflow list.** (Claude, from
agents B and C) The rows `templates/*.json` load into carry `template_key`; the list
endpoint leaves them out, PUT and DELETE on them answer 409, and the builder opens one
with a banner and a "Create editable copy" action (instantiate). Why: a template must
stay what the file says so `make run` on a fresh machine behaves the same. Cost: an
operator cannot tweak the shipped template in place; the copy is one click.

**D-022 Idempotency and gating details.** (Claude, from agents B and D) The idempotency
key excludes `run_token` from the canonical input (the key already carries the node
visit, and stored inputs have the token redacted); `finish` is always permitted and
always `auto`; a read-only tool set to `require_approval` is gated without a key; a
`blocked` platform tool is removed from the agent's OpenClaw tool list, so OpenClaw
refuses the call before it reaches the MCP gate and no `tool_calls` row exists for it
(the gate still refuses it when reached). Why: keys must be reproducible from the row;
finish is the protocol, not a side effect. Cost: a blocked attempt is visible only in
OpenClaw's own log, not in the trace.

**D-021 Workflow runs layer limits as request, workflow, defaults, clamp.** (Claude, from
agent A) Agent-level limits are not applied to workflow runs this wave; the min-of rule
across participating agents is the guardrails work of wave 3 (DESIGN 5.11 rule 1). Why:
one place to read a run's limits. Cost: an agent's own `max_cost_usd` is not yet a cap
inside a workflow.

**D-020 A run waits only while a tool call waits; tool approvals outlive a successful
run.** (Claude, from agent A) The run is `waiting_approval` only while an MCP call is
blocked on the decision; once the wait ends the run is `running` again with the approval
still pending, and a pending tool approval stays decidable after the run succeeded so the
late grant of D-014 can execute the effect. On a failed, cancelled or timed-out run every
pending approval expires. This supersedes the revision 1 test T5.1.2 ("terminal runs
reject approval") for kind `tool`. Why: D-014's block-then-fall-back needs it. Cost: an
inbox may show a pending approval for a run that already succeeded; the card shows the
run status.

**D-019 The run directory is copied forward between agents.** (Claude, from agent D's
finding) OpenClaw gives every agent its own workspace and its sandbox sees only that, so
the Reviewer could not read the Coder's files. Before each agent node visit the engine
copies `runs/<run_id>/` from the earlier visits' agent workspaces into the next agent's
workspace (later visit wins); the deploy tools read the calling agent's copy. Why: a
copy through the shared state directory needs no gateway configuration and keeps the
sandbox isolation; a shared bind mount would need a probe of how OpenClaw's file tools
resolve paths. Cost: large run directories are copied once per node visit. DESIGN 5.6
rule 13.

**D-018 Templates name agents; `make seed` creates them.** (Claude, unobjected) A template's
agent node carries `agent_name` ("Coder") instead of an id, and instantiating a template
resolves names to the agents of that name, failing with a path-qualified 422 when one is
missing. `yuno seed` creates the six demo agents once and never overwrites an edited one.
Why: templates are files in the repository and cannot know database ids; the alternative,
embedding agent definitions in templates, would duplicate the agent model. Cost: a
renamed agent breaks instantiation until the template is edited; the error names it.

**D-017 Side-effect approvals register with the gateway two-phase; the gateway's own
timeout is not a decision.** (Claude, from the probe) `plugin.approval.request` blocks
until a decision unless `twoPhase` is set, and its timeout (at most 10 minutes) ends in a
`deny` event with `resolvedBy: null`. The engine registers two-phase, treats that event
as no decision, and keeps the approval pending until the platform's own expiry, decidable
from the web UI. Why: approval timeouts are a product setting (default 24 h), not a
gateway constant. Cost: after the gateway's window only the platform's surfaces can
decide; chat-side decisions are verified with the Telegram channel in wave 3. Details in
`docs/DESIGN.md` 5.3 rule 5a; `make probe-approval` re-verifies.

**D-016 Model upstream is configurable; the demo runs through OpenRouter.** (Andre)
`MODEL_UPSTREAM=anthropic|openrouter`. OpenRouter serves the same Claude models through
the Anthropic messages format (verified 2026-10-15: `/api/v1/messages`, streaming and
tool use), so the proxy only switches the auth header to a bearer token and rewrites
model ids (`claude-opus-5-5` to `anthropic/claude-opus-5.5`); agents, the UI and the
price table are unchanged. Why: Andre's key is an OpenRouter key. Cost: OpenRouter's
usage block carries its own cost figure, which OpenClaw and the platform ignore in
favour of the price table, identical for these models.

**D-015 OpenClaw Tool Search is off for the platform gateway.** (Claude, from integration)
`tools.toolSearch: false` in the baseline and re-asserted at every api start. OpenClaw
otherwise hides MCP tools behind `tool_search` / `tool_call` meta-tools and a direct
`yuno__finish` call fails with "not found". Why: the finish protocol and the scripted
eval provider call platform tools by name; platform agents have small allowlists, so
there is no catalogue to compact. Cost: an agent given a very large tool set would get
a long prompt; none exists in this platform.

**D-014 Gated platform tools block, then fall back.** (Andre) An approval-gated MCP tool
call waits up to `MCP_TOOL_WAIT` for the human decision so the agent sees the outcome in
its turn; on timeout it returns `pending_approval` and the engine executes the effect
itself once approved, with the same idempotency key. Why: best demo experience with both
paths tested (DESIGN 5.3 rule 5).

**D-013 Telegram is OpenClaw's native channel. Supersedes D-002 and D-008 in part.**
(Andre) The agent is bound to OpenClaw's Telegram plugin (`channels.telegram` with
`webhookUrl` and `webhookSecret`, `bindings[]`), which verifies the secret token, keeps
the durable ingress queue and the conversation history, and handles exec approvals from
chat. The platform surfaces that history in its UI from the gateway and mirrors it to
Postgres. Why: Andre prefers the runtime's own multi-channel support over a parallel
adapter. Cost: the webhook verification and history code the brief grades are OpenClaw's,
documented and demonstrated rather than written here; side-effect approvals from chat
depend on OpenClaw's plugin-approval routing (verified in wave 2, web UI otherwise).

**D-012 Agent shell commands run in OpenClaw's Docker sandbox, always on.** (Andre)
`agents.defaults.sandbox.mode: "all"`; the gateway container gets the Docker socket and
host-path-mapped workspaces so sibling sandbox containers can mount them. Why: untrusted
code never runs in the gateway container. Cost: mounting the Docker socket is
root-equivalent on the host, recorded in the threat model; more setup surface.

**D-011 Both brief versions bind; OpenClaw is the execution engine. Supersedes D-001.**
(Andre) After reassessing with the PDF treated as binding alongside the portal PRD,
Andre chose OpenClaw over OpenCode and Goose as the embedded runtime. The Go control
plane (API, workflow engine, scheduler, store, UI, evaluation runner) orchestrates
OpenClaw agents over the gateway WebSocket protocol; OpenClaw executes every agent
turn and its tools. Platform tools that need idempotency and the approval gate are
served to OpenClaw over MCP from the Go binary. Why: the PDF requires one of the three
runtimes and Andre runs a four-agent OpenClaw fleet already, so the platform becomes a
real control plane for a runtime he uses. Trade-offs and the integration contract are
in `docs/adr/0001-runtime.md` (rewritten) and `docs/DESIGN.md` section 5.14.

**D-010 Deploy side effects: local deploy target plus optional GitLab MR.** (Andre)
`deploy_release` targets the `deploytarget` compose service and is always available, so
the approval-gated side effect works offline. `gitlab_create_mr` is a second
side-effecting tool, registered only when `GITLAB_TOKEN`, `GITLAB_PROJECT_ID` and
`GITLAB_BASE_URL` are set. Why: the brief mandates a local fallback and also rewards a
real external service.

**D-009 Operators may set a side-effecting tool to `auto` per agent.** (Andre)
The agent form shows a warning on that field; shipped templates keep `deploy_release`
and `gitlab_create_mr` gated. Why: the brief's interaction rules are "what the agent can
do autonomously vs what requires approval", which implies the operator chooses.

**D-008 Approvals can be decided from Telegram.** (Andre)
`/approve <id> [reason]` and `/reject <id> [reason]` act on pending approvals of runs
that were triggered from that conversation, and only from user ids in
`TELEGRAM_ALLOWED_USER_IDS` when that list is set. Why: makes the live conversation in
the demo meaningful; the restriction keeps a random chat member from approving a deploy.

**D-007 Rubric evaluator has an offline heuristic mode.** (Andre)
Every rubric criterion carries a local check (`contains`, `regex`, `json_path_eq`). With
`EVAL_PROVIDER=scripted` the rubric is scored by those checks and the report labels the
mode. Why: `make eval` must run with no cloud dependency.

**D-006 Approval wait time does not count toward the run time limit.** (Claude,
unobjected) Approvals have their own expiry, default 24 h. Why: a human may take hours;
a 10-minute run limit should bound agent work, not human latency.

**D-005 Everything runs in Docker.** (Andre) No toolchains installed on the host. The
Makefile wraps `docker compose`; Go and pnpm caches are named volumes. Why: keep the host
clean.

**D-004 Tracer-bullet build order.** (Andre) Wave 1 is a thin end-to-end slice (agent
form, run, real model, real tool, Postgres, SSE to the browser), later waves widen each
layer. Why: integration risk surfaces on day one and the first eval report is a true
baseline.

**D-003 Demo workflow is the dev pipeline.** (Andre) Coder writes code, Reviewer approves
or sends feedback back, Deployer deploys after a human approval. The loop is data in the
workflow definition, not Go code. Why: it is the brief's own example and exercises every
required mechanism (loop, typed state, sandbox, gated side effect).

**D-002 Telegram is the external channel.** (Andre) Webhook mode with the
`X-Telegram-Bot-Api-Secret-Token` header check; long-polling fallback for local runs
without a tunnel. Why: no workspace admin needed, easy to demo from a phone; the adapter
interface keeps Slack and WhatsApp addable.

**D-001 Native Go runtime, not Goose, OpenClaw or OpenCode.** (Andre) The attached PDF
(March 2026) requires embedding one of those; the portal PRD (October 2026, the version
the submission is tied to) makes frameworks optional. We build to the portal PRD. Why:
step and time limits, the approval gate, idempotency keys and per-call token accounting
are the heaviest-weighted rubric items, and a loop we own enforces them directly. See
`docs/adr/0001-runtime.md`.
