# Demo recording pipeline

Produces the walkthrough video of `docs/demo-script.md` from the running stack. A browser container
drives the real UI and records one WebM per scene; an ffmpeg container cuts the waits, adds the narration
and joins everything. Nothing is installed on the host, and nothing on screen is simulated: slow waits for
the model, the gateway or a person are recorded in `cuts.json` and stripped in post, not skipped.

```
make demo-record                      # dry run of every scene: read-only, stops before state-changing clicks
make demo-record SCENES=04,07         # only those scenes (re-record one without touching the others)
make demo-record LIVE=1               # the real thing: spends model budget and changes state
make demo-stitch                      # scripts/demo/out/demo.mp4, title.png, thumbnail.png
make demo-clean                       # delete scripts/demo/out
```

## Layout

| Path | What it is |
|---|---|
| `record.mjs` | Entry point: scene filter, pre-flight, one browser context and one video per scene, `cuts.json`. |
| `scenes.mjs` | Scenes 2 to 9 as functions, and the title and closing stills. |
| `helpers.mjs` | `waitCut`, pacing, the pointer overlay, auth injection, page helpers. |
| `entrypoint.sh` | Installs `playwright-core` into `.pw/` (git-ignored) once, then runs `record.mjs`. |
| `cards/` | `title.html`, `closing.html`, `thumbnail.html` (stills), `report.html` (scene 7), `terminal.html`, vendored `marked`. |
| `narration/NN.txt` | Narration per scene, copied from the demo script. Put `NN.mp3` or `NN.wav` next to it. |
| `stitch.sh` | Cuts, uniform re-encode, narration mux, concat. Runs ffmpeg in a container. |
| `fonts.conf` | Maps the CSS generic families to Liberation Sans in the browser container. |
| `out/` | Everything produced (git-ignored). |

The `demo` service in `compose.yaml` (profile `tools`) uses the official Playwright image
`mcr.microsoft.com/playwright:v1.49.1-noble` on the default compose network and reaches the UI as
`http://web:80`. `make demo-record` runs it with `--no-deps`, so it never touches the running stack.

## Before recording (pre-flight)

The recorder checks the first three and prints what it finds; the rest is yours.

1. The stack is up at the commit you are recording (`make ps`; `make smoke`). The title card shows `git rev-parse --short HEAD`.
2. `API_TOKEN` in `.env` matches the api's (the browser attaches it to `/api/*` requests; it is not shown or written anywhere).
3. The seeded agents exist, the Chief of Staff and the three panel reviewers included (`make seed`).
4. A **fresh editable copy of the research pipeline** exists (scene 9 schedules it; shipped templates cannot be scheduled).
5. For scene 3 either let it create a fresh copy of the dev pipeline (`LIVE=1`, the default) or have exactly the copy you want and pass `DEMO_NEW_COPY=0`.
6. The dev pipeline works on a project, so scene 3 registers one on the Projects page before the run: `PROJECT_REPO_URL`
   and `PROJECT_GIT_TOKEN` in `.env` (the api's bootstrap inputs) name the repository and a token that can push. `LIVE=1`
   creates the project, named after the repository path (`DEMO_PROJECT_NAME` to choose), or shows the one that already
   has that name, and clicks Check. The token is typed into a password field, so the footage shows dots, and nothing
   prints it. A workflow that does not opt in (`DEMO_PIPELINE_WORKFLOW`) needs none of this.
7. The Runs list shows what you want a viewer to see on scenes 6 and 9 (the eval suite leaves its runs there).
8. An editable copy of the **Review panel** exists (scene 5 opens it; with `LIVE=1` and no copy it makes one from the template), and a finished panel run to tour in a dry run (`DEMO_PANEL_RUN_ID`; default the run that opened !8).
9. Telegram bound to the Chief of Staff, the phone in hand, for scene 8 (`LIVE=1` waits up to 10 minutes for your message; the suggested one is printed with WAITING and is in `docs/demo-script.md`). The panel run of scene 5 should be recorded first, so the Chief has a run to talk about.
10. For scene 7's terminal segment, capture a real `make eval` console first (see below).
11. Record the time-free scenes first; scene 9 is time-bound (the schedule fires about two minutes after it is created).

## Dry run

```
make demo-record                          # all scenes, read-only
make demo-record SCENES=02,03,07 DEMO_PACE=0.4   # faster, for checking selectors
make demo-stitch
```

Without `LIVE=1` every scene stops before its state-changing click and nothing is spent or created:

| Scene | Dry run does | `LIVE=1` also does |
|---|---|---|
| 02 | tours Coder's form, read-only | same |
| 03 | fills the New project form and cancels (when the workflow works on a project), opens the template drawer, tours an existing editable copy, fills the run form with a project that exists, cancels | creates the project (or reuses the one with its name) and clicks Check, creates a copy from the template, picks the project, clicks **Start run**, saves the run id to `out/state.json` |
| 04 | tours the trace of `RUN_ID`, or of an existing finished run | follows the run scene 3 started; approves the gate and the `open_merge_request` call (`deploy_release` for a run without a project) |
| 05 | opens the Review panel in the builder (Coder, fork `panel`, three reviewers, join `merge`, lead Reviewer, gate, Deployer), fills the run form and cancels, then tours a finished panel run: header, fork card with its three columns, join card, the lead's verdict | picks the project, starts the run, waits until the fork card shows all three branches running, then the join, approves the gate and the `open_merge_request` call as scene 4 does, shows the merge request in the summary, saves `panel_run_id` to `out/state.json` |
| 06 | shows Coder's Max steps field, failed runs, rejected approvals | sets Max steps 2, runs Coder, restores the limit afterwards; runs Deployer and rejects its approval |
| 07 | renders `evals/reports/baseline.md` (no stack needed) | same |
| 08 | Channels page with the Telegram card (the Chief of Staff) and its conversation | waits for the message from the phone and for the Chief's reply, follows the reply's run link, and opens the panel run when the reply names it |
| 09 | fills the schedule form, cancels; shows Status | creates the once-schedule, waits for it to fire, shows the run and Status |

Output goes to `scripts/demo/out/`: `NN.webm` per scene, `01.png`/`10.png` with `.seconds`, `cuts.json`,
`state.json`, `thumbnail-src.png`, `shots/`. A scene that fails is saved as `NN.failed.webm` and
`NN-failure.png`; the last good `NN.webm` and its cuts are kept. A scene prints `SKIPPED ...` for a flourish
(a scroll, a pointer move) whose element it could not find; load-bearing steps fail the scene.

## Recording live

```
make demo-record SCENES=02,03 LIVE=1      # 03 registers the project and starts the dev-pipeline run
make demo-record SCENES=04 LIVE=1         # follows that run (or: DEMO_RUN_ID=<id>)
make demo-record SCENES=05 LIVE=1         # the review panel run (DEMO_PANEL_TASK, DEMO_PANEL_PROJECT)
make demo-record SCENES=06,07 LIVE=1
make demo-record SCENES=09 LIVE=1
make demo-record SCENES=08 LIVE=1         # send the bot a message when it prints WAITING
make demo-record SCENES=01,10             # stills, last, so the commit is final
make demo-stitch
```

Scene 4 should start within a minute or two of scene 3's run (it follows the run from the page, so a
finished run also works: the waits are then instant and nothing is cut).

`LIVE=1` for scene 3 creates the project from `PROJECT_REPO_URL` and `PROJECT_GIT_TOKEN` when none has its name; a
re-take finds it and shows its row instead (delete it on the Projects page first to record its creation again). The
run then clones the repository, and the Deployer ends by opening a merge request on it, which scene 4 approves and shows.

`LIVE=1` for scene 6 saves Coder's agent form with Max steps 2 and puts the previous value back in a
non-recorded session when the scene ends, also when it fails.

### Narration

`make demo-narrate` voices every `narration/NN.txt` with Chatterbox and the operator's reference voice (`narrate-chatterbox.sh`, the local GPU setup in `~/code/chatterbox-play`, one sentence per generation); `narrate.sh` is the piper fallback. Or write the audio with whatever you use for speech; one clip per scene named by scene number, next to its text:
`narration/01.mp3` ... `narration/10.mp3` (`wav`, `m4a`, `ogg`, `flac` also work). `make demo-stitch` picks
them up; scenes without a clip keep their footage length and are silent.

### The eval console (scene 7)

Scene 7 shows the report from `evals/reports/baseline.md`. To open it with a real `make eval` console first:

```
make eval 2>&1 | tee scripts/demo/out/eval-console.log
make demo-record SCENES=07
```

(Do this after `make demo-clean`, since that deletes `out/`. Set `DEMO_EVAL_CONSOLE` for another path inside the container.)

## Stitching

`make demo-stitch` runs `stitch.sh` in `linuxserver/ffmpeg` (override with `FFMPEG_IMAGE`). For each scene:

- strips the `cuts.json` intervals, keeping the first `CUT_HEAD_MS` (1000) of the pending state after the
  click and the last `CUT_TAIL_MS` (600) before the result; waits shorter than `MIN_CUT_MS` (1200) are left alone;
- re-encodes to one intermediate: 1440x900, 30 fps, `yuv420p`, H.264 (CRF 20) + AAC 48 kHz stereo;
- with narration: a still is held for the clip plus `BREATH` (1 s); footage longer than the clip plus
  `TAIL_SLACK` (2 s) is sped up to fit, capped at `MAX_SPEEDUP` (1.6x); shorter footage holds its last frame;
- without narration: stills use their `.seconds`, footage plays at 1x with silence.

Then it concatenates the intermediates into `demo.mp4` and writes `title.png` (the title still) and
`thumbnail.png` (1280x720, from `thumbnail-src.png`). It prints a table of footage, cut, narration, speed and
output length per scene, and the final duration. Intermediates are in `out/.stitch/`.

## `cuts.json`

One object per line, in footage time (ms from the start of the scene's video):

```
{"scene":"04","label":"coder finishes, reviewer reviews","start_ms":18250,"end_ms":93120}
```

`waitCut(page, label, fn)` records the interval of `fn`, called right after the action that started the
wait. `keep_head_ms` / `keep_tail_ms` override the defaults for one entry (the page-load trim uses 0/0).
Re-recording scenes replaces only their entries.

## Inputs and knobs

Read from `.env` as they are: `API_TOKEN` (the api's bearer token, required when the api has one) and, for a workflow
that works on a project (the dev pipeline), `PROJECT_REPO_URL`, `PROJECT_GIT_TOKEN` and `PROJECT_DEFAULT_BRANCH`: the
repository scene 3 registers. The token is typed into the page's password field and is never printed or written
anywhere by the recorder. Without the URL a dry run leaves the project form out and says so; `LIVE=1` refuses to start
the scene unless `DEMO_PROJECT_NAME` names a project that already exists.

Environment variables of `make demo-record` (all optional): `SCENES`, `LIVE`, `DEMO_PACE` (multiplies the
deliberate pauses), `DEMO_RUN_ID`, `DEMO_NEW_COPY=0`, `DEMO_PIPELINE_TASK`, `DEMO_STEP_LIMIT_TASK`,
`DEMO_REJECT_TASK`, `DEMO_SCHEDULE_TOPIC`, `DEMO_EVAL_CONSOLE`, `DEMO_RUN_TIMEOUT_MS`, `DEMO_PHONE_TIMEOUT_MS`, `DEMO_PANEL_RUN_ID` (the finished panel run toured in a dry run, default the run that opened !8), `DEMO_PANEL_TASK` (default `write slugify(text): lowercase, spaces to hyphens, strip punctuation, with tests`), `DEMO_PANEL_PROJECT` (default the project of `DEMO_PROJECT_NAME`), `DEMO_PANEL_WORKFLOW`, `DEMO_PANEL_TEMPLATE` (default `Review panel`),
`DEMO_BASE_URL`, `DEMO_MASK_IDS=1` (blurs long numeric ids such as a Telegram user id), `DEMO_PIPELINE_TEMPLATE`, `DEMO_PIPELINE_WORKFLOW`, `DEMO_SCHEDULE_WORKFLOW`,
`DEMO_PROJECT_NAME` (the project scene 3 registers or reuses; default: the last segment of the repository path),
`DEMO_PROJECT_DESCRIPTION` (typed into the project form when set). Stitch: `OUT_DIR`, `FFMPEG_IMAGE`, `FPS`, `WIDTH`, `HEIGHT`, `CRF`, `CUT_HEAD_MS`,
`CUT_TAIL_MS`, `MIN_CUT_MS`, `MAX_SPEEDUP`, `TAIL_SLACK`, `BREATH`.

## Known limits

- Playwright 1.49 records VP8 at a low bitrate, so text has faint noise around it. The stitched H.264 keeps what was recorded.
- The pointer you see is an overlay added to the page for the recording (browsers do not draw one into video); `CURSOR=0` removes it.
- The sidebar, Telegram card and approvals show what the stack holds (a bot name, allowlisted user ids): check them before publishing.
- After a recording, watch `demo.mp4` end to end; a scene recorded before a UI change is stale and must be re-recorded.

## Checking the result

- `scripts/demo/frames.sh 04.webm 5 20 60` writes frames of a scene to `out/frames/` (or `demo.mp4` after stitching;
  without times it takes one frame every `FRAMES_EVERY` seconds). Look at the first and last frame of every scene,
  and at the frames either side of each cut, then watch `demo.mp4` end to end.
- In the stitch table, a speed of `1.60x` means the scene's footage outruns its narration even at the cap: the voice ends
  and the screen goes on in silence. Re-record that scene with a smaller `DEMO_PACE` (it scales every deliberate
  pause), or trim the narration the other way. A scene whose footage is shorter than its clip holds its last frame.
- Stale footage is the usual failure: if the UI changed after a scene was shot, re-record that scene
  (`make demo-record SCENES=NN LIVE=1`). The title card shows the commit it was rendered at.

## Self-test

The recorder attaches `API_TOKEN` to the app's own `/api/` requests at the network layer. This proves it goes nowhere else
(other paths, other origins, the page) using two throwaway local servers and no stack:

```
docker compose --profile tools run --rm -T --no-deps --entrypoint node demo /demo/selftest-auth.mjs
```
