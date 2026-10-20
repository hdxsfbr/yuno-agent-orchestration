package workflow

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

// CheckWavesPartition is the name of the node check that makes a waves node's output a
// partition of the tickets of an earlier node (DESIGN 5.28 rule 4).
const CheckWavesPartition = "waves_partition"

// The caps of a plan (DESIGN 5.29 rule 2). The waves check applies them before the plan
// is approved, and publish_tickets applies them again, so an approved plan always publishes.
const (
	MaxPlanTickets = 100
	MaxPlanWaves   = 30
)

// KnownChecks are the names a node's `checks` may hold; save refuses any other.
var KnownChecks = map[string]bool{CheckWavesPartition: true}

// RunCheck applies one named check to a node's finish result. state is the run state
// (node id → output); it returns "" when the result passes, otherwise a message the
// agent can act on. The caller has validated the name (KnownChecks).
func (n *Node) RunCheck(name string, state map[string]json.RawMessage, result json.RawMessage) string {
	switch name {
	case CheckWavesPartition:
		return checkWavesPartition(n, state, result)
	}
	return "unknown check '" + name + "'"
}

// validateChecks reports unknown check names and checks the node cannot be given.
func (n *Node) validateChecks(prefix string) []string {
	var msgs []string
	for j, c := range n.Checks {
		if !KnownChecks[c] {
			msgs = append(msgs, fmt.Sprintf("%s.checks[%d]: unknown check '%s'", prefix, j, c))
			continue
		}
		if n.Kind != KindAgent {
			msgs = append(msgs, fmt.Sprintf("%s.checks[%d]: only agent nodes have checks", prefix, j))
		}
		if c == CheckWavesPartition && len(n.InputFrom) == 0 {
			msgs = append(msgs, fmt.Sprintf("%s.checks[%d]: %s needs input_from to name the node whose output lists the tickets", prefix, j, c))
		}
	}
	return msgs
}

type ticketOut struct {
	ID    string   `json:"id"`
	Files []string `json:"files"`
}

// checkWavesPartition: the result's `waves` ([{n, tickets: [id]}]) must number the waves
// 1..k without gaps, put every ticket of the input_from node's `tickets` in exactly one
// wave, name no unknown ticket, and keep tickets that list a common file apart.
func checkWavesPartition(n *Node, state map[string]json.RawMessage, result json.RawMessage) string {
	var tickets []ticketOut
	found := false
	for _, from := range n.InputFrom {
		var o struct {
			Tickets *[]ticketOut `json:"tickets"`
		}
		if json.Unmarshal(state[from], &o) == nil && o.Tickets != nil {
			tickets, found = *o.Tickets, true
			break
		}
	}
	if !found {
		return "waves check: no tickets in the output of " + strings.Join(n.InputFrom, ", ")
	}
	seen := map[string]bool{}
	for _, t := range tickets {
		if strings.TrimSpace(t.ID) == "" {
			return "waves check: a ticket has no id"
		}
		if seen[t.ID] {
			return fmt.Sprintf("waves check: ticket id %s is used by more than one ticket; ids must be unique", t.ID)
		}
		seen[t.ID] = true
	}
	var res struct {
		Waves []struct {
			N       int      `json:"n"`
			Tickets []string `json:"tickets"`
		} `json:"waves"`
	}
	if err := json.Unmarshal(result, &res); err != nil {
		return "waves check: waves must be a list of {n, tickets}"
	}
	if len(tickets) > MaxPlanTickets {
		return fmt.Sprintf("waves check: the plan has %d tickets; at most %d: merge tickets into larger slices", len(tickets), MaxPlanTickets)
	}
	if len(res.Waves) > MaxPlanWaves {
		return fmt.Sprintf("waves check: the plan has %d waves; at most %d: put more independent tickets in the same wave", len(res.Waves), MaxPlanWaves)
	}
	files := map[string][]string{}
	for _, t := range tickets {
		for _, f := range t.Files {
			files[t.ID] = append(files[t.ID], path.Clean(strings.TrimSpace(f)))
		}
	}
	nums := make([]int, 0, len(res.Waves))
	for _, w := range res.Waves {
		nums = append(nums, w.N)
	}
	sort.Ints(nums)
	for i, v := range nums {
		if v != i+1 {
			return fmt.Sprintf("waves must be numbered 1 to %d without gaps or repeats, got %v", len(nums), nums)
		}
	}
	placed := map[string]int{}
	for _, w := range res.Waves {
		owner := map[string]string{} // file → ticket in this wave
		for _, id := range w.Tickets {
			if !hasTicket(tickets, id) {
				return fmt.Sprintf("wave %d names ticket %s, which is not in the ticket list", w.N, id)
			}
			if prev, dup := placed[id]; dup {
				return fmt.Sprintf("ticket %s is in wave %d and wave %d; each ticket belongs to exactly one wave", id, prev, w.N)
			}
			placed[id] = w.N
			for _, f := range files[id] {
				if other, clash := owner[f]; clash && other != id {
					return fmt.Sprintf("tickets %s and %s are both in wave %d and both list %s; tickets that share a file go in different waves", other, id, w.N, f)
				}
				owner[f] = id
			}
		}
	}
	var missing []string
	for _, t := range tickets {
		if _, ok := placed[t.ID]; !ok {
			missing = append(missing, t.ID)
		}
	}
	if len(missing) > 0 {
		return "tickets not placed in any wave: " + strings.Join(missing, ", ")
	}
	return ""
}

func hasTicket(ts []ticketOut, id string) bool {
	for _, t := range ts {
		if t.ID == id {
			return true
		}
	}
	return false
}
