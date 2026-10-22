package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"yuno/internal/liveness"
	"yuno/internal/store"
)

// DESIGN W2 Outputs: the diagnosis gains the run's pending commands (with whether the worker
// took them) and its cancel_state; a paused run, and its paused step, wait for a resume.
func TestDiagnosisRecoveryFields(t *testing.T) {
	base := diagnosisServer(t)
	ctx := context.Background()
	router := contract(t)
	now := time.Now().UTC()
	agentID := testAgent(t, "recovery-"+uniqueSuffix())

	// A running run with a pause the worker took and a steer it has not read yet.
	running, err := createTestRun(ctx, testPool, store.TriggerManual, nil, store.Limits{})
	mustDo(t, err)
	setRun(t, running.ID, store.RunRunning, now, nil)
	step := liveVisit(t, running.ID, "coder", agentID, nil, now.Add(-time.Minute))
	mustDo(t, store.TouchRunNode(ctx, testPool, running.ID, step.ID, now))
	pause := &store.RunCommand{RunID: running.ID, Kind: store.CommandPause}
	mustDo(t, store.InsertRunCommand(ctx, testPool, pause))
	_, err = store.AckRunCommand(ctx, testPool, running.ID, pause.ID)
	mustDo(t, err)
	steer := &store.RunCommand{RunID: running.ID, Kind: store.CommandSteer}
	mustDo(t, store.InsertRunCommand(ctx, testPool, steer))
	settled := &store.RunCommand{RunID: running.ID, Kind: store.CommandResume}
	mustDo(t, store.InsertRunCommand(ctx, testPool, settled))
	_, err = store.SettleRunCommand(ctx, testPool, settled.ID, store.CommandRejected, "the run is not paused")
	mustDo(t, err)

	d := getDiagnosis(t, router, base, running.ID)
	if len(d.CommandsPending) != 2 || d.CommandsPending[0].CommandID != pause.ID || d.CommandsPending[0].AckedAt == nil ||
		d.CommandsPending[1].CommandID != steer.ID || d.CommandsPending[1].AckedAt != nil || d.CommandsPending[1].Kind != store.CommandSteer {
		t.Fatalf("commands_pending = %+v", d.CommandsPending)
	}
	if d.CancelState != "" {
		t.Fatalf("cancel_state = %q", d.CancelState)
	}

	// A paused run: its parked step and the run wait for a resume; an unconfirmed stop shows.
	paused, err := createTestRun(ctx, testPool, store.TriggerManual, nil, store.Limits{})
	mustDo(t, err)
	setRun(t, paused.ID, store.RunRunning, now, nil)
	parked := liveVisit(t, paused.ID, "coder", agentID, nil, now.Add(-time.Minute))
	mustDo(t, store.TouchRunNode(ctx, testPool, paused.ID, parked.ID, now))
	_, err = store.PauseVisit(ctx, testPool, paused.ID, parked.ID, now.Add(-90*time.Second))
	mustDo(t, err)
	_, err = store.SetRunCancelState(ctx, testPool, paused.ID, store.CancelUnconfirmed)
	mustDo(t, err)
	d = getDiagnosis(t, router, base, paused.ID)
	if d.Status != store.RunPaused || d.Liveness != liveness.Waiting || !strings.HasPrefix(d.Reason, "paused by the operator 1m30s ago") ||
		d.CancelState != store.CancelUnconfirmed {
		t.Fatalf("paused run = %s %s %q %q", d.Status, d.Liveness, d.Reason, d.CancelState)
	}
	if len(d.Nodes) != 1 || d.Nodes[0].Liveness != liveness.Waiting || d.Nodes[0].Reason != "paused by the operator; a resume continues the step" {
		t.Fatalf("paused step = %+v", d.Nodes)
	}

	// The list holds the paused run with the other live ones.
	code, body := checkConformance(t, router, base, http.MethodGet, "/api/runs/diagnosis", "", true)
	if code != http.StatusOK || !strings.Contains(string(body), paused.ID) || !strings.Contains(string(body), `"commands_pending"`) {
		t.Fatalf("list: %d %s", code, body)
	}
}
