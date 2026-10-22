package liveness

import (
	"testing"

	"yuno/internal/store"
)

// DESIGN W2 rule 14: a paused run waits for a resume, with its age, and so does its paused
// step, whatever its heartbeat (no worker holds it).
func TestDiagnosisPaused(t *testing.T) {
	got, reason := ClassifyRun(Run{Status: store.RunPaused, PausedAt: ago(95)}, th, checkedAt)
	if got != Waiting || reason != "paused by the operator 1m35s ago; waiting for a resume" {
		t.Fatalf("paused run = %s %q", got, reason)
	}
	if got, reason := ClassifyRun(Run{Status: store.RunPaused}, th, checkedAt); got != Waiting || reason != "paused by the operator; waiting for a resume" {
		t.Fatalf("paused run without paused_at = %s %q", got, reason)
	}
	v := ClassifyNode(Node{NodeID: "coder", Parked: true, Paused: true, HeartbeatAt: ago(4000)}, th, checkedAt)
	if v.Liveness != Waiting || v.Reason != "paused by the operator; a resume continues the step" {
		t.Fatalf("paused step = %+v", v)
	}
}
