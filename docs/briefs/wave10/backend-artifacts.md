# Wave 10 backend brief C: run artifacts (5.25)

Spec: `docs/DESIGN.md` 5.25 rules 2 to 6 and their edge cases, with the amendment to
5.6 rule 13; decisions D-047 and D-057; `api/openapi.yaml` 0.7.0: `Artifact`, `GET
/api/runs/{id}/artifacts`, `GET /api/runs/{id}/artifacts/{artifactId}`. Read
`docs/briefs/wave10/00-plan.md` first: the prerequisites there are ground truth and
frozen, in particular migration `0014_wave10.sql` (`run_artifacts`), `store.RunArtifact`
and `store.ListRunArtifacts` (yours to extend in `internal/store/artifacts.go`),
`engine.ArtifactCollector` and `engine.ArtifactInput` (`contract.go`: the interface your
collector satisfies and what each visit's end hands you), `engine.Emit` and
`engine.LogEvent` (how you publish `run.artifacts` and the log lines), `config.ArtifactDir`
and `config.ArtifactTTLDays`, `openclaw.OpenRunDir` (the `os.Root` of `runs/<run id>/`
in an agent's workspace), and the 501 stubs in `internal/httpapi/artifacts.go` you
replace. The coordinator wires your collector into the server at integration
(`artifacts.New`, `Deps.Artifacts`, the sweeper's `Also`): your package needs no
knowledge of `server.go`.

Already in the tree and worth reading before you write: `internal/openclaw/rundir.go`
(`CopyRunDir`, `openRunDir`, `copyFile`: the walk over an `os.Root`, how links are
refused and skipped), `internal/project/git.go` (`CloneBranch`, `HeadSHA`, `ProposeOnto`
and `propose`: the private clone, the working-tree copy with the excluded directories,
how git is run with the token and never inside the checkout) and `git_onto_test.go`
(the local bare repository as the remote), `internal/project/toolchain.go` (`RepoRel`,
the excluded directories of 5.16 rule 3), `internal/memory/sweeper.go` (`SweepFunc`,
`Also`), `internal/httpapi/runs.go` (`getRun`, 404 handling, `writeJSON`),
`internal/httpapi/errors.go`, `internal/httpapi/runs_test.go` (the handler harness),
`internal/engine/wave8_projects_test.go` (a project run with a checkout under the
scripted provider: the fixture for your diff test), `internal/httpapi/openapi_test.go`
(`conformanceStep`, how a steps function is shaped; the coordinator appends yours).

## Deliverables

1. **The collector (rule 2).** New package `internal/artifacts`: `type Collector struct
   { Dir string; Pool *pgxpool.Pool; Log *slog.Logger; Emit func(ctx, runID, event
   string, payload map[string]any); LogEvent func(ctx, runID, level, msg string, attrs
   ...any); Now func() time.Time; TTL time.Duration }` and `New(dir string, pool, log,
   emit, logEvent, ttlDays int) (*Collector, error)`: creates `dir` if missing, writes
   and removes a probe file, and returns an error (the coordinator logs `artifacts:
   <dir>: <error>; artifacts are disabled` and passes nil) when it cannot. `Collect(ctx,
   in engine.ArtifactInput)` satisfies `engine.ArtifactCollector`: a 60 s deadline;
   `openclaw.OpenRunDir(stateDir?, ...)`: `in.Workspace` is the workspace directory, so
   open it with `os.OpenRoot(in.Workspace)` then the run directory as `OpenRunDir` does
   (use the exported helper if its signature fits, else `os.OpenRoot` twice with the
   same link refusals); `out` must be a plain directory (`Lstat`), a link or a file is
   the log line `artifacts: out/ is not a plain directory` and nothing collected; a
   missing `out/` collects nothing silently. Walk `out/` with `fs.WalkDir` over
   `root.FS()` in lexical order; regular files only (`d.Type().IsRegular()`), links and
   special files skipped; a path over 512 bytes skipped; the per-run room read once
   before the walk (`store.ArtifactTotals(ctx, q, runID) (files int, bytes int64, err
   error)`), a file over 20 MiB or past the room (200 files, 50 MiB) skipped; each file
   copied through `root.Open` into `<Dir>/<run id>/<new id>.tmp` while hashing
   (`sha256`), then `store.UpsertRunArtifact(ctx, q, RunArtifact{...})`: insert when no
   row at `(run_id, path)`; when the row's `sha256` equals the new one, the temp file is
   removed and nothing changes; otherwise the row is updated in place (`node_id`,
   `visit`, `size`, `sha256`, `content_type`, `updated_at`) and the temp file is renamed
   over `<Dir>/<run id>/<row id>` (the new row's id for an insert). Skipped files: one
   `LogEvent(info)` line `artifacts: <n> files not collected: <path>: <reason>; ...`
   naming the first three; a span is optional (the engine's spans are not exported:
   leave it to the log line and say so in your report). After a collection that
   changed anything: `Emit(ctx, runID, "run.artifacts", {"run_id", "count"})`. Any
   failure is `LogEvent(warn, "artifacts: <error>")` and the function returns; nothing
   propagates to the visit.
2. **The diff (rule 3).** `internal/project/diff.go`: `func (c Config) Diff(ctx
   context.Context, checkout, branch, base string) ([]byte, error)`: refuses a
   `checkout` without a `.git` directory (`ErrNoCheckout`); clones `branch` at depth 1
   into a private temporary directory (`CloneBranch`), and when `base != ""` and
   `HeadSHA` differs, `git fetch --depth 1 origin <base>` then `git checkout --detach
   <base>` (a failure is the returned error, the caller skips); empties the working
   tree and copies the checkout's tree over it exactly as `propose` does (reuse its
   helper, exporting nothing new unless needed: the copy rule of 5.16 rule 10, links
   skipped, `.git` and the excluded directories left out); `git add -A`; `git diff
   --cached --binary --no-color --no-ext-diff`; removes the directory; returns the
   patch (empty for no change). In the collector: when `in.Project != nil` and the
   checkout `runs/<run id>/repo/` exists in the workspace (through the root, a link
   refused), call `in.Project.Diff(ctx, checkoutPath, in.Branch, derefString(in.Run.CheckoutSHA))`;
   a non-empty patch over 10 MiB is cut and ends with `\n# cut at 10 MiB\n`; store it as
   path `diff.patch` with `generated = true` and `content_type` `text/x-patch` through
   the same upsert, unless the agent's own `out/diff.patch` was collected in this pass
   (then a log line `artifacts: diff.patch skipped: the agent wrote its own`); an empty
   patch deletes an earlier row at `diff.patch` with `generated = true`
   (`store.DeleteGeneratedArtifact`) and its file; a `Diff` error is the log line
   `artifacts: diff skipped: <reason>`. The patch counts toward the caps like any file.
3. **Content types (rule 4).** `contentType(path string, head []byte) string`:
   `http.DetectContentType(head)`, overridden by extension for `.md`, `.json`, `.csv`,
   `.patch`, `.diff`, `.yaml`, `.yml`, `.svg` and the source extensions `.py .go .js .ts
   .tsx .sh .sql .toml .ini .log`, as rule 4 lists them; unknown and empty → the
   detector's answer or `application/octet-stream`. A table test.
4. **The store** (`internal/store/artifacts.go`, extending P's file): `ArtifactTotals`,
   `UpsertRunArtifact` (returning the row and whether it changed), `GetRunArtifact(ctx,
   q, runID, id) (*RunArtifact, error)` (`ErrNotFound` when the id is not this run's),
   `DeleteGeneratedArtifact(ctx, q, runID string) (*RunArtifact, error)`,
   `ListExpiredArtifactRuns(ctx, q, before time.Time) ([]string, error)` (run ids with
   rows whose run's `finished_at < before`), `DeleteRunArtifacts(ctx, q, runID) (int64,
   error)`; tests for each.
5. **The endpoints (rule 5).** `internal/httpapi/artifacts.go` replaces P's stubs:
   `listArtifacts`: 404 for an unknown run (`store.GetRun`), else
   `store.ListRunArtifacts` as JSON (`[]`, never `null`); `downloadArtifact`: the row
   through `GetRunArtifact` (404 `not_found`), the file at `<Dir>/<run id>/<id>` (a
   missing file is 404 with the message `artifact file is missing`), headers
   `Content-Type` (the stored type, except `text/html`, `application/xhtml+xml` and
   `image/svg+xml` → `text/plain; charset=utf-8`), `Content-Length`,
   `Content-Disposition: attachment; filename="<base name of path with quotes and
   control characters removed>"`, `X-Content-Type-Options: nosniff`, `Cache-Control:
   private, no-store`; `http.ServeContent` is fine when it keeps these headers (set
   them first; it must not sniff: the type is set). The server needs the directory:
   add `ArtifactDir string` to the `Server`'s fields through its existing options or
   `cfg` (read `s.cfg.ArtifactDir`); an empty directory (collector disabled) answers
   `[]` and 404. `artifactConformanceSteps(t) []conformanceStep` in your
   `artifacts_test.go`: the list (200 with two rows inserted by hand, 404 unknown run),
   the download's headers and body, 404 for another run's id and for a missing file.
6. **The sweep (rule 6).** `func (c *Collector) Sweep(ctx context.Context, now
   time.Time) (int64, error)` with `memory.SweepFunc`'s signature: when `c.TTL > 0`,
   `ListExpiredArtifactRuns(now - TTL)`, for each `DeleteRunArtifacts` then
   `os.RemoveAll(<Dir>/<run id>)`; then every directory under `Dir` whose run has no
   rows (`store.ArtifactTotals` 0 files) and whose run row is gone or finished is
   removed; returns the number of rows deleted; one `Log.Info` line with the counts
   when anything was removed. `TTL == 0` removes orphans only.
7. **Tests**: T5.25.1 to T5.25.7 as the design lists them: the collector against a
   fixture workspace built with `t.TempDir()` (nested files, a symlink, a FIFO where the
   platform allows, a link at `out/`, 201 files, a 21 MiB sparse file, the 50 MiB cap
   with small files and a lowered cap through an unexported variable), the same file
   across visits, a changed file, a deleted file; the diff against a local bare
   repository as `git_onto_test.go` builds one (a change yields a hunk; no change yields
   none and removes an earlier generated row; the agent's own `diff.patch` wins; a
   moved branch diffs against `checkout_sha`; an unfetchable sha skips with the reason;
   `node_modules` left out); content types; the endpoints and headers; the sweep (31
   days ago gone, yesterday kept, an orphan directory gone, a live run kept, `TTL 0`);
   the disabled store (`New` on an unwritable directory fails; the handler with an
   empty directory answers `[]` and 404).

## Rules of the house

- Everything runs in Docker, from this worktree in a throwaway container on the stack's
  network with your own test database:

  ```
  docker run --rm --network yuno_default -v "$PWD:/src" -v yuno_gomod:/go/pkg/mod -v yuno_gocache:/root/.cache/go-build -w /src -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 -e DATABASE_URL='postgres://yuno:yuno@postgres:5432/yuno_test_b?sslmode=disable' yuno-go-tools:1.26 sh -c 'go test ./internal/artifacts/ ./internal/project/ ./internal/store/ ./internal/httpapi/ -count=1 2>&1 | tail -60'
  ```

  (the image has git for the diff tests.) vet with `go vet ./...` and `gofmt -l .` in
  the same container. Write long outputs to a file under `/tmp` and read the file.
  Never run `make run`, `make down`, `make clean`, `make test-*`, `make lint` or `docker
  compose ...` from this directory; never run the whole suite: the coordinator does.
- Files you own: `internal/artifacts/` (new); `internal/project/diff.go` and
  `diff_test.go`; `internal/store/artifacts.go` and `artifacts_test.go`;
  `internal/httpapi/artifacts.go` and `artifacts_test.go`; `internal/openclaw/rundir.go`
  (additions only). Nothing else: `types.go`, `contract.go`, `walk.go`, `node.go`,
  `wave10.go`, `git.go` (add helpers in `diff.go`; if `propose`'s copy helper must be
  exported, say so in your report and copy the minimum), `server.go`, `main_test.go`,
  the migrations, `web/`, `docs/`, `api/openapi.yaml` are other hands' or frozen.
- Never run git inside the agent's checkout, not even `git status`: the private clone
  is the only place git runs (5.16 rule 10). Never follow a link. The token never
  appears in a log line, an error or a fixture (`project.Config` redacts its URL; use
  its helpers).
- Rule texts are exact: copy them from 5.25, do not paraphrase.
- Commit nothing; the coordinator commits. Report: the files changed, the test
  output's last lines, every rule you had to interpret, the exact `New` signature you
  settled on for the coordinator's wiring, and anything you found wrong in the design.
