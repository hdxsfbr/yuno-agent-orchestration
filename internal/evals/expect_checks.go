package evals

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// checkFinalState compares each expect.state entry with the run's final state. Every
// entry is checked and reported, in pointer order.
func checkFinalState(want map[string]any, raw json.RawMessage) []string {
	if len(want) == 0 {
		return nil
	}
	var state any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &state); err != nil {
			return []string{"run state is not valid JSON: " + err.Error()}
		}
	}
	var problems []string
	for _, ptr := range sortedKeys(want) {
		got, ok := workflow.Lookup(state, ptr)
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("state %s is missing, want %s", ptr, compactJSON(want[ptr])))
		case !jsonEqual(want[ptr], got):
			problems = append(problems, fmt.Sprintf("state %s is %s, want %s", ptr, compactJSON(got), compactJSON(want[ptr])))
		}
	}
	return problems
}

// checkToolStatuses compares each expected status list with the statuses of the rows of
// that tool, in trace order: the wanted statuses must appear as a subsequence, each
// matched by a distinct row. An empty list means no row for the tool may exist.
func checkToolStatuses(want map[string][]string, calls []store.ToolCall) []string {
	var problems []string
	for _, tool := range sortedKeys(want) {
		var have []string
		for _, tc := range calls {
			if toolBase(tc.ToolName) == tool {
				have = append(have, tc.Status)
			}
		}
		switch {
		case len(want[tool]) == 0 && len(have) > 0:
			problems = append(problems, fmt.Sprintf("tool %s was called (%s), want no call", tool, strings.Join(have, ", ")))
		case len(want[tool]) > 0 && !isSubsequence(want[tool], have):
			problems = append(problems, fmt.Sprintf("tool %s statuses [%s], want [%s]",
				tool, strings.Join(have, ", "), strings.Join(want[tool], ", ")))
		}
	}
	return problems
}

// checkNeverToolStatuses reports every row whose status the case rules out for its tool.
func checkNeverToolStatuses(never map[string][]string, calls []store.ToolCall) []string {
	var problems []string
	for _, tool := range sortedKeys(never) {
		for _, tc := range calls {
			if toolBase(tc.ToolName) == tool && slices.Contains(never[tool], tc.Status) {
				problems = append(problems, fmt.Sprintf("tool %s has a row with status %s (%s), which the case rules out", tool, tc.Status, tc.ID))
			}
		}
	}
	return problems
}

// isSubsequence reports whether want occurs in have in order, gaps allowed.
func isSubsequence(want, have []string) bool {
	i := 0
	for _, h := range have {
		if i < len(want) && h == want[i] {
			i++
		}
	}
	return i == len(want)
}

// checkApprovalStatuses compares the exact list of approval statuses, oldest request
// first. A nil want is not checked.
func checkApprovalStatuses(want *[]string, approvals []store.Approval) []string {
	if want == nil {
		return nil
	}
	sorted := slices.Clone(approvals)
	slices.SortStableFunc(sorted, func(a, b store.Approval) int { return a.RequestedAt.Compare(b.RequestedAt) })
	have := make([]string, len(sorted))
	for i, a := range sorted {
		have[i] = a.Status
	}
	if slices.Equal(have, *want) {
		return nil
	}
	return []string{fmt.Sprintf("approval statuses [%s], want [%s]", strings.Join(have, ", "), strings.Join(*want, ", "))}
}
