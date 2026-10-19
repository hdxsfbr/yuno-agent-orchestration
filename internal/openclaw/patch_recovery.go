package openclaw

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"
)

// The pinned gateway (2026.10.3) can persist a config.patch and then refuse it, and
// every later one, with UNAVAILABLE "...a recovery restart is required..." until it is
// restarted. The patch did persist, so the platform restarts the gateway, waits for it,
// and confirms the patched paths before treating the call as done.
const recoveryRestartMarker = "recovery restart is required"

var (
	recoveryMu       sync.Mutex // one restart in flight; concurrent callers wait on it
	lastRecoveryDone time.Time  // when the last recovery finished
	recoveryTimeout  = 60 * time.Second
	recoveryBackoff  = 250 * time.Millisecond
	recoveryBackoffM = 3 * time.Second

	inFlightMu sync.Mutex
	inFlight   chan struct{} // closed when the recovery in flight ends; nil when none is
)

// recoveryCtxKey marks the calls the recovery itself makes.
type recoveryCtxKey struct{}

// waitRecovery blocks while a recovery restart is in flight, so other callers wait on it
// instead of failing against the restarting gateway.
func waitRecovery(ctx context.Context) {
	inFlightMu.Lock()
	done := inFlight
	inFlightMu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func needsRecoveryRestart(err error) bool {
	var rpc *RPCError
	return errors.As(err, &rpc) && rpc.Code == "UNAVAILABLE" &&
		strings.Contains(strings.ToLower(rpc.Message), recoveryRestartMarker)
}

// recoverPatch restarts the gateway unless a recovery finished after started (another
// caller's restart covers this patch), waits until config.get answers, and confirms raw
// is in the live config.
func recoverPatch(ctx context.Context, c Client, started time.Time, raw string) error {
	recoveryMu.Lock()
	defer recoveryMu.Unlock()
	done := make(chan struct{})
	inFlightMu.Lock()
	inFlight = done
	inFlightMu.Unlock()
	defer func() {
		inFlightMu.Lock()
		inFlight = nil
		inFlightMu.Unlock()
		close(done)
	}()
	ctx = context.WithValue(ctx, recoveryCtxKey{}, true) // our own calls do not wait on us
	if !lastRecoveryDone.After(started) {
		slog.Info("openclaw gateway restart after config.patch", "reason", "recovery restart required")
		if _, err := GatewayRestart(ctx, c, "recovery restart after config.patch"); err != nil {
			return err
		}
		// Give the gateway time to start closing connections before polling it.
		if err := sleepCtx(ctx, recoveryBackoff); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(recoveryTimeout)
	wait := recoveryBackoff
	for {
		cfg, err := ConfigGet(ctx, c)
		if err == nil {
			lastRecoveryDone = time.Now()
			var patch, live map[string]any
			if json.Unmarshal([]byte(raw), &patch) != nil || json.Unmarshal(cfg.Config, &live) != nil {
				return errors.New("config unreadable after restart")
			}
			if !containsPatch(live, patch) {
				return errors.New("patched paths missing after restart")
			}
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return err
		}
		if wait *= 2; wait > recoveryBackoffM {
			wait = recoveryBackoffM
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// containsPatch reports whether live holds every value patch sets (null: absent).
func containsPatch(live, patch map[string]any) bool {
	for k, pv := range patch {
		lv, ok := live[k]
		switch pm := pv.(type) {
		case nil:
			if ok {
				return false
			}
		case map[string]any:
			lm, _ := lv.(map[string]any)
			if !ok || !containsPatch(lm, pm) {
				return false
			}
		default:
			if !ok || !reflect.DeepEqual(lv, pv) {
				return false
			}
		}
	}
	return true
}
