# Changelog

Behaviour changes operators notice, newest first. The design reasons are in
`DECISIONS.md` and `docs/DESIGN.md`.

## Unreleased

- A draft merge request now says in its description not to merge it until Yuno marks it ready. If you merge it before the run's last push anyway, Yuno opens a small "Follow-up: ..." merge request with the files that were left instead of failing the ticket, and the run's report links it; the follow-up needs your merge too.
- Opening a merge request no longer fails when GitLab or GitHub is slow to show the branch Yuno just pushed: Yuno waits for the branch, and names it if it never appears. The manual test step names the branch to pull only when the draft merge request exists; otherwise it tells you the draft is missing instead of showing a placeholder.
