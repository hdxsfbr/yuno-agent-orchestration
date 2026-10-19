# Skills library sources

Vendored unchanged on 2026-10-19 (DESIGN 5.27 rule 10, D-069). Each skill was read in full
before it was committed; nothing here is run by the platform. Scripts run only when an
agent that holds the skill calls them through `exec` in its sandbox, and several need a
network or tools the default sandbox image does not have (noted below).

| Directory | Source | Commit or version | Licence |
|---|---|---|---|
| `anthropic/*` | https://github.com/anthropics/skills (`skills/<name>`) | `dbd4588f9e1033efb41dad4bef2f7947c8993d44` | Apache-2.0 (`LICENSE.txt` in each skill); `canvas-design` fonts under the SIL Open Font License (texts beside them) |
| `openclaw/*` | https://github.com/openclaw/openclaw (`skills/<name>`) | `f79f452826959925b400f94667b0b9a79fd7a8c9` | MIT (`openclaw/LICENSE`, the repository's licence) |
| `clawhub/test-runner`, `clawhub/debug-pro` | https://clawhub.ai, publisher cmanfre7 | 1.0.0 (download API, `ownerHandle=cmanfre7`) | MIT-0, as each ClawHub listing states (`clawhub/LICENSE`); security audit "Pass" on the listing |

Notes from the review:

- `anthropic/skill-creator`: its evaluation scripts call the `claude` CLI and its viewer
  frees a local port by killing the process that holds it; the instructions and
  references are the useful part in a sandbox.
- `anthropic/web-artifacts-builder`: its scripts install npm packages (some unpinned) and
  need node and the network.
- `anthropic/webapp-testing`: needs Playwright; `anthropic/slack-gif-creator`: needs
  Pillow, imageio and numpy; `anthropic/mcp-builder` and `anthropic/claude-api`: call the
  Anthropic API.
- `openclaw/github`: needs the `gh` CLI and the network; part of its text is specific to
  OpenClaw's own repository.
- Left out after the review: Anthropic's `docx`, `pdf`, `pptx`, `xlsx` (licence does not
  allow redistribution) and `doc-coauthoring` (no licence); OpenClaw's `coding-agent`
  (runs the Claude CLI with permission prompts off), `gh-issues` (reads a token from the
  gateway's config), `healthcheck` (host-specific), `nano-pdf` and `summarize` (need extra
  CLIs and send content to a model provider).
