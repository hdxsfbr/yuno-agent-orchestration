package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"yuno/api"
	"yuno/internal/engine"
	"yuno/internal/store"
)

// Recovery commands (DESIGN 5.33 rules 1 to 3, 14, 15 and 18), API side.

// recoveryConformanceSteps exercises the command routes against the contract: pause, resume,
// steer and retry from a step with their 202, 409 and 422 answers, the command reads, and
// 404s.
func recoveryConformanceSteps(t *testing.T) []conformanceStep {
	t.Helper()
	ctx := context.Background()
	queued, err := createTestRun(ctx, testPool, store.TriggerManual, nil, store.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	running := endedTestRun(t, testPool, store.RunRunning, 1, store.Limits{})
	failed := endedTestRun(t, testPool, store.RunFailed, 1, store.Limits{MaxSteps: 10})
	recorded := &store.RunCommand{RunID: running.ID, Kind: store.CommandCancel}
	if err := store.InsertRunCommand(ctx, testPool, recorded); err != nil {
		t.Fatal(err)
	}
	return []conformanceStep{
		{http.MethodPost, "/api/runs/" + queued.ID + "/pause", "", http.StatusAccepted},
		{http.MethodPost, "/api/runs/" + queued.ID + "/pause", "", http.StatusConflict},
		{http.MethodPost, "/api/runs/" + queued.ID + "/resume", `{"note":` + strings.Repeat("x", 2001) + `}`, http.StatusUnprocessableEntity},
		{http.MethodPost, "/api/runs/" + queued.ID + "/resume", `{"note":"` + strings.Repeat("x", 2001) + `"}`, http.StatusUnprocessableEntity},
		{http.MethodPost, "/api/runs/" + queued.ID + "/resume", `{"note":"go on"}`, http.StatusAccepted},
		{http.MethodPost, "/api/runs/" + queued.ID + "/resume", "", http.StatusConflict},
		{http.MethodPost, "/api/runs/" + queued.ID + "/steer", `{"message":"hi"}`, http.StatusConflict},
		{http.MethodPost, "/api/runs/" + queued.ID + "/steer", `{"message":""}`, http.StatusUnprocessableEntity},
		{http.MethodPost, "/api/runs/" + running.ID + "/steer", `{"node_id":"agent","message":"look at README.md first"}`, http.StatusAccepted},
		{http.MethodPost, "/api/runs/" + failed.ID + "/retry", `{"from_node":"agent"}`, http.StatusAccepted},
		{http.MethodGet, "/api/runs/" + running.ID + "/commands", "", http.StatusOK},
		{http.MethodGet, "/api/runs/" + running.ID + "/commands/" + recorded.ID, "", http.StatusOK},
		{http.MethodGet, "/api/runs/" + running.ID + "/commands/no-such-command", "", http.StatusNotFound},
		{http.MethodGet, "/api/runs/no-such-run/commands", "", http.StatusNotFound},
		{http.MethodPost, "/api/runs/no-such-run/pause", "", http.StatusNotFound},
	}
}

// The five operations are in the contract with operationId equal to their handler and the
// risk PARITY.md gives them: routine for the commands, read for their reads.
func TestRecoveryOperations(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(api.OpenAPI)
	mustDo(t, err)
	for path, want := range map[string]struct{ method, id, risk string }{
		"/api/runs/{id}/pause":          {http.MethodPost, "pauseRun", "routine"},
		"/api/runs/{id}/resume":         {http.MethodPost, "resumeRun", "routine"},
		"/api/runs/{id}/steer":          {http.MethodPost, "steerRun", "routine"},
		"/api/runs/{id}/commands":       {http.MethodGet, "listRunCommands", "read"},
		"/api/runs/{id}/commands/{cid}": {http.MethodGet, "getRunCommand", "read"},
	} {
		item := doc.Paths.Value(path)
		if item == nil || item.GetOperation(want.method) == nil {
			t.Fatalf("%s: no %s in the contract", path, want.method)
		}
		op := item.GetOperation(want.method)
		if op.OperationID != want.id || op.Extensions["x-yuno-risk"] != want.risk {
			t.Fatalf("%s: operationId %q, x-yuno-risk %v", path, op.OperationID, op.Extensions["x-yuno-risk"])
		}
	}
}

// commandEngine answers the commands with what a test sets.
type commandEngine struct {
	*fakeEngine
	err  error
	run  *store.Run
	seen []string
}

func (e *commandEngine) Pause(_ context.Context, runID, by string) (*store.Run, error) {
	e.seen = append(e.seen, "pause "+runID+" "+by)
	return e.run, e.err
}

func (e *commandEngine) Resume(_ context.Context, runID, by, note string) (*store.Run, error) {
	e.seen = append(e.seen, "resume "+runID+" "+by+" "+note)
	return e.run, e.err
}

func (e *commandEngine) Steer(_ context.Context, runID, by, nodeID, message string) (*store.Run, error) {
	e.seen = append(e.seen, "steer "+runID+" "+by+" "+nodeID+" "+message)
	return e.run, e.err
}

func (e *commandEngine) Cancel(_ context.Context, runID, by string) (*store.Run, error) {
	e.seen = append(e.seen, "cancel "+runID+" "+by)
	return e.run, e.err
}

// The handlers pass the request to the engine as the operator and answer its run with the
// command (202), its refusal with the code, the detail and the command (409), a command that
// could not be recorded with 503, and a bad body with 422 before the engine sees it.
func TestRecoveryHandlers(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	run, err := createTestRun(ctx, pool, store.TriggerManual, nil, store.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	cmd := &store.RunCommand{RunID: run.ID, Kind: store.CommandPause}
	if err := store.InsertRunCommand(ctx, pool, cmd); err != nil {
		t.Fatal(err)
	}
	withCmd := *run
	withCmd.Command = cmd
	eng := &commandEngine{fakeEngine: &fakeEngine{pool: pool}, run: &withCmd}
	_, ts := newTestServer(t, pool, eng, nil)

	code, body := do(t, ts, http.MethodPost, "/api/runs/"+run.ID+"/pause", "")
	var got store.Run
	if err := json.Unmarshal(body, &got); code != http.StatusAccepted || err != nil || got.Command == nil || got.Command.ID != cmd.ID {
		t.Fatalf("pause: %d %s", code, body)
	}
	if code, body := do(t, ts, http.MethodPost, "/api/runs/"+run.ID+"/resume", `{"note":"  keep it short  "}`); code != http.StatusAccepted {
		t.Fatalf("resume: %d %s", code, body)
	}
	if code, body := do(t, ts, http.MethodPost, "/api/runs/"+run.ID+"/steer", `{"message":"look","node_id":"coder"}`); code != http.StatusAccepted {
		t.Fatalf("steer: %d %s", code, body)
	}
	want := []string{"pause " + run.ID + " operator", "resume " + run.ID + " operator   keep it short  ", "steer " + run.ID + " operator coder look"}
	if strings.Join(eng.seen, "|") != strings.Join(want, "|") {
		t.Fatalf("engine saw %q", eng.seen)
	}

	for _, tc := range []struct{ path, body, field string }{
		{"/steer", `{"message":""}`, "message"},
		{"/steer", `{"message":"` + strings.Repeat("é", engine.MaxSteerChars+1) + `"}`, "message"},
		{"/steer", `{"message":"x","node_id":"a\u0000b"}`, "node_id"},
		{"/resume", `{"note":"` + strings.Repeat("n", engine.MaxResumeNoteChars+1) + `"}`, "note"},
	} {
		code, body := do(t, ts, http.MethodPost, "/api/runs/"+run.ID+tc.path, tc.body)
		if code != http.StatusUnprocessableEntity || !strings.Contains(strings.Join(detailPaths(t, body), ","), tc.field) {
			t.Fatalf("%s %.40s: %d %s", tc.path, tc.body, code, body)
		}
	}
	if len(eng.seen) != 3 {
		t.Fatalf("a refused body reached the engine: %q", eng.seen)
	}

	rejected := *cmd
	rejected.Outcome, rejected.Detail = store.CommandRejected, "the run is waiting on approval a1"
	eng.err, eng.run = &engine.CommandError{Code: engine.CodeCommandRejected, Message: rejected.Detail, Command: &rejected}, nil
	code, body = do(t, ts, http.MethodPost, "/api/runs/"+run.ID+"/pause", "")
	e := decodeError(t, body)
	if code != http.StatusConflict || e.Error.Code != "command_rejected" || e.Error.Message != rejected.Detail ||
		!strings.Contains(string(e.Error.Details), `"outcome":"rejected"`) {
		t.Fatalf("refused pause: %d %s", code, body)
	}
	eng.err = engine.ErrCommandNotTaken
	if code, body := do(t, ts, http.MethodPost, "/api/runs/"+run.ID+"/cancel", ""); code != http.StatusServiceUnavailable ||
		decodeError(t, body).Error.Code != "command_not_taken" {
		t.Fatalf("cancel not taken: %d %s", code, body)
	}
	eng.err = &engine.CommandError{Code: engine.CodeCancelUnconfirmed, Message: "turn t1 of session s1 is not confirmed stopped yet"}
	if code, body := do(t, ts, http.MethodPost, "/api/runs/"+run.ID+"/resume", ""); code != http.StatusConflict ||
		decodeError(t, body).Error.Code != "cancel_unconfirmed" {
		t.Fatalf("resume while unconfirmed: %d %s", code, body)
	}
}

// GET /api/runs/{id}/commands lists the run's commands newest first; GET .../commands/{cid}
// reads one, 404 for another run's.
func TestRunCommandReads(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	a, err := createTestRun(ctx, pool, store.TriggerManual, nil, store.Limits{})
	mustDo(t, err)
	b, err := createTestRun(ctx, pool, store.TriggerManual, nil, store.Limits{})
	mustDo(t, err)
	first := &store.RunCommand{RunID: a.ID, Kind: store.CommandPause}
	mustDo(t, store.InsertRunCommand(ctx, pool, first))
	second := &store.RunCommand{RunID: a.ID, Kind: store.CommandSteer, Payload: json.RawMessage(`{"message":"hi"}`)}
	mustDo(t, store.InsertRunCommand(ctx, pool, second))
	other := &store.RunCommand{RunID: b.ID, Kind: store.CommandCancel}
	mustDo(t, store.InsertRunCommand(ctx, pool, other))

	code, body := do(t, ts, http.MethodGet, "/api/runs/"+a.ID+"/commands", "")
	var list []store.RunCommand
	if err := json.Unmarshal(body, &list); code != http.StatusOK || err != nil || len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("list: %d %s", code, body)
	}
	code, body = do(t, ts, http.MethodGet, "/api/runs/"+a.ID+"/commands/"+second.ID, "")
	var one store.RunCommand
	if err := json.Unmarshal(body, &one); code != http.StatusOK || err != nil || one.ID != second.ID || one.Outcome != store.CommandPending {
		t.Fatalf("get: %d %s", code, body)
	}
	if code, _ := do(t, ts, http.MethodGet, "/api/runs/"+a.ID+"/commands/"+other.ID, ""); code != http.StatusNotFound {
		t.Fatalf("another run's command: %d", code)
	}
}

// The status counts paused runs with the other live ones.
func TestStatusCountsPausedRuns(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	paused := func() int64 {
		t.Helper()
		code, body := do(t, ts, http.MethodGet, "/api/status", "")
		var st struct {
			Runs map[string]int64 `json:"runs"`
		}
		if err := json.Unmarshal(body, &st); code != http.StatusOK || err != nil {
			t.Fatalf("status: %d %s", code, body)
		}
		n, ok := st.Runs["paused"]
		if !ok {
			t.Fatalf("status has no paused count: %s", body)
		}
		return n
	}
	before := paused()
	run, err := createTestRun(ctx, pool, store.TriggerManual, nil, store.Limits{})
	mustDo(t, err)
	if _, err := store.PauseQueuedRun(ctx, pool, run.ID, run.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if n := paused(); n != before+1 {
		t.Fatalf("paused runs = %d, was %d", n, before)
	}
	if code, body := do(t, ts, http.MethodGet, "/api/runs?status=paused", ""); code != http.StatusOK || !strings.Contains(string(body), run.ID) {
		t.Fatalf("list of paused runs: %d %s", code, body)
	}
}
