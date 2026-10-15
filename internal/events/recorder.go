package events

import (
	"context"
	"encoding/json"
	"time"
)

// Appender persists an event and returns its sequence number. The store package
// provides one backed by run_events.
type Appender func(ctx context.Context, runID, typ string, data json.RawMessage) (int64, error)

// Recorder writes an event durably and then fans it out (DESIGN 5.1 rule 6: the DB row
// exists before the SSE event).
type Recorder struct {
	Hub    *Hub
	Append Appender
}

// Emit marshals v, appends it, and publishes. Returns the sequence number.
func (r *Recorder) Emit(ctx context.Context, runID, typ string, v any) (int64, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	seq, err := r.Append(ctx, runID, typ, data)
	if err != nil {
		return 0, err
	}
	r.Hub.Publish(Event{Seq: seq, RunID: runID, Type: typ, Data: data, At: time.Now().UTC()})
	return seq, nil
}
