package openclaw

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

func fastRestartWait(t *testing.T) {
	restartWaitBase = 5 * time.Millisecond
	restartWaitCap = 20 * time.Millisecond
	t.Cleanup(func() { restartWaitBase, restartWaitCap = 200*time.Millisecond, 3*time.Second })
}

// A call made while the gateway restarts (connection dropped, reconnects refused for a
// moment, then an UNAVAILABLE refusal) succeeds once the gateway is back.
func TestCallWaitsOutGatewayRestart(t *testing.T) {
	fastRestartWait(t)
	f := newFakeGateway(t, testToken)
	var n atomic.Int32
	f.on("sessions.patch", func(json.RawMessage) (any, *RPCError) {
		if n.Add(1) == 1 {
			return nil, &RPCError{Code: "UNAVAILABLE", Message: "sessions.patch unavailable during gateway restart"}
		}
		return map[string]any{"ok": true}, nil
	})
	f.on("agents.list", func(json.RawMessage) (any, *RPCError) { return map[string]any{"agents": []any{}}, nil })
	gw, _ := dialFake(t, f, DialConfig{})
	ctx := context.Background()

	f.refuseConnects(2, &RPCError{Code: "UNAVAILABLE", Message: "gateway starting; retry shortly", Retryable: true})
	f.dropAll()
	if _, err := gw.Call(ctx, "agents.list", nil); err != nil {
		t.Fatalf("agents.list across the restart: %v", err)
	}
	if _, err := gw.Call(ctx, "sessions.patch", map[string]any{}); err != nil {
		t.Fatalf("sessions.patch during the restart: %v", err)
	}
	if n.Load() != 2 {
		t.Fatalf("sessions.patch attempts = %d, want 2", n.Load())
	}
}

func TestCallDoesNotRetryOtherErrors(t *testing.T) {
	fastRestartWait(t)
	f := newFakeGateway(t, testToken)
	var n atomic.Int32
	f.on("agents.delete", func(json.RawMessage) (any, *RPCError) {
		n.Add(1)
		return nil, &RPCError{Code: "INVALID_REQUEST", Message: "no such agent"}
	})
	f.on("chat.abort", func(json.RawMessage) (any, *RPCError) {
		n.Add(100)
		return nil, &RPCError{Code: "UNAVAILABLE", Message: "busy"}
	})
	gw, _ := dialFake(t, f, DialConfig{})
	for _, m := range []string{"agents.delete", "chat.abort"} {
		if _, err := gw.Call(context.Background(), m, nil); err == nil {
			t.Fatalf("%s succeeded", m)
		}
	}
	if n.Load() != 101 {
		t.Fatalf("attempts = %d, want one each", n.Load())
	}
}
