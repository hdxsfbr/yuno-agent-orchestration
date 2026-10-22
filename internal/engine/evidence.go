package engine

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"yuno/internal/liveness"
	"yuno/internal/obs"
	"yuno/internal/store"
)

// Liveness evidence (DESIGN 5.12): a live visit records on its row what it waits on, its
// open tool calls, its last progress and its heartbeat, so a diagnosis rests on timestamps.
// Everything here runs on the visit goroutine, from the state the stall check (5.5 rule 6)
// already keeps: progressAt and openCalls.

// openCall is a tool call of the current turn with no result yet. Its name, start and
// summary come from whichever of the item event and the transcript entry gives them first;
// the item's start, when the tool really started, wins over the time the model asked.
type openCall struct {
	name    string
	started time.Time
	exact   bool // started is the item's own start
	summary string
}

// writtenEvidence is what the visit's row holds: the evidence last written, and when
// progress_at was last written (the engine's clock).
type writtenEvidence struct {
	waitingOn      string
	modelWaitSince time.Time
	openCalls      string
	progressAt     time.Time
	progressWrote  time.Time
}

// startEvidence records the visit's start: its first heartbeat and progress, waiting on
// setup until the first turn.
func (v *visit) startEvidence(ctx context.Context, now time.Time) {
	v.waitingOn = store.WaitingOnSetup
	if err := store.StartRunNodeEvidence(context.WithoutCancel(ctx), v.e.deps.Pool, v.rn.ID, now); err != nil {
		v.log.Warn("record liveness evidence failed", "error", err)
		return
	}
	v.written = writtenEvidence{waitingOn: store.WaitingOnSetup, openCalls: "[]", progressAt: now, progressWrote: now}
}

// noteOpenCall opens a call of the live turn, or fills in what an earlier sighting lacked.
// exact says started is the item's own start.
func (v *visit) noteOpenCall(id, name string, started time.Time, exact bool) {
	c := v.openCalls[id]
	if c == nil {
		c = &openCall{}
		v.openCalls[id] = c
	}
	if c.name == "" {
		c.name = name
	}
	if !started.IsZero() && (c.started.IsZero() || (exact && !c.exact)) {
		c.started, c.exact = started, exact
	}
	if c.summary == "" {
		c.summary = callSummary(v.inputs[id].input)
	}
}

// callSummary is an open call's summary: the redacted first 200 characters of its command
// or path argument, on one line; "" when it has neither.
func callSummary(args json.RawMessage) string {
	var a map[string]json.RawMessage
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	for _, k := range []string{"command", "path", "file_path"} {
		var s string
		if json.Unmarshal(a[k], &s) != nil {
			continue
		}
		if s = strings.Join(strings.Fields(obs.Redact(s)), " "); s == "" {
			continue
		}
		if r := []rune(s); len(r) > liveness.SummaryChars {
			s = string(r[:liveness.SummaryChars])
		}
		return s
	}
	return ""
}

// currentWait is what the visit waits on now: a person while an approval or a question
// pauses it, setup before its first turn, a tool while a call of the live turn is open, the
// model otherwise; nothing it can name between turns or while the gateway is away.
func (v *visit) currentWait() string {
	switch {
	case v.rn.Status == store.NodeWaitingApproval:
		return store.WaitingOnHuman
	case v.turn == 0:
		return store.WaitingOnSetup
	case !v.turnLive || v.disconnected:
		return ""
	case len(v.openCalls) > 0:
		return store.WaitingOnTool
	}
	return store.WaitingOnModel
}

// openCallsJSON is open_calls as the row holds it: the live turn's open calls, oldest first.
func (v *visit) openCallsJSON() string {
	if !v.turnLive || len(v.openCalls) == 0 {
		return "[]"
	}
	list := make([]liveness.OpenCall, 0, len(v.openCalls))
	for id, c := range v.openCalls {
		list = append(list, liveness.OpenCall{ID: id, Name: c.name, StartedAt: c.started.UTC(), Summary: c.summary})
	}
	slices.SortFunc(list, func(a, b liveness.OpenCall) int {
		if c := a.StartedAt.Compare(b.StartedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	b, _ := json.Marshal(list)
	return string(b)
}

// recordEvidence writes the visit's evidence when it changed: waiting_on, model_wait_since
// and open_calls at once, progress_at with them or, alone, at most once per heartbeatEvery
// of the engine's clock. The loop calls it before every wait, so each event, signal, call
// and tick is followed by one comparison and, rarely, one write.
func (v *visit) recordEvidence(ctx context.Context) {
	now := v.e.clock.Now()
	if wait := v.currentWait(); wait != v.waitingOn {
		v.waitingOn, v.modelWaitSince = wait, time.Time{}
		if wait == store.WaitingOnModel {
			v.modelWaitSince = now
		}
	}
	calls := v.openCallsJSON()
	w := &v.written
	changed := v.waitingOn != w.waitingOn || !v.modelWaitSince.Equal(w.modelWaitSince) || calls != w.openCalls
	progressed := v.progressAt.After(w.progressAt)
	if !changed && (!progressed || now.Sub(w.progressWrote) < heartbeatEvery) {
		return
	}
	ev := store.NodeEvidence{WaitingOn: v.waitingOn, OpenCalls: json.RawMessage(calls)}
	if !v.modelWaitSince.IsZero() {
		at := v.modelWaitSince
		ev.ModelWaitSince = &at
	}
	if progressed {
		at := v.progressAt
		ev.ProgressAt = &at
	}
	if err := store.SetRunNodeEvidence(context.WithoutCancel(ctx), v.e.deps.Pool, v.rn.ID, ev); err != nil {
		v.log.Warn("record liveness evidence failed", "error", err)
		return
	}
	w.waitingOn, w.modelWaitSince, w.openCalls = v.waitingOn, v.modelWaitSince, calls
	if progressed {
		w.progressAt, w.progressWrote = v.progressAt, now
	}
}
