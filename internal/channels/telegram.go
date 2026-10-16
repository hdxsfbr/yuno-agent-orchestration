// Package channels keeps the gateway's channel configuration in line with the
// platform's bindings (DESIGN 5.8): which platform agent answers the Telegram channel,
// and that nothing else does (D-025).
package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// DMScope gives every Telegram peer its own session with the bound agent, keyed
// agent:<id>:telegram:direct:<user id> (DESIGN 5.8 rule 3).
const DMScope = "per-channel-peer"

// ApplyTimeout bounds one apply (config.get, config.patch, gateway.restart.request).
const ApplyTimeout = 30 * time.Second

// applyMu serialises this process's read-modify-write cycles on the gateway config.
var applyMu sync.Mutex

// ApplyResult says what applying a binding changed on the gateway.
type ApplyResult struct {
	// Changed is true when the config was patched.
	Changed bool `json:"changed"`
	// Restart is the gateway restart status ("scheduled", "deferred", "coalesced") when
	// the patch asked for one, "" otherwise.
	Restart string `json:"restart,omitempty"`
}

// telegramBinding is the bindings[] entry the platform owns [probed schema].
func telegramBinding(openclawAgentID string) map[string]any {
	return map[string]any{
		"agentId": openclawAgentID,
		"match":   map[string]any{"channel": store.ChannelTelegram},
		"session": map[string]any{"dmScope": DMScope},
	}
}

// TelegramPatch computes the config patch that routes the Telegram channel to
// openclawAgentID, or closes it to direct messages when the id is "" (D-025), and the
// replacePaths it needs. Bindings for other channels are kept. The patch is nil when
// the config already says so.
func TelegramPatch(cfg map[string]any, openclawAgentID string) (map[string]any, []string) {
	var kept, ours []any
	existing, _ := cfg["bindings"].([]any)
	for _, b := range existing {
		if isTelegramBinding(b) {
			ours = append(ours, b)
			continue
		}
		kept = append(kept, b)
	}
	var want []any
	if openclawAgentID != "" {
		want = []any{telegramBinding(openclawAgentID)}
	}
	patch := map[string]any{}
	var replace []string
	if !sameJSON(ours, want) {
		patch["bindings"] = append(append([]any{}, kept...), want...)
		replace = append(replace, "bindings")
	}
	// The DM policy only exists to be switched when the channel is configured; an
	// install without Telegram gets no channel block from the platform.
	if tg, ok := nested(cfg, "channels", "telegram"); ok {
		policy := openclaw.TelegramDMClosed
		if openclawAgentID != "" {
			policy = openclaw.TelegramDMOpen
		}
		set := map[string]any{}
		if tg["dmPolicy"] != policy {
			set["dmPolicy"] = policy
		}
		if tg["groupPolicy"] != "disabled" {
			set["groupPolicy"] = "disabled"
		}
		if len(set) > 0 {
			patch["channels"] = map[string]any{"telegram": set}
		}
	}
	if len(patch) == 0 {
		return nil, nil
	}
	return patch, replace
}

func isTelegramBinding(b any) bool {
	m, _ := b.(map[string]any)
	match, _ := m["match"].(map[string]any)
	return match["channel"] == store.ChannelTelegram
}

func nested(m map[string]any, path ...string) (map[string]any, bool) {
	cur := m
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// sameJSON compares two decoded JSON values, treating nil and empty arrays alike.
func sameJSON(a, b []any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	var x, y any
	_ = json.Unmarshal(ab, &x)
	_ = json.Unmarshal(bb, &y)
	return reflect.DeepEqual(x, y)
}

// ApplyTelegram makes the gateway route Telegram to openclawAgentID ("" closes the
// channel to DMs) and, when that changed the config, asks the gateway for its safe
// restart: a bindings patch was observed not to reroute the next DM (DESIGN 5.8 rule
// 3), and the restart re-reads the config in place without touching the container.
func ApplyTelegram(ctx context.Context, gw openclaw.Client, openclawAgentID, reason string) (ApplyResult, error) {
	if gw == nil {
		return ApplyResult{}, errors.New("no gateway connection")
	}
	applyMu.Lock()
	defer applyMu.Unlock()
	var res ApplyResult
	// The materialiser patches the same config; a lost baseHash race is retried.
	for attempt := 0; ; attempt++ {
		cur, err := openclaw.ConfigGet(ctx, gw)
		if err != nil {
			return res, fmt.Errorf("config.get: %w", err)
		}
		var cfg map[string]any
		if err := json.Unmarshal(cur.Config, &cfg); err != nil {
			return res, fmt.Errorf("config.get: decode: %w", err)
		}
		patch, replace := TelegramPatch(cfg, openclawAgentID)
		if patch == nil {
			return res, nil
		}
		raw, _ := json.Marshal(patch)
		_, err = openclaw.ConfigPatch(ctx, gw, string(raw), cur.Hash, replace)
		if err != nil && attempt < 2 && isBaseHashConflict(err) {
			continue
		}
		if err != nil {
			return res, fmt.Errorf("config.patch: %w", err)
		}
		break
	}
	res.Changed = true
	rr, err := openclaw.GatewayRestart(ctx, gw, reason)
	if err != nil {
		return res, fmt.Errorf("gateway.restart.request: %w", err)
	}
	res.Restart = rr.Status
	return res, nil
}

func isBaseHashConflict(err error) bool {
	var re *openclaw.RPCError
	return errors.As(err, &re) && (strings.Contains(re.Message, "baseHash") || strings.Contains(re.Message, "config changed since last load"))
}

// BoundOpenClawAgent returns the OpenClaw id of the agent bound to Telegram, "" when
// none is (or the bound agent was never materialised, which the platform treats as
// unbound: the channel stays closed).
func BoundOpenClawAgent(ctx context.Context, pool *pgxpool.Pool) (string, *store.ChannelBinding, error) {
	b, err := store.GetChannelBinding(ctx, pool, store.ChannelTelegram)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if b.OpenClawAgentID == nil || *b.OpenClawAgentID == "" {
		return "", b, nil
	}
	return *b.OpenClawAgentID, b, nil
}

// Reassert applies the stored binding to the gateway (DESIGN 5.8 rule 3): at every api
// start and after every reconnect, so a gateway whose config was rewritten (a fresh
// state directory, a baseline change) never loses it, and a channel with no binding
// stays closed.
func Reassert(ctx context.Context, pool *pgxpool.Pool, gw openclaw.Client, log *slog.Logger) (ApplyResult, error) {
	ocID, b, err := BoundOpenClawAgent(ctx, pool)
	if err != nil {
		return ApplyResult{}, err
	}
	if b != nil && ocID == "" {
		log.Warn("telegram binding names an agent that is not materialised; the channel stays closed", "agent_id", b.AgentID)
	}
	res, err := ApplyTelegram(ctx, gw, ocID, "yuno: re-assert the telegram binding")
	if err != nil {
		return res, err
	}
	if res.Changed {
		log.Info("telegram binding re-asserted on the gateway", "openclaw_agent_id", ocID, "restart", res.Restart)
	}
	if ocID == "" {
		log.Info("telegram channel closed to direct messages: no agent is bound")
	}
	return res, nil
}

// Keeper re-asserts the binding at start and after every gateway reconnect.
type Keeper struct {
	Pool    *pgxpool.Pool
	Gateway openclaw.Client
	Log     *slog.Logger
	// StateDir is the gateway state directory; when set, the bound agent's MEMORY.md is
	// rendered after each re-assert.
	StateDir string
	// Backoff bounds the retry of a failed re-assert (1 s doubling to 30 s by default).
	BackoffBase, BackoffMax time.Duration
}

// Run blocks until ctx ends.
func (k *Keeper) Run(ctx context.Context) {
	if k.Gateway == nil || k.Pool == nil {
		return
	}
	log := k.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	base, maxWait := k.BackoffBase, k.BackoffMax
	if base <= 0 {
		base = time.Second
	}
	if maxWait <= 0 {
		maxWait = 30 * time.Second
	}
	events, unsub := k.Gateway.Subscribe(64)
	defer unsub()
	due := true // re-assert now
	wait := base
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	defer retry.Stop()
	for {
		if due {
			due = false
			actx, cancel := context.WithTimeout(ctx, ApplyTimeout)
			_, err := Reassert(actx, k.Pool, k.Gateway, log)
			if err == nil {
				k.renderBoundMemory(actx, log)
			}
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Warn("telegram binding re-assert failed", "error", err, "retry_in", wait.String())
				retry.Reset(wait)
				wait = min(wait*2, maxWait)
			} else {
				wait = base
			}
		}
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Name == openclaw.EventReconnected {
				due = true
			}
		case <-retry.C:
			due = true
		}
	}
}

// renderBoundMemory writes the bound agent's MEMORY.md from its facts: a conversation
// reaches the agent without passing through the engine, which renders the file only
// before a node visit (DESIGN 5.10 rule 5). Failures are logged.
func (k *Keeper) renderBoundMemory(ctx context.Context, log *slog.Logger) {
	if k.StateDir == "" {
		return
	}
	b, err := store.GetChannelBinding(ctx, k.Pool, store.ChannelTelegram)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	var a *store.Agent
	if err == nil {
		a, err = store.GetAgent(ctx, k.Pool, b.AgentID)
	}
	if err == nil {
		err = openclaw.RenderMemory(ctx, k.Pool, k.StateDir, a, time.Now().UTC())
	}
	if err != nil {
		log.Warn("render the bound agent's MEMORY.md failed", "error", err)
	}
}
