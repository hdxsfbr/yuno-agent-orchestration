---
name: omm-view
description: Start the oh-my-mermaid viewer (containerised, make omm-view) to explore the .omm/ architecture diagrams in the browser. Use when the user says "omm view", "open viewer", "show diagrams", "view architecture", or "open architecture".
argument-hint: "[--port <port>]"
---

# omm-view — Architecture Viewer

## Purpose

Launch the interactive web viewer so the user can explore `.omm/` architecture diagrams in their browser.

## Prerequisites (this project)

Nothing is installed on the host: the CLI runs in a container through
`./scripts/omm.sh` and the viewer through `make omm-view` (compose service `omm`,
published on port 3030 of this host, or OMM_PORT).

## Steps

### Step 1: Verify .omm/ exists

```bash
./scripts/omm.sh list
```

If no classes found, tell the user:
"No architecture docs found. Run `/omm-scan` first to generate them."
Then stop.

### Step 2: Start the viewer

```bash
make omm-view
```

For another port: `OMM_PORT=<port> make omm-view`. The command keeps running; run it in
the background and stop it with `docker compose --profile tools stop omm` or Ctrl-C.

### Step 3: Report

Tell the user the viewer is running and give the URL on this host's LAN address
(for example `http://10.0.0.32:3030`), since the browser is on another machine.

The viewer auto-refreshes when `.omm/` files change.

## Rules

- Always check for existing classes before starting the viewer
- If no classes exist, suggest `/omm-scan` instead of starting an empty viewer
- The viewer is read-only — it does not modify `.omm/` files
