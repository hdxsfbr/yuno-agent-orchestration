package workflow

import (
	"fmt"
	"sort"
	"strings"
)

// Parallel steps (DESIGN 5.26): a fork starts one branch per true edge, every branch
// runs at once and they meet at the fork's join. The region of a fork is the set of nodes
// reachable from its heads (its edges' targets) without passing the join; rule 2 says what
// may be inside and that nothing crosses its border except through the fork and the join.

// MaxBranches is how many outgoing edges a fork may have (DESIGN 5.26 rule 2, D-060).
const MaxBranches = 5

// ForkRegion is the validated branch region of one fork.
type ForkRegion struct {
	Fork string
	Join string
	// Heads are the fork's edges' targets in edge order: one branch each.
	Heads []string
	// Members maps every node of the region to the head of its branch.
	Members map[string]string
	// EdgeIndexes are the fork's edges' positions in Definition.Edges, in order.
	EdgeIndexes []int
	// Checkout is the join's checkout branch (its head), "" when none (rule 10).
	Checkout string
}

// Head returns the branch head of a node of the region ("" outside it).
func (r *ForkRegion) Head(nodeID string) string { return r.Members[nodeID] }

// ForkOf returns the id of the fork that names joinID as its join.
func (d *Definition) ForkOf(joinID string) (string, bool) {
	for _, n := range d.Nodes {
		if n.Kind == KindFork && n.Join == joinID {
			return n.ID, true
		}
	}
	return "", false
}

// Region computes the branch region of a fork on a validated definition (DESIGN 5.26
// rule 2); the engine reads the heads and the members from it. An unknown fork, or one
// without a join node, is an error.
func (d *Definition) Region(forkID string) (*ForkRegion, error) {
	f, ok := d.Node(forkID)
	if !ok || f.Kind != KindFork {
		return nil, fmt.Errorf("fork %s: not a fork node", forkID)
	}
	j, ok := d.Node(f.Join)
	if !ok || j.Kind != KindJoin {
		return nil, fmt.Errorf("fork %s: join %q is not a join node", forkID, f.Join)
	}
	r := &ForkRegion{Fork: f.ID, Join: j.ID, Members: map[string]string{}, Checkout: j.Checkout}
	for i, e := range d.Edges {
		if e.From != f.ID {
			continue
		}
		r.EdgeIndexes = append(r.EdgeIndexes, i)
		if e.To == j.ID || r.Members[e.To] != "" {
			continue
		}
		r.Heads = append(r.Heads, e.To)
		d.walkBranch(r, e.To, nil, nil)
	}
	return r, nil
}

// walkBranch adds to r.Members every node reachable from head through edges and
// on_failure targets without passing the join, and without entering `outside` (the nodes
// before the fork and after the join); the edges that would are reported through leave.
func (d *Definition) walkBranch(r *ForkRegion, head string, outside map[string]bool, leave func(from, path, to string)) {
	stack := []string{head}
	r.Members[head] = head
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		type hop struct{ path, to string }
		var hops []hop
		for i, e := range d.Edges {
			if e.From == cur {
				hops = append(hops, hop{fmt.Sprintf("edges[%d].to", i), e.To})
			}
		}
		if n, ok := d.Node(cur); ok && n.OnFailure != "" {
			hops = append(hops, hop{fmt.Sprintf("nodes[%d].on_failure", d.index(cur)), n.OnFailure})
		}
		for _, h := range hops {
			if h.to == r.Join || h.to == "" {
				continue
			}
			if _, known := d.Node(h.to); !known {
				continue // reported by the structural rules
			}
			if outside[h.to] {
				if leave != nil {
					leave(cur, h.path, h.to)
				}
				continue
			}
			if _, seen := r.Members[h.to]; seen {
				continue
			}
			r.Members[h.to] = head
			stack = append(stack, h.to)
		}
	}
}

func (d *Definition) index(id string) int {
	for i := range d.Nodes {
		if d.Nodes[i].ID == id {
			return i
		}
	}
	return -1
}

// reachableWithout lists the nodes reachable from `from` through edges, on_failure and
// on_reject targets, never expanding the nodes in `stop` (they are listed, their edges are
// not followed) and never listing the nodes in `skip`.
func (d *Definition) reachableWithout(from string, stop, skip map[string]bool) map[string]bool {
	seen := map[string]bool{}
	if skip[from] {
		return seen
	}
	seen[from] = true
	stack := []string{from}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if stop[cur] {
			continue
		}
		var next []string
		for _, e := range d.OutgoingEdges(cur) {
			next = append(next, e.To)
		}
		if n, ok := d.Node(cur); ok {
			next = append(next, n.OnReject, n.OnFailure)
		}
		for _, to := range next {
			if to == "" || seen[to] || skip[to] {
				continue
			}
			if _, known := d.Node(to); !known {
				continue
			}
			seen[to] = true
			stack = append(stack, to)
		}
	}
	return seen
}

// validateForks checks DESIGN 5.26 rule 2 for every fork and join of the definition and
// returns its path-qualified messages. ids are the known node ids (the structural rules
// already reported unknown targets, which are skipped here).
func (d *Definition) validateForks(ids map[string]bool) []string {
	var msgs []string
	forks := map[string]int{}     // fork id → node index
	joinOf := map[string]string{} // join id → the fork that names it
	for i, n := range d.Nodes {
		switch n.Kind {
		case KindFork:
			forks[n.ID] = i
			if n.Checkout != "" {
				msgs = append(msgs, fmt.Sprintf("nodes[%d].checkout: only join nodes have one", i))
			}
			if n.OnFailure != "" {
				msgs = append(msgs, fmt.Sprintf("nodes[%d].on_failure: a fork has none; a failed branch fails the run", i))
			}
		case KindJoin:
			if n.Join != "" {
				msgs = append(msgs, fmt.Sprintf("nodes[%d].join: only fork nodes have one", i))
			}
			if n.OnFailure != "" {
				msgs = append(msgs, fmt.Sprintf("nodes[%d].on_failure: a join has none", i))
			}
		default:
			if n.Join != "" {
				msgs = append(msgs, fmt.Sprintf("nodes[%d].join: only fork nodes have one", i))
			}
			if n.Checkout != "" {
				msgs = append(msgs, fmt.Sprintf("nodes[%d].checkout: only join nodes have one", i))
			}
		}
	}
	if len(forks) == 0 {
		for i, n := range d.Nodes {
			if n.Kind == KindJoin {
				msgs = append(msgs, fmt.Sprintf("nodes[%d]: join '%s' is not named by any fork", i, n.ID))
			}
		}
		return msgs
	}
	start, _ := d.Start()
	for i, n := range d.Nodes {
		if n.Kind != KindFork {
			continue
		}
		if n.Join == "" {
			continue // reported by the kind check
		}
		j, ok := d.Node(n.Join)
		switch {
		case !ok:
			msgs = append(msgs, fmt.Sprintf("nodes[%d].join: unknown node '%s'", i, n.Join))
			continue
		case j.Kind != KindJoin:
			msgs = append(msgs, fmt.Sprintf("nodes[%d].join: '%s' is not a join node", i, n.Join))
			continue
		}
		if first, taken := joinOf[j.ID]; taken {
			msgs = append(msgs, fmt.Sprintf("nodes[%d].join: '%s' is already the join of '%s'", i, j.ID, first))
			continue
		}
		joinOf[j.ID] = n.ID
		msgs = append(msgs, d.validateRegion(i, &d.Nodes[i], j, start, forks)...)
	}
	for i, n := range d.Nodes {
		if n.Kind == KindJoin && joinOf[n.ID] == "" {
			msgs = append(msgs, fmt.Sprintf("nodes[%d]: join '%s' is not named by any fork", i, n.ID))
		}
	}
	return msgs
}

// validateRegion is validateForks for one fork F (node fi) with join J.
func (d *Definition) validateRegion(fi int, f *Node, j *Node, start string, forks map[string]int) []string {
	var msgs []string
	F, J := f.ID, j.ID
	// The fork's edges: 2 to 5, distinct heads, none the join.
	var edges []int
	heads := []string{}
	seenHead := map[string]bool{}
	for k, e := range d.Edges {
		if e.From != F {
			continue
		}
		edges = append(edges, k)
		switch {
		case e.To == J:
			msgs = append(msgs, fmt.Sprintf("edges[%d].to: a branch of '%s' cannot start at its join", k, F))
		case seenHead[e.To]:
			msgs = append(msgs, fmt.Sprintf("edges[%d].to: duplicate branch of '%s'", k, F))
		default:
			if _, known := d.Node(e.To); known {
				seenHead[e.To] = true
				heads = append(heads, e.To)
			}
		}
	}
	if len(edges) < 2 {
		msgs = append(msgs, fmt.Sprintf("nodes[%d]: a fork needs at least two outgoing edges", fi))
	}
	if len(edges) > MaxBranches {
		msgs = append(msgs, fmt.Sprintf("nodes[%d]: a fork may have at most %d outgoing edges", fi, MaxBranches))
	}
	// The join's own fields.
	ji := d.index(J)
	if j.Checkout != "" {
		if !seenHead[j.Checkout] {
			msgs = append(msgs, fmt.Sprintf("nodes[%d].checkout: unknown branch '%s'", ji, j.Checkout))
		} else if !d.Project {
			msgs = append(msgs, fmt.Sprintf("nodes[%d].checkout: the workflow does not work on a project", ji))
		}
	}
	// Outside the region: what is reached from start without taking the fork's edges
	// (before the fork, the fork itself included), and what is reached from the join
	// without taking the fork's edges and without those (after the join).
	stopAtFork := map[string]bool{F: true}
	pre := d.reachableWithout(start, stopAtFork, nil)
	post := d.reachableWithout(J, stopAtFork, pre)
	delete(post, J)
	outside := map[string]bool{}
	for id := range pre {
		outside[id] = true
	}
	for id := range post {
		outside[id] = true
	}
	// The region: every branch from its head, never crossing into outside.
	r := &ForkRegion{Fork: F, Join: J, Members: map[string]string{}}
	twice := map[string]bool{}
	for _, h := range heads {
		if prior, seen := r.Members[h]; seen {
			if !twice[h] {
				twice[h] = true
				msgs = append(msgs, fmt.Sprintf("nodes[%d]: '%s' is reachable from two branches of '%s' ('%s' and '%s'); branches meet only at the join '%s'",
					d.index(h), h, F, prior, h, J))
			}
			continue
		}
		before := map[string]string{}
		for k, v := range r.Members {
			before[k] = v
		}
		d.walkBranch(r, h, outside, func(from, path, to string) {
			msgs = append(msgs, fmt.Sprintf("%s: leaves the branch of '%s' without reaching its join '%s'", path, F, J))
		})
		// A node this branch reached that an earlier branch had already reached is in two
		// branches: walkBranch keeps the first owner, so compare what it would have added.
		for id := range reachedBy(d, h, J, outside) {
			if owner, ok := before[id]; ok && owner != h && !twice[id] {
				twice[id] = true
				msgs = append(msgs, fmt.Sprintf("nodes[%d]: '%s' is reachable from two branches of '%s' ('%s' and '%s'); branches meet only at the join '%s'",
					d.index(id), id, F, owner, h, J))
			}
		}
	}
	// What is inside: agent nodes only.
	members := make([]string, 0, len(r.Members))
	for id := range r.Members {
		members = append(members, id)
	}
	sort.Slice(members, func(a, b int) bool { return d.index(members[a]) < d.index(members[b]) })
	agentIn := map[string]string{} // agent key → head of the first branch using it
	for _, id := range members {
		n, _ := d.Node(id)
		ni := d.index(id)
		switch n.Kind {
		case KindApproval:
			msgs = append(msgs, fmt.Sprintf("nodes[%d]: an approval node cannot be inside a branch of '%s'; put the gate after its join '%s'", ni, F, J))
		case KindFork:
			msgs = append(msgs, fmt.Sprintf("nodes[%d]: a fork cannot be inside a branch of '%s' (forks do not nest)", ni, F))
		case KindJoin:
			msgs = append(msgs, fmt.Sprintf("nodes[%d]: join '%s' cannot be inside a branch of '%s'", ni, id, F))
		case KindEnd:
			msgs = append(msgs, fmt.Sprintf("nodes[%d]: an end node cannot be inside a branch of '%s'", ni, F))
		case KindStart:
			msgs = append(msgs, fmt.Sprintf("nodes[%d]: the start node cannot be inside a branch of '%s'", ni, F))
		case KindAgent:
			key, path := n.AgentID, "agent_id"
			if key == "" {
				key, path = strings.ToLower(n.AgentName), "agent_name"
			}
			if key == "" {
				break
			}
			if first, used := agentIn[key]; used && first != r.Members[id] {
				msgs = append(msgs, fmt.Sprintf("nodes[%d].%s: agent '%s' is used in two branches of '%s' ('%s' and '%s'); branches run at once and share one workspace per agent",
					ni, path, n.AgentID+n.AgentName, F, first, r.Members[id]))
			} else if !used {
				agentIn[key] = r.Members[id]
			}
			// In a project workflow the checkout from before the fork lives in the workspaces
			// of the agents that ran before it; one of them working in a branch the join does
			// not name would overwrite it with that branch's tree (rule 10, review M5).
			if d.Project && j.Checkout != r.Members[id] && agentBefore(d, pre, key) {
				msgs = append(msgs, fmt.Sprintf("nodes[%d].%s: agent '%s' runs before the fork '%s' and inside its branch '%s', which is not the join's checkout; the checkout from before the fork would be lost",
					ni, path, n.AgentID+n.AgentName, F, r.Members[id]))
			}
		}
		for k, from := range n.InputFrom {
			if owner, in := r.Members[from]; in && owner != r.Members[id] {
				msgs = append(msgs, fmt.Sprintf("nodes[%d].input_from[%d]: '%s' is in another branch of '%s'", ni, k, from, F))
			}
		}
	}
	// Nothing enters the region, or reaches the join, from outside.
	for k, e := range d.Edges {
		if e.From == F || r.Members[e.From] != "" {
			continue
		}
		if r.Members[e.To] != "" {
			msgs = append(msgs, fmt.Sprintf("edges[%d].to: enters a branch of '%s' from outside", k, F))
		} else if e.To == J {
			msgs = append(msgs, fmt.Sprintf("edges[%d].to: join '%s' is reached only through the branches of '%s'", k, J, F))
		}
	}
	for i, n := range d.Nodes {
		if n.ID == F || r.Members[n.ID] != "" {
			continue
		}
		for field, to := range map[string]string{"on_failure": n.OnFailure, "on_reject": n.OnReject} {
			if to == "" {
				continue
			}
			if r.Members[to] != "" {
				msgs = append(msgs, fmt.Sprintf("nodes[%d].%s: enters a branch of '%s' from outside", i, field, F))
			} else if to == J {
				msgs = append(msgs, fmt.Sprintf("nodes[%d].%s: join '%s' is reached only through the branches of '%s'", i, field, J, F))
			}
		}
	}
	return msgs
}

// agentBefore reports whether an agent (by id, or by lower-cased name) runs on a node
// reached before the fork.
func agentBefore(d *Definition, pre map[string]bool, key string) bool {
	for _, n := range d.Nodes {
		if n.Kind != KindAgent || !pre[n.ID] {
			continue
		}
		k := n.AgentID
		if k == "" {
			k = strings.ToLower(n.AgentName)
		}
		if k == key {
			return true
		}
	}
	return false
}

// reachedBy lists the nodes a branch from head reaches without passing the join or
// entering outside (the same walk as walkBranch, on a scratch region).
func reachedBy(d *Definition, head, join string, outside map[string]bool) map[string]string {
	r := &ForkRegion{Join: join, Members: map[string]string{}}
	d.walkBranch(r, head, outside, nil)
	return r.Members
}
