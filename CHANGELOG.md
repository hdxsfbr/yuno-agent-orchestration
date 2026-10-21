# Changelog

Behaviour changes operators notice, newest first. The design reasons are in
`DECISIONS.md` and `docs/DESIGN.md`.

## Unreleased

- Each run step works in its own private workspace: runs no longer see or change each other's folders, finished runs' folders are deleted, and the Python sandbox has libgomp.

