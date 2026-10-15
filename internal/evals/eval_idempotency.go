package evals

import (
	"fmt"
	"sort"
	"strings"

	"yuno/internal/store"
)

// callGroup is the set of side-effecting calls that share one node visit, one tool and
// one input: DESIGN 5.4 gives them one idempotency key.
type callGroup struct {
	node, tool, input string
	calls             []store.ToolCall
}

func (g callGroup) label() string {
	return fmt.Sprintf("%s in node visit %s", g.tool, g.node)
}

// evalIdempotency checks DESIGN 5.4 on the trace. Within the run, calls with equal
// (node visit, tool, input) share one idempotency key and produce exactly one
// side_effects row: the first executed call made the effect, every later executed call
// returned the stored result and says so (output.replayed). Calls from different visits
// or with different inputs never share a key, and every side effect on record belongs
// to a call that executed.
func evalIdempotency(_ Case, t store.Trace) EvalResult {
	res := EvalResult{Name: EvalIdempotency}
	if t.Run == nil {
		res.Detail = "trace has no run"
		return res
	}

	var order []string
	groups := map[string]*callGroup{}
	calls := 0
	for _, tc := range t.ToolCalls {
		if !isSideEffecting(tc) {
			continue
		}
		calls++
		in := canonicalJSON(tc.Input)
		id := tc.RunNodeID + "\x00" + toolBase(tc.ToolName) + "\x00" + in
		g, ok := groups[id]
		if !ok {
			g = &callGroup{node: tc.RunNodeID, tool: toolBase(tc.ToolName), input: in}
			groups[id] = g
			order = append(order, id)
		}
		g.calls = append(g.calls, tc)
	}

	effects := map[string][]store.SideEffect{}
	for _, se := range t.SideEffects {
		effects[se.IdempotencyKey] = append(effects[se.IdempotencyKey], se)
		if se.RunID != "" && se.RunID != t.Run.ID {
			return failed(res, fmt.Sprintf("side effect %s belongs to run %s, not %s", shortKey(se.IdempotencyKey), se.RunID, t.Run.ID))
		}
	}

	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	keyOwner := map[string]string{} // key -> label of the group that holds it
	executedKeys := map[string]bool{}
	made, replays := 0, 0

	for _, id := range order {
		g := groups[id]
		key, ok := groupKey(g, add)
		if !ok {
			continue
		}
		if key != "" {
			if prev, dup := keyOwner[key]; dup {
				add("%s and %s share idempotency key %s", prev, g.label(), shortKey(key))
			}
			keyOwner[key] = g.label()
		}
		var executed []store.ToolCall
		for _, tc := range g.calls {
			if tc.Status == store.ToolExecuted {
				executed = append(executed, tc)
			}
		}
		if len(executed) == 0 {
			continue
		}
		if key == "" {
			add("%s executed without an idempotency key", g.label())
			continue
		}
		executedKeys[key] = true

		fresh := 0
		var original *store.ToolCall
		for i, tc := range executed {
			if wasReplayed(tc) {
				replays++
				continue
			}
			fresh++
			if original == nil {
				original = &executed[i]
			}
		}
		switch {
		case fresh == 0:
			add("%s: every executed call is marked replayed, none made the effect", g.label())
		case fresh > 1:
			add("%s: %d executed calls with the same input each made the effect, only the first may", g.label(), fresh)
		case wasReplayed(executed[0]):
			add("%s: the first executed call is marked replayed but a later one made the effect", g.label())
		}
		rows := effects[key]
		switch len(rows) {
		case 0:
			add("%s executed but has no side_effects row for key %s", g.label(), shortKey(key))
		case 1:
			made++
			if original != nil && rows[0].ToolCallID != "" && rows[0].ToolCallID != original.ID {
				add("%s: the side effect was made by tool call %s but the original executed call is %s",
					g.label(), rows[0].ToolCallID, original.ID)
			}
		default:
			add("%s has %d side_effects rows for key %s, want 1", g.label(), len(rows), shortKey(key))
		}
	}

	var orphans []string
	for key := range effects {
		if !executedKeys[key] {
			orphans = append(orphans, key)
		}
	}
	sort.Strings(orphans)
	for _, key := range orphans {
		add("side effect %s has no executed tool call", shortKey(key))
	}

	if len(problems) > 0 {
		res.Detail = strings.Join(problems, "; ")
		return res
	}
	res.Passed = true
	if calls == 0 && len(t.SideEffects) == 0 {
		res.Detail = "no side-effecting tool calls"
		return res
	}
	res.Detail = fmt.Sprintf("%d side-effecting call(s) in %d group(s): %d effect(s) made, %d replayed", calls, len(order), made, replays)
	return res
}

// groupKey returns the one idempotency key the calls of a group share. ok is false when
// the calls carry more than one key (reported through add).
func groupKey(g *callGroup, add func(string, ...any)) (key string, ok bool) {
	keys := map[string]bool{}
	for _, tc := range g.calls {
		if tc.IdempotencyKey != nil && *tc.IdempotencyKey != "" {
			keys[*tc.IdempotencyKey] = true
		}
	}
	switch len(keys) {
	case 0:
		return "", true
	case 1:
		for k := range keys {
			return k, true
		}
	}
	add("%s: calls with the same input carry different idempotency keys", g.label())
	return "", false
}

func failed(res EvalResult, detail string) EvalResult {
	res.Detail = detail
	return res
}

// shortKey shortens a hex key for messages.
func shortKey(key string) string {
	if len(key) > 12 {
		return key[:12]
	}
	return key
}
