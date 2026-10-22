# Changelog

Behaviour changes operators notice, newest first. The design reasons are in
`DECISIONS.md` and `docs/DESIGN.md`.

## Unreleased

- A conflict the documentation audit finds now asks you and waits; your decision goes back to the Builder instead of ending the ticket.
- Each run step works in its own private workspace: runs no longer see or change each other's folders, finished runs' folders are deleted, and the Python sandbox has libgomp.

- A plan can now have up to 100 tickets in up to 30 waves (it was 50 and 10). A plan over
  either limit is sent back to the Planner with the reason before you are asked to approve
  it, so an approved plan is never refused when its tickets are published. The limits are
  in the README and in DESIGN 5.29.
- The Integrator's merge request descriptions end with a "How it was checked" line. It
  reports what the earlier steps verified (the Builder's tests, the Code Reviewer's
  verdict, your manual test). When the run verified nothing, as in a quick run of the
  Integrator, it says "Not run in this task; the project's CI checks this merge request."
  instead of "NOT VERIFIED".
- The Builder and the Scout now install a project's dependencies from its lockfile, and
  when a new release of an unpinned lint or test tool breaks the project they report it
  and propose pinning the tool in the project, instead of changing unrelated code. The
  sandbox images already pin every tool they install; a test now keeps it that way.
- Opening a merge request no longer fails when GitLab or GitHub is slow to show the branch Yuno just pushed: Yuno waits for the branch, and names it if it never appears. The manual test step names the branch to pull only when the draft merge request exists; otherwise it tells you the draft is missing instead of showing a placeholder.
