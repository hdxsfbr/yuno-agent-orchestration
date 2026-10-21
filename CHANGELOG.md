# Changelog

Behaviour changes operators notice, newest first. The design reasons are in
`DECISIONS.md` and `docs/DESIGN.md`.

## Unreleased

- A plan revision that changes a ticket whose merge request has already merged no longer
  loses that change: it becomes a new follow-up ticket ("Follow-up to T8: ...") with its own
  issue in the current wave, and the approval message names it. The merged ticket and its
  issue stay as they were.

