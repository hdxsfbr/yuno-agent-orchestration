# Changelog

Behaviour changes operators notice, newest first. The design reasons are in
`DECISIONS.md` and `docs/DESIGN.md`.

## Unreleased

- Runs see the history of their branch and of main. A run that updates a merge request
  starts with main merged into the request's branch: when they conflict, the agent is
  told which files to resolve, and the push carries the merge, so the request can be
  merged without conflicts.

- A merge request now carries only what its run changed. Work that reached main after the
  run started (another ticket's merge) is kept instead of being deleted by the run's
  commit. When the run and main changed the same file, nothing is pushed and the agent is
  told which files conflict, so it can ask you how to resolve them.

