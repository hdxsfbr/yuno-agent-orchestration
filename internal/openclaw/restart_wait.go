package openclaw

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// A gateway restart (the platform's own recovery restart, or the gateway's) refuses or
// drops calls for a few seconds. Every Call waits it out: a disconnected client, and an
// UNAVAILABLE answer that says the gateway is restarting or starting, are retried with
// backoff until restartWaitMax; anything else is returned at once.
var (
	restartWaitMax  = 60 * time.Second
	restartWaitBase = 200 * time.Millisecond
	restartWaitCap  = 3 * time.Second
)

// transientRefusal reports an UNAVAILABLE refusal the gateway lifts by itself. The
// "recovery restart is required" refusal is not one: only a restart clears it.
func transientRefusal(err error) bool {
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != "UNAVAILABLE" {
		return false
	}
	m := strings.ToLower(rpc.Message)
	if strings.Contains(m, recoveryRestartMarker) {
		return false
	}
	return rpc.Retryable || strings.Contains(m, "restart") || strings.Contains(m, "gateway starting") || strings.Contains(m, "retry shortly")
}

// replaySafe lists the methods that only read: resending one whose request was in
// flight when the connection dropped cannot repeat a side effect.
func replaySafe(method string) bool {
	return strings.HasSuffix(method, ".get") || strings.HasSuffix(method, ".list") ||
		strings.HasSuffix(method, ".status") || strings.HasSuffix(method, ".history")
}

// callWithRestartWait runs attempt until it succeeds, fails with something that is not
// a restart, or the wait budget ends (the last error is returned). A request that may
// have reached the gateway before a disconnect is resent only if the method is read-only.
func callWithRestartWait(ctx context.Context, method string, attempt func() (json.RawMessage, bool, error), waitConnected func(context.Context) error) (json.RawMessage, error) {
	if ctx.Value(recoveryCtxKey{}) == nil {
		waitRecovery(ctx)
	}
	deadline := time.Now().Add(restartWaitMax)
	wait := restartWaitBase
	for {
		out, sent, err := attempt()
		if err == nil {
			return out, nil
		}
		retry := false
		switch {
		case errors.Is(err, ErrDisconnected):
			retry = !sent || replaySafe(method)
		case transientRefusal(err):
			retry = true
		}
		if !retry || ctx.Err() != nil || time.Now().Add(wait).After(deadline) {
			return nil, err
		}
		if errors.Is(err, ErrDisconnected) {
			wctx, cancel := context.WithDeadline(ctx, deadline)
			_ = waitConnected(wctx)
			cancel()
		}
		if sleepCtx(ctx, wait) != nil {
			return nil, err
		}
		if wait *= 2; wait > restartWaitCap {
			wait = restartWaitCap
		}
	}
}
