package tools

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"yuno/internal/store"
)

// Follow-up tickets (DESIGN 5.29 rule 16): a revision that changes the text of a ticket
// whose work is merged or closed leaves that ticket and its issue as they are, and the change
// becomes a follow-up ticket with an issue of its own. The Planner may name the follow-up
// itself ("Follow-up to T<k>: ..."); otherwise publish_tickets adds one.

// ticketFollowUpPrefix starts a follow-up ticket's title, the Planner's and the platform's alike.
const ticketFollowUpPrefix = "Follow-up to "

// followUp is a follow-up ticket of a revision.
type followUp struct {
	id      string           // the follow-up's ticket id
	of      store.PlanTicket // the merged or closed ticket it follows
	revised planTicketIn     // the revised text of that ticket
	hash    string           // ticketContentHash of the revised text
	wave    int
	// planner is set when the Planner named the follow-up itself: its own text is used.
	planner bool
	// record is set when the follow-up carries this text for the first time (a plan event
	// follow_up records it); reused when it is an open follow-up of an earlier revision.
	record, reused bool
}

// isFollowUpOf reports whether a title names a follow-up of the ticket id.
func isFollowUpOf(title, id string) bool {
	p := strings.ToLower(ticketFollowUpPrefix + id)
	t := strings.ToLower(strings.TrimSpace(title))
	if !strings.HasPrefix(t, p) {
		return false
	}
	rest := []rune(t[len(p):])
	return len(rest) == 0 || !unicode.IsLetter(rest[0]) && !unicode.IsDigit(rest[0])
}

// textChanged reports whether the revised ticket's text differs from what its issue was
// written from: by the recorded content hash, or by title when none was recorded.
func textChanged(prev store.PlanTicket, hashes map[string]string, feature string, t planTicketIn) bool {
	if h, ok := hashes[t.ID]; ok {
		return h != ticketContentHash(feature, t)
	}
	return strings.TrimSpace(prev.Title) != strings.TrimSpace(t.Title)
}

// ticketNumber is n of an id T<n>.
func ticketNumber(id string) (int, bool) {
	if !strings.HasPrefix(id, "T") {
		return 0, false
	}
	n, err := strconv.Atoi(id[1:])
	return n, err == nil && n > 0
}

// planFollowUps lists the follow-ups of a revision, in the plan's order: one for each ticket
// whose work is merged or closed and whose text the revision changes. A change already
// carried by a follow-up (of an earlier revision or a retried visit) adds nothing new; an
// open follow-up of the ticket takes a further change; otherwise a new ticket gets the next
// free id T<n> and goes in the current wave, or the next one while the current wave is being
// shipped, and later still while a ticket of that wave lists one of its files (a wave builds
// in parallel). shipLive says the current wave's ship run is live.
func planFollowUps(rows []store.PlanTicket, hashes map[string]string, links []store.FollowUp, plan *publishPlan, shipLive bool) ([]followUp, error) {
	prev := map[string]store.PlanTicket{}
	next := 0
	bump := func(id string) {
		if n, ok := ticketNumber(id); ok && n > next {
			next = n
		}
	}
	for _, t := range rows {
		prev[t.TicketID] = t
		bump(t.TicketID)
	}
	named := map[string]planTicketIn{}
	files := map[int][]string{} // the files of the open tickets per wave
	for i, w := range plan.waves {
		for _, t := range w {
			named[t.ID] = t
			bump(t.ID)
			if p, ok := prev[t.ID]; !ok || !settled(p) {
				files[i+1] = append(files[i+1], t.Files...)
			}
		}
	}
	carries := map[string]string{} // follow-up → the hash of the text it carries (last record)
	byOrigin := map[string][]string{}
	for _, l := range links {
		if _, seen := carries[l.TicketID]; !seen {
			byOrigin[l.Follows] = append(byOrigin[l.Follows], l.TicketID)
		}
		carries[l.TicketID] = l.Hash
	}
	first := currentWaveOf(rows)
	if shipLive {
		first++
	}
	overlaps := func(wave int, fs []string) bool {
		for _, f := range fs {
			if slices.Contains(files[wave], f) {
				return true
			}
		}
		return false
	}

	var out []followUp
	for _, w := range plan.waves {
		for _, t := range w {
			p, ok := prev[t.ID]
			if !ok || !settled(p) || !textChanged(p, hashes, plan.feature, t) {
				continue
			}
			fu := followUp{of: p, revised: t, hash: ticketContentHash(plan.feature, t)}
			// A follow-up already carries this text: the Planner's, or one the platform added.
			carried := false
			for _, id := range byOrigin[t.ID] {
				if carries[id] != fu.hash {
					continue
				}
				if u, isNamed := named[id]; isNamed {
					if !isFollowUpOf(u.Title, t.ID) {
						continue // the Planner gave the id to another ticket
					}
					carried = true
					break
				}
				if r, has := prev[id]; has {
					carried = true
					if !settled(r) {
						fu.id, fu.wave = id, r.Wave
					}
					break
				}
			}
			if carried {
				if fu.id != "" {
					files[fu.wave] = append(files[fu.wave], t.Files...)
					out = append(out, fu)
				}
				continue
			}
			// The Planner named a follow-up of its own that is not merged yet.
			for _, u := range planTickets(plan) {
				if u.ID == t.ID || !isFollowUpOf(u.Title, t.ID) {
					continue
				}
				if r, has := prev[u.ID]; has && settled(r) {
					continue
				}
				fu.id, fu.planner, fu.record = u.ID, true, true
				break
			}
			if fu.planner {
				out = append(out, fu)
				continue
			}
			// An open follow-up of an earlier revision takes the change.
			open := ""
			for _, id := range byOrigin[t.ID] {
				if r, has := prev[id]; has && !settled(r) {
					if _, isNamed := named[id]; !isNamed {
						open = id
					}
				}
			}
			if open != "" {
				fu.id, fu.wave, fu.record, fu.reused = open, prev[open].Wave, true, true
				files[fu.wave] = append(files[fu.wave], t.Files...)
				out = append(out, fu)
				continue
			}
			next++
			fu.id, fu.wave, fu.record = "T"+strconv.Itoa(next), first, true
			for overlaps(fu.wave, t.Files) {
				fu.wave++
			}
			if fu.wave > MaxPlanWaves {
				return nil, &ModelError{Message: fmt.Sprintf("publish_tickets: the follow-up of %s would need wave %d; a plan has at most %d waves",
					t.ID, fu.wave, MaxPlanWaves)}
			}
			files[fu.wave] = append(files[fu.wave], t.Files...)
			out = append(out, fu)
		}
	}
	return out, nil
}

// planTickets is every ticket of the plan in wave order.
func planTickets(plan *publishPlan) []planTicketIn {
	var all []planTicketIn
	for _, w := range plan.waves {
		all = append(all, w...)
	}
	return all
}

// withFollowUps is the plan with the platform's follow-ups placed in their waves (the
// Planner's are in it already). Their text is completed when the issue is written.
func withFollowUps(plan *publishPlan, fus []followUp) *publishPlan {
	out := &publishPlan{feature: plan.feature, revise: plan.revise, count: plan.count}
	for _, w := range plan.waves {
		out.waves = append(out.waves, slices.Clone(w))
	}
	for _, fu := range fus {
		if fu.planner {
			continue
		}
		for len(out.waves) < fu.wave {
			out.waves = append(out.waves, nil)
		}
		out.waves[fu.wave-1] = append(out.waves[fu.wave-1], fu.ticket("", nil))
		out.count++
	}
	return out
}

// followUpSentences are the approval text's lines on follow-ups: "T8 is merged; its change
// becomes follow-up ticket T21 (wave 3)".
func followUpSentences(fus []followUp) []string {
	var out []string
	for _, fu := range fus {
		if fu.planner || !fu.record {
			continue
		}
		state := "merged"
		if fu.of.Status == store.TicketClosed {
			state = "closed"
		}
		if fu.reused {
			out = append(out, fmt.Sprintf("%s is %s; its change goes to its follow-up ticket %s", fu.of.TicketID, state, fu.id))
		} else {
			out = append(out, fmt.Sprintf("%s is %s; its change becomes follow-up ticket %s (wave %d)", fu.of.TicketID, state, fu.id, fu.wave))
		}
	}
	return out
}

// ticket is the follow-up's ticket: the revised text of the ticket it follows, opened by a
// paragraph that links the merge request and the old issue and says what changed against the
// merged version. old is the old issue's body ("" when it was not found); mr is the followed
// ticket's merge request record, nil when it has none.
func (fu followUp) ticket(old string, mr *store.MergeRequest) planTicketIn {
	t := fu.revised
	k := fu.of.TicketID
	var b strings.Builder
	switch {
	case mr != nil && mr.State == store.MergeRequestMerged && mr.URL != "":
		fmt.Fprintf(&b, "Follow-up to %s, which is merged (%s: %s).", k, mr.Label(), mr.URL)
	case fu.of.Status == store.TicketMerged || mr != nil && mr.State == store.MergeRequestMerged:
		fmt.Fprintf(&b, "Follow-up to %s, which is merged.", k)
	default:
		fmt.Fprintf(&b, "Follow-up to %s, which is closed.", k)
	}
	b.WriteString(" The plan was revised after that; this ticket carries the change.")
	if fu.of.IssueURL != "" {
		fmt.Fprintf(&b, " The issue of %s (%s) keeps the text it was built from.", k, fu.of.IssueURL)
	}
	b.WriteString("\n\nWhat changed against that version:\n")
	var lines []string
	if before := strings.TrimSpace(fu.of.Title); before != strings.TrimSpace(t.Title) {
		lines = append(lines, fmt.Sprintf("- Title, before: %s", before))
	}
	if strings.TrimSpace(old) == "" {
		lines = append(lines, fmt.Sprintf("- The rest of the earlier text is in the issue of %s.", k))
	} else {
		oldSlice, oldFiles, oldTests := ticketText(old)
		tests := t.Tests
		if len(tests) == 0 {
			tests = t.Acceptance
		}
		if s := oneLine(t.Slice); s != oldSlice && oldSlice != "" {
			lines = append(lines, fmt.Sprintf("- Slice, before: %s", oldSlice))
		}
		lines = append(lines, listChange("Files", oldFiles, trimmed(t.Files))...)
		lines = append(lines, listChange("Acceptance tests", oldTests, trimmed(acceptanceLines(tests)))...)
	}
	if len(lines) == 0 {
		lines = append(lines, fmt.Sprintf("- The text below replaces the text of %s.", k))
	}
	b.WriteString(strings.Join(lines, "\n"))
	if s := strings.TrimSpace(t.Slice); s != "" {
		b.WriteString("\n\n" + s)
	}
	t.ID = fu.id
	t.Title = ticketFollowUpPrefix + k + ": " + strings.TrimSpace(t.Title)
	t.Slice = b.String()
	return t
}

// listChange says which entries were added to a list and which are no longer in it.
func listChange(name string, before, after []string) []string {
	var added, gone []string
	for _, a := range after {
		if !slices.Contains(before, a) {
			added = append(added, a)
		}
	}
	for _, o := range before {
		if !slices.Contains(after, o) {
			gone = append(gone, o)
		}
	}
	var out []string
	if len(added) > 0 {
		out = append(out, fmt.Sprintf("- %s added: %s", name, strings.Join(added, ", ")))
	}
	if len(gone) > 0 {
		out = append(out, fmt.Sprintf("- %s no longer listed: %s", name, strings.Join(gone, ", ")))
	}
	return out
}

// ticketText reads the slice (on one line), files and acceptance tests back from an issue
// body that ticketIssueBody wrote.
func ticketText(body string) (slice string, files, tests []string) {
	section := ""
	var text []string
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		l := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(l, "Feature: "):
			return oneLine(strings.Join(text, " ")), files, tests
		case l == "Files:":
			section = "files"
		case l == "Acceptance tests:":
			section = "tests"
		case section == "files" && strings.HasPrefix(l, "- "):
			files = append(files, strings.TrimSpace(l[2:]))
		case section == "tests" && strings.HasPrefix(l, "- "):
			tests = append(tests, strings.TrimSpace(l[2:]))
		case section == "":
			text = append(text, l)
		}
	}
	return oneLine(strings.Join(text, " ")), files, tests
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func trimmed(list []string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, strings.TrimSpace(s))
	}
	return out
}
