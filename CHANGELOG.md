# Changelog

Behaviour changes operators notice, newest first. The design reasons are in
`DECISIONS.md` and `docs/DESIGN.md`.

## Unreleased

- A merge request now carries only what its run changed. Work that reached main after the
  run started (another ticket's merge) is kept instead of being deleted by the run's
  commit. When the run and main changed the same file, nothing is pushed and the agent is
  told which files conflict, so it can ask you how to resolve them.

