# Wave 10 review (2026-10-18)

Two Sonnet 5.5 reviewer agents read the wave's diff against `main` (`5579c3d..e0fdc22`)
with disjoint scopes: the engine, tools, notices, prompts, artifact collector and diff;
and the HTTP API, the store plumbing, the migration, the contract and the web UI. Both ran
the packages' tests read-only in their own databases and reproduced their findings with
throwaway probes (removed afterwards); the backend reviewer also ran the decision race 120
times around the turn's end and found no lost or doubled decision. The coordinator's
disposition of each finding follows; where a finding showed a rule wrong rather than code
wrong, the rule was amended first (noted) and the fix pass built the amended rule. The
fix pass covered every kept item in one round with two Sonnet 5.5 fix agents (backend,
frontend).

## Engine, tools, notices, prompts, collector and diff

| # | Severity | Finding | Disposition |
|---|---|---|---|
| H1 | high | An empty diff deletes the earlier generated `diff.patch`; after the Deployer pushes, `checkout_sha` moves to the pushed commit (5.22 rule 2), so every successful run that updates a merge request ends without a diff | fixed: an empty diff leaves the generated row alone (5.25 rule 3 had already been amended to say so; T5.25.3 amended to match) |
| M1 | medium | The sweep treats every directory whose name matches a loose pattern as a run directory, so a shared or mis-set `ARTIFACT_DIR` loses `backups/` or `tmp/` on the first pass | fixed: the sweep touches only directories named by a 36-character run id |
| M2 | medium | The task's `Questions:` paragraph reads the raw allowlist while `AGENTS.md`'s section reads the effective one, so an agent whose `ask_operator` is blocked is still told to ask | fixed: both read the effective allow list through one exported helper |
| L1 | low | The diff's working-tree copy takes no context and has no size cap, so a huge checkout runs past the 60 s deadline | fixed: the copy honours the context and stops at 500 MiB, skipping the diff with a log line |
| L2 | low | The checkout is reached by path after the collector's link check, so a process still alive in the sandbox could swap `repo/` for a link between the two | not changed: the same path check guards `open_merge_request` (5.16 rule 10); recorded as a limitation in the threat model with it |
| L3 | low | Options are stored unredacted while the question is redacted | fixed |
| L4 | low | A NUL in a question or an answer fails the insert with a database error the model and the operator cannot act on | fixed: NUL and invalid UTF-8 are refused with the existing `question:` and `answer:` texts (with the API review's L1) |
| L5 | low | `tools.QuestionParker` has no compile-time assertion and `QuestionPending` drops the caller's deadline | fixed |
| L6 | low | The decision handoff to a live visit waits without a bound while the visit loop is busy | fixed: a 5 s bound, then the turn's end reads the decided row |
| L7 | low | A next turn that carries an answer counts against the model-error retry budget | fixed: error retries counted apart |
| L8 | low | A failed read of the step's questions sends "Continue" without the paragraph, so the answer never reaches the model | fixed: the visit fails with the read's error, as a visit never runs on a silently incomplete prompt (5.18) |
| L9 | low | Two collections of one run can overlap (the parked visit's and the continuation's), and an unchanged file leaves an unnamed store file | fixed: a per-run mutex in the collector; the unnamed file is removed at once |
| L10 | low | An update renames the new bytes before the row is updated, so a failed row update leaves the old row over new content | not changed: the next collection rewrites the file since the sha differs; 5.25 Consistency amended to say so |
| L11 | low | The notice to the Telegram-bound agent (rule 6 (b)) is sent without checking that the question is still pending | fixed: the row is re-read in the goroutine and a decided question sends nothing |
| L12 | low | A late `ask_operator` after `finish` was accepted creates a card and a notice that close at once | fixed: refused with `the step has finished` |
| L13 | low | A retry counts the step's loop visits although an unconsumed continuation would be let in by the walk | fixed: the retry skips the count when a continuation question exists |

## HTTP API, store, contract and UI

| # | Severity | Finding | Disposition |
|---|---|---|---|
| M1 | medium | Eval case 015's writer task names `out/report.md` but its scripted `write` and `finish` still use `runs/$RUN_ID/report.md` | fixed: both paths moved under `out/` |
| L1 | low | A NUL in the answer, or in a run or artifact id on the new routes, answers 500 | fixed: the answer is 422, a NUL in an id is not found (as wave 9's L1) |
| L2 | low | The answer box caps characters while the server counts bytes; a long multi-byte answer passes the box and gets 422 | fixed: the box counts bytes and no longer cuts a paste silently |
| L3 | low | The download's object URL is revoked right after the click | fixed: revoked after several seconds |
| L4 | low | Format characters (U+202E) survive in the download name on both sides, so a saved name can read as another type | fixed: format characters and the line separators dropped in the header and in the UI's name; the collector refuses such names |
| L5 | low | XML types pass through the content-type override | fixed: any type containing `xml` is sent as plain text |
| L6 | low | No index backs `run_nodes.question_id`, and the two step queries bind `kind` so the partial index cannot apply | fixed in migration 0014 (not yet deployed) and the queries write the literal |
| L7 | low | `ARTIFACT_DIR` is configurable but the volume target is fixed; a negative TTL keeps files forever | fixed: the volume mounts at `${ARTIFACT_DIR}`; a negative TTL is the default |
| L8 | low | After retention the terminal empty state says "No files were delivered." for a run that did | fixed: 5.25 rule 7 amended to `No files are available for this run.` |
| L9 | low | With the store disabled the endpoints keep serving rows that exist, against rule 4's "empty lists" | not changed: 5.25 rule 4 amended (what was stored stays served) |
| L10 | low | Accessibility: the 422 text is not described to the box, the absolute date lives only in a title, a question card is named "Approval" | fixed |
| L11 | low | The agent form's tool picker offers "Require approval" for `ask_operator` | fixed: hidden for `ask_operator` (and `finish`) |

Recorded, not changed: the eval runner decides only approve and reject, so a real-model
eval whose agent asks a question would wait until the case times out (no shipped case
asks); the question cap counts every status, so a retried step that asked three times
cannot ask again; a recovery visit after a restart counts toward the loop limit (it is not
a continuation).

## Coverage reported by the reviewers

Rules checked and found correct: 5.24 rules 1 to 12 (validation texts, risk and timeout,
the waiter before the insert, the pause and span, nothing registered with the gateway, the
four result shapes, the delivery order with its second re-queue, the one-transaction park
and the idempotent re-queue closing the parked row, the continuation question's
selection, the paragraph's text and placement, the expiry cases, `answer_question`'s
texts and `decided_by`, the notices on both routes with the claim and the key the mirror
reads, the loop count in `enter` and `Retry`, the prompts and the `## Questions` section,
the agent form's refusal); 5.25 rules 2 to 6 (the `os.Root` use with `SameFile` and
`O_NONBLOCK`, caps and skip line, rename before the row on insert, no visit failed by a
collection, git only in the private clone with the token in the environment, the sha
fetch and the 10 MiB cut, content types, the TTL from `finished_at`, live and retried runs
kept, `TTL=0` orphans only); the answer endpoint's order and texts; the download headers
on every path including `Range` and `HEAD`; the contract against the served JSON; the
store's upsert, re-queue predicate and claim lists; the migration against existing rows;
config coverage and the image's ownership of `/data/artifacts`; the UI's card states,
header line, Timeline labels, blob download and previews with no `innerHTML`, the
`run.artifacts` refresh ignoring other runs, the sidebar count, dark mode; no token in
any result, log line or fixture.
