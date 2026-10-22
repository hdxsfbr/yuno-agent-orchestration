package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// PublishTicketsName turns a plan's tickets into tracker issues grouped in waves (DESIGN
// 5.29 rule 2, D-071 to D-078).
const PublishTicketsName = "publish_tickets"

// The caps of a plan (rule 2), shared with the waves check that runs before the plan is
// approved.
const (
	MaxPlanTickets = workflow.MaxPlanTickets
	MaxPlanWaves   = workflow.MaxPlanWaves
)

// The ids of the plan pipeline's nodes whose outputs hold the plan (rule 2): the Planner's
// tickets and the waves that partition them.
const (
	planTicketsNode = "slicer"
	planWavesNode   = "waves"
)

// PublishTicketsConfig is what publish_tickets needs.
type PublishTicketsConfig struct {
	// DB reads and writes the plan when the call carries no transaction (the unit tests);
	// the executor's transaction is used otherwise.
	DB       store.Querier
	Projects ProjectResolver
	// APIBase and RepoPath override the project's APIBase() and Path() (tests aim them at
	// a fake provider).
	APIBase  string
	RepoPath string
	Client   *http.Client
	// RunIDForToken, when set, lets the approval sentence count the tickets per wave: it
	// resolves the run_token of the input to its run. Without it the sentence is generic.
	RunIDForToken func(token string) (runID string, ok bool)
}

var publishTicketsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_token": {"type": "string", "description": "The run token from the task message."}
  },
  "required": ["run_token"],
  "additionalProperties": false
}`)

// planTicketIn is one ticket of the slicer's output.
type planTicketIn struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	Slice      string          `json:"slice"`
	Files      []string        `json:"files"`
	Tests      json.RawMessage `json:"tests"`
	Acceptance json.RawMessage `json:"acceptance_tests"` // fallback for tests
	// DependsOn names the tickets of the plan whose merged work this one needs (Chief
	// platform control, DESIGN wave 3 rule 2): each in the same or an earlier wave.
	DependsOn []string `json:"depends_on,omitempty"`
}

// The checks of the tool (review L4): the feature slug of 5.28 and the longest ticket title
// in an issue title, in characters.
const maxTicketTitle = 200

var featureSlug = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// publishPlan is the validated plan of a run: its tickets in wave order.
type publishPlan struct {
	feature string
	// revise is the id of the plan this run amends (input revise_plan, rule 16); "" for a new plan.
	revise string
	waves  [][]planTicketIn // index = wave - 1
	count  int
	// edges are the tickets' depends_on as [dependent, prerequisite] pairs, in plan order
	// (DESIGN wave 3 rule 2).
	edges [][2]string
}

// publishTicketsOutput is the tool's result.
type publishTicketsOutput struct {
	PlanID string               `json:"plan_id"`
	Issues []publishedTicketOut `json:"issues"`
	Reused int                  `json:"reused"`
	// The counts of a revision (rule 16); absent for a new plan.
	Revised bool `json:"revised,omitempty"`
	Added   int  `json:"added,omitempty"`
	Updated int  `json:"updated,omitempty"`
	Dropped int  `json:"dropped,omitempty"`
	Reset   int  `json:"reset,omitempty"`
	// ChangedInFlight names the building or in-review tickets whose text changed: their work
	// in progress was built from the old text, so the operator requests changes (rule 16).
	ChangedInFlight []string `json:"changed_in_flight,omitempty"`
	Released        int      `json:"released,omitempty"`
	// FollowUps are the tickets the platform added for changes to merged or closed tickets
	// (rule 16).
	FollowUps []followUpOut `json:"follow_ups,omitempty"`
}

type publishedTicketOut struct {
	Ticket string `json:"ticket"`
	Number int64  `json:"number"`
	URL    string `json:"url"`
}

type followUpOut struct {
	Ticket  string `json:"ticket"`
	Follows string `json:"follows"`
	Number  int64  `json:"number"`
	URL     string `json:"url"`
}

// planIDNamespace derives plan ids, so that a visit retried after its transaction rolled
// back finds the issues it created by their marker (the marker holds the plan id).
var planIDNamespace = uuid.MustParse("5a0f1c2e-7d3b-4b8e-9a61-2f4d6c8e0b13")

// NewPublishTickets builds publish_tickets: create the run's plan and one issue per ticket
// in wave order, reusing the issues found by their marker (DESIGN 5.29 rule 2). The tool
// reads the plan from the calling run's state, so the model cannot change a ticket between
// the approval and the effect.
func NewPublishTickets(cfg PublishTicketsConfig) *Tool {
	if cfg.Client == nil {
		cfg.Client = ProviderClient(nil)
	}
	p := &publishTickets{cfg: cfg}
	return &Tool{
		Name: PublishTicketsName,
		Description: "Publish the plan's tickets as tracker issues: one issue per ticket of the plan you wrote, " +
			"labelled with its wave and feature, in wave order. Takes only the run_token; the platform reads the " +
			"tickets and waves from the run's earlier steps, so nothing else is passed. Refuses when the project " +
			"already has an open plan. When the run's input names a plan to revise, it amends that plan instead: new tickets " +
			"get issues, changed tickets update theirs, dropped tickets are closed, and a change to a ticket that is already merged " +
			"becomes a follow-up ticket with its own issue. Needs a human approval. Returns the plan id and each ticket's issue number " +
			"and URL; if a human has not decided yet it returns status pending_approval: then end your turn " +
			"without finishing, and the platform resumes you with the outcome; if they refused it returns " +
			"status rejected with a reason.",
		InputSchema: publishTicketsSchema,
		Risk:        RiskSideEffecting,
		Hits:        HitsExternal,
		Timeout:     5 * time.Minute,
		Describe:    p.describe,
		Exec:        p.exec,
	}
}

type publishTickets struct {
	cfg PublishTicketsConfig
}

// describe is the approval sentence: the counts per wave and the labels the issues get.
func (p *publishTickets) describe(input json.RawMessage) string {
	const generic = "Publish the plan's tickets as issues on the run's project"
	var in struct {
		RunToken string `json:"run_token"`
	}
	if json.Unmarshal(input, &in) != nil || p.cfg.RunIDForToken == nil || p.cfg.DB == nil {
		return generic
	}
	runID, ok := p.cfg.RunIDForToken(in.RunToken)
	if !ok {
		return generic
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	run, err := store.GetRun(ctx, p.cfg.DB, runID)
	if err != nil {
		return generic
	}
	plan, err := readPublishPlan(run)
	if err != nil {
		return generic
	}
	parts := make([]string, len(plan.waves))
	for i, w := range plan.waves {
		parts[i] = fmt.Sprintf("wave %d: %d", i+1, len(w))
	}
	if plan.revise != "" {
		return reviseDescription(ctx, p.cfg.DB, plan, parts)
	}
	return fmt.Sprintf("Publish %d tickets of feature %q as issues (%s), labelled wave:<n> and feature:%s",
		plan.count, plan.feature, strings.Join(parts, ", "), plan.feature)
}

// readPublishPlan reads and checks the plan in a run's state and input.
func readPublishPlan(run *store.Run) (*publishPlan, error) {
	var in struct {
		Feature    string `json:"feature"`
		RevisePlan string `json:"revise_plan"`
	}
	_ = json.Unmarshal(run.Input, &in)
	feature := strings.TrimSpace(in.Feature)
	revise := strings.TrimSpace(in.RevisePlan)
	if revise != "" {
		if _, err := uuid.Parse(revise); err != nil {
			return nil, fmt.Errorf("revise_plan %q is not a plan id", revise)
		}
	}
	if feature == "" {
		return nil, errors.New("the run's input has no feature")
	}
	if !featureSlug.MatchString(feature) {
		return nil, fmt.Errorf("the feature %q is not a slug (lowercase letters, digits and hyphens, 1 to 40 characters)", feature)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(run.State, &state); err != nil {
		return nil, errors.New("the run has no state yet")
	}
	var slicer struct {
		Tickets []planTicketIn `json:"tickets"`
	}
	if json.Unmarshal(state[planTicketsNode], &slicer) != nil || len(slicer.Tickets) == 0 {
		return nil, fmt.Errorf("the run has no tickets (the output of %s)", planTicketsNode)
	}
	var waves struct {
		Waves []struct {
			N       int      `json:"n"`
			Tickets []string `json:"tickets"`
		} `json:"waves"`
	}
	if json.Unmarshal(state[planWavesNode], &waves) != nil || len(waves.Waves) == 0 {
		return nil, fmt.Errorf("the run has no waves (the output of %s)", planWavesNode)
	}
	if len(slicer.Tickets) > MaxPlanTickets {
		return nil, fmt.Errorf("the plan has %d tickets; at most %d", len(slicer.Tickets), MaxPlanTickets)
	}
	if len(waves.Waves) > MaxPlanWaves {
		return nil, fmt.Errorf("the plan has %d waves; at most %d", len(waves.Waves), MaxPlanWaves)
	}
	byID := map[string]planTicketIn{}
	for _, t := range slicer.Tickets {
		if t.ID == "" || strings.TrimSpace(t.Title) == "" {
			return nil, errors.New("a ticket has no id or no title")
		}
		if _, dup := byID[t.ID]; dup {
			return nil, fmt.Errorf("ticket id %s is used twice", t.ID)
		}
		byID[t.ID] = t
	}
	sort.SliceStable(waves.Waves, func(i, j int) bool { return waves.Waves[i].N < waves.Waves[j].N })
	plan := &publishPlan{feature: feature, revise: revise}
	placed := map[string]bool{}
	for i, w := range waves.Waves {
		if w.N != i+1 {
			return nil, fmt.Errorf("waves must be numbered 1 to %d without gaps", len(waves.Waves))
		}
		var list []planTicketIn
		for _, id := range w.Tickets {
			t, ok := byID[id]
			switch {
			case !ok:
				return nil, fmt.Errorf("wave %d names the unknown ticket %s", w.N, id)
			case placed[id]:
				return nil, fmt.Errorf("ticket %s is in more than one wave", id)
			}
			placed[id] = true
			list = append(list, t)
		}
		plan.waves = append(plan.waves, list)
		plan.count += len(list)
	}
	if plan.count != len(byID) {
		return nil, errors.New("some tickets are in no wave")
	}
	edges, err := ticketDependencies(plan.waves)
	if err != nil {
		return nil, err
	}
	plan.edges = edges
	return plan, nil
}

// ticketDependencies reads and checks the tickets' depends_on (Chief platform control, DESIGN
// wave 3 rules 1 and 2): each id is another ticket of the plan in the same or an earlier
// wave, and the edges make no cycle. It returns them as [dependent, prerequisite] pairs.
func ticketDependencies(waves [][]planTicketIn) ([][2]string, error) {
	waveOf := map[string]int{}
	for i, w := range waves {
		for _, t := range w {
			waveOf[t.ID] = i + 1
		}
	}
	var edges [][2]string
	next := map[string][]string{}
	for _, w := range waves {
		for _, t := range w {
			seen := map[string]bool{}
			for _, raw := range t.DependsOn {
				dep := strings.TrimSpace(raw)
				switch wv, ok := waveOf[dep]; {
				case seen[dep]:
					continue
				case dep == t.ID:
					return nil, fmt.Errorf("%s depends on itself", t.ID)
				case !ok:
					return nil, fmt.Errorf("%s depends on %s, which is not a ticket of the plan", t.ID, dep)
				case wv > waveOf[t.ID]:
					return nil, fmt.Errorf("%s depends on %s, which is in a later wave", t.ID, dep)
				}
				seen[dep] = true
				edges = append(edges, [2]string{t.ID, dep})
				next[t.ID] = append(next[t.ID], dep)
			}
		}
	}
	// An edge never points to a later wave, so a cycle lies within one wave; a depth-first
	// walk finds it.
	const (
		unseen = iota
		onPath
		done
	)
	state := map[string]int{}
	var path []string
	var walk func(id string) []string
	walk = func(id string) []string {
		state[id] = onPath
		path = append(path, id)
		for _, dep := range next[id] {
			switch state[dep] {
			case onPath:
				i := slices.Index(path, dep)
				return append(slices.Clone(path[i:]), dep)
			case unseen:
				if c := walk(dep); c != nil {
					return c
				}
			}
		}
		path = path[:len(path)-1]
		state[id] = done
		return nil
	}
	for _, w := range waves {
		for _, t := range w {
			if state[t.ID] == unseen {
				if c := walk(t.ID); c != nil {
					return nil, fmt.Errorf("the dependencies would make a cycle: %s", strings.Join(c, " -> "))
				}
			}
		}
	}
	return edges, nil
}

func (p *publishTickets) exec(ctx context.Context, call Call) (Result, error) {
	if p.cfg.Projects == nil {
		return Result{}, &ModelError{Message: project.ErrNoProject.Error()}
	}
	var q store.Querier = p.cfg.DB
	if call.Tx != nil {
		q = call.Tx
	}
	if q == nil {
		return Result{}, errors.New("publish_tickets: no database")
	}
	run, err := store.GetRun(ctx, q, call.RunID)
	if err != nil {
		return Result{}, fmt.Errorf("read the run: %w", err)
	}
	if run.ProjectID == nil {
		return Result{}, &ModelError{Message: project.ErrNoProject.Error()}
	}
	pc, err := p.cfg.Projects.ForRunID(ctx, call.RunID)
	if errors.Is(err, project.ErrNoProject) {
		return Result{}, &ModelError{Message: err.Error()}
	}
	if err != nil {
		return Result{IsError: true, Text: err.Error()}, nil
	}
	plan, err := readPublishPlan(run)
	if err != nil {
		return Result{}, &ModelError{Message: "publish_tickets: " + err.Error()}
	}

	if plan.revise != "" {
		return p.revise(ctx, q, run, pc, plan)
	}
	planID := uuid.NewSHA1(planIDNamespace, []byte(call.RunID+"\x00"+plan.feature)).String()
	stored, perr := p.ensurePlan(ctx, q, run, planID, plan.feature)
	if perr != nil {
		return Result{}, perr
	}

	base, path := p.cfg.APIBase, p.cfg.RepoPath
	if base == "" {
		base = pc.APIBase()
	}
	if path == "" {
		path = pc.Path()
	}
	issues := newIssueProvider(pc, p.cfg.Client, base, path)

	// The plan's merge request is linked whenever the plan lacks one and the run has a
	// record (review M5: the record may appear after the first call).
	if stored.PlanMRID == nil {
		if mr, err := store.MergeRequestByRun(ctx, q, run.ID); err != nil {
			return Result{}, err
		} else if mr != nil {
			if err := store.SetPlanMR(ctx, q, planID, mr.ID); err != nil {
				return Result{}, err
			}
			stored.PlanMRID = &mr.ID
		}
	}
	planMRURL := ""
	if stored.PlanMRID != nil {
		if mr, err := store.GetMergeRequest(ctx, q, *stored.PlanMRID); err == nil {
			planMRURL = mr.URL
		}
	}
	// The links name the default branch, which outlives the plan MR's branch (review M6).
	docs := planDocLinks(pc, pc.DefaultBranch, plan.feature)

	existing := map[string]store.PlanTicket{}
	rows, err := store.ListPlanTickets(ctx, q, planID)
	if err != nil {
		return Result{}, err
	}
	for _, t := range rows {
		existing[t.TicketID] = t
	}

	out := publishTicketsOutput{PlanID: planID, Issues: []publishedTicketOut{}}
	label := "feature:" + plan.feature
	// The issues of the feature are listed once, when a ticket lacks one, and matched by
	// marker here (review M7).
	var byMarker map[string]Issue
	for i, wave := range plan.waves {
		n := i + 1
		for _, t := range wave {
			row := store.PlanTicket{PlanID: planID, TicketID: t.ID, Wave: n, Title: t.Title}
			if prev, ok := existing[t.ID]; ok && prev.IssueNumber != nil {
				row.IssueNumber, row.IssueURL = prev.IssueNumber, prev.IssueURL
			}
			if err := store.UpsertPlanTicket(ctx, q, &row); err != nil {
				return Result{}, err
			}
			if err := store.SetTicketContentHash(ctx, q, planID, t.ID, ticketContentHash(plan.feature, t)); err != nil {
				return Result{}, err
			}
			if row.IssueNumber != nil {
				out.Reused++
				out.Issues = append(out.Issues, publishedTicketOut{Ticket: t.ID, Number: *row.IssueNumber, URL: row.IssueURL})
				continue
			}
			marker := ticketMarker(planID, t.ID)
			if byMarker == nil {
				all, err := issues.ListAll(ctx, label, issueScanCap)
				if err != nil {
					return Result{IsError: true, Text: fmt.Sprintf("publish_tickets: look up the issues of %s: %s", label, err)}, nil
				}
				byMarker = map[string]Issue{}
				for _, is := range all {
					if m := lastLine(is.Body); strings.HasPrefix(m, "<!-- yuno-ticket:") {
						if _, dup := byMarker[m]; !dup {
							byMarker[m] = is
						}
					}
				}
			}
			var issue Issue
			if found, ok := byMarker[marker]; ok {
				issue = found
				out.Reused++
			} else {
				labels := []string{fmt.Sprintf("wave:%d", n), label}
				issue, err = issues.Create(ctx, ticketIssueTitle(plan.feature, t), ticketIssueBody(plan.feature, t, docs, planMRURL, marker), labels)
				if err != nil {
					return Result{IsError: true, Text: fmt.Sprintf("publish_tickets: create the issue of ticket %s: %s", t.ID, err)}, nil
				}
			}
			if err := store.SetTicketIssue(ctx, q, planID, t.ID, issue.Number, issue.URL); err != nil {
				return Result{}, err
			}
			out.Issues = append(out.Issues, publishedTicketOut{Ticket: t.ID, Number: issue.Number, URL: issue.URL})
		}
	}

	// The Planner's edges, ticket to ticket (DESIGN wave 3 rule 2).
	if err := store.ReplacePlannerDependencies(ctx, q, *run.ProjectID, planID, plan.edges); err != nil {
		return Result{}, err
	}
	if stored.Status == store.PlanPublishing {
		if err := store.SetPlanStatus(ctx, q, planID, store.PlanWaitingPlanMerge); err != nil {
			return Result{}, err
		}
	}
	detail, _ := json.Marshal(map[string]any{"tickets": plan.count, "waves": len(plan.waves), "reused": out.Reused})
	if err := store.AddPlanEvent(ctx, q, &store.PlanEvent{PlanID: planID, Kind: store.PlanEventPublished, Detail: detail}); err != nil {
		return Result{}, err
	}
	return JSONResult(out), nil
}

// ensurePlan returns the plan of this run and feature, creating it when absent. A second
// open plan, or an earlier plan of the same feature, is refused with a model error naming
// it. The checks come before the insert so that the call's transaction is not poisoned by
// a unique violation.
func (p *publishTickets) ensurePlan(ctx context.Context, q store.Querier, run *store.Run, planID, feature string) (*store.Plan, error) {
	if got, err := store.GetPlan(ctx, q, planID); err == nil {
		return got, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	open, err := store.OpenPlanForProject(ctx, q, *run.ProjectID)
	if err != nil {
		return nil, err
	}
	if open != nil {
		return nil, openPlanError(open)
	}
	all, err := store.ListPlans(ctx, q, *run.ProjectID)
	if err != nil {
		return nil, err
	}
	for _, o := range all {
		if o.Feature == feature {
			return nil, &ModelError{Message: fmt.Sprintf("publish_tickets: the project already has a plan for feature %q (plan %s, %s)", feature, o.ID, o.Status)}
		}
	}
	plan := &store.Plan{ID: planID, ProjectID: *run.ProjectID, Feature: feature, PlanRunID: run.ID, Status: store.PlanPublishing}
	if mr, err := store.MergeRequestByRun(ctx, q, run.ID); err != nil {
		return nil, err
	} else if mr != nil {
		plan.PlanMRID = &mr.ID
	}
	if err := store.CreatePlan(ctx, q, plan); err != nil {
		switch {
		case errors.Is(err, store.ErrPlanOpen):
			if open, oerr := store.OpenPlanForProject(ctx, q, *run.ProjectID); oerr == nil && open != nil {
				return nil, openPlanError(open)
			}
			return nil, &ModelError{Message: "publish_tickets: " + err.Error()}
		case errors.Is(err, store.ErrPlanExists):
			return nil, &ModelError{Message: fmt.Sprintf("publish_tickets: the project already has a plan for feature %q", feature)}
		}
		return nil, err
	}
	return plan, nil
}

func openPlanError(open *store.Plan) error {
	return &ModelError{Message: fmt.Sprintf("publish_tickets: the project already has an open plan: feature %q (plan %s, %s); "+
		"it must be done or cancelled before another is published", open.Feature, open.ID, open.Status)}
}

// ticketMarker is the hidden last line of an issue's body (rule 2).
func ticketMarker(planID, ticketID string) string {
	return "<!-- yuno-ticket:" + planID + ":" + ticketID + " -->"
}

// ticketContentHash identifies the text a ticket's issue is written from (its title, slice,
// files and tests), not the links around it.
func ticketContentHash(feature string, t planTicketIn) string {
	sum := sha256.Sum256([]byte(ticketIssueTitle(feature, t) + "\x00" + ticketIssueBody(feature, t, nil, "", "")))
	return hex.EncodeToString(sum[:])
}

func ticketIssueTitle(feature string, t planTicketIn) string {
	title := strings.TrimSpace(t.Title)
	if r := []rune(title); len(r) > maxTicketTitle {
		title = strings.TrimSpace(string(r[:maxTicketTitle]))
	}
	return fmt.Sprintf("[%s %s] %s", feature, t.ID, title)
}

// lastLine is the last non-empty line of a body, trimmed (review L3: the marker counts
// only there, so a quote of one in a ticket's text matches nothing).
func lastLine(body string) string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

// planDocLinks are the links to the plan documents on the default branch, where they are
// once the plan merge request is merged.
func planDocLinks(pc *project.Config, branch, feature string) []string {
	web := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(pc.RepoURL), "/"), ".git")
	if u, err := url.Parse(web); err != nil || u.Host == "" {
		return nil
	} else {
		u.User = nil
		web = u.String()
	}
	blob := "/-/blob/"
	if pc.Provider == project.ProviderGitHub {
		blob = "/blob/"
	}
	var out []string
	for _, doc := range []string{"PRD.md", "ARCHITECTURE.md", "TASKS.md", "WAVES.md"} {
		out = append(out, fmt.Sprintf("- [%s](%s%s%s/docs/plans/%s/%s)", doc, web, blob, escapeBranch(branch), feature, doc))
	}
	return out
}

// ticketIssueBody is the issue text (rule 2): the ticket's slice, files and acceptance
// tests, the Feature line that 5.28 rule 5 reads, the plan documents, and the marker last.
func ticketIssueBody(feature string, t planTicketIn, docs []string, planMR, marker string) string {
	var b strings.Builder
	if s := strings.TrimSpace(t.Slice); s != "" {
		b.WriteString(s + "\n\n")
	}
	if len(t.Files) > 0 {
		b.WriteString("Files:\n")
		for _, f := range t.Files {
			b.WriteString("- " + f + "\n")
		}
		b.WriteString("\n")
	}
	tests := t.Tests
	if len(tests) == 0 {
		tests = t.Acceptance
	}
	if acc := acceptanceLines(tests); len(acc) > 0 {
		b.WriteString("Acceptance tests:\n")
		for _, a := range acc {
			b.WriteString("- " + a + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("Feature: " + feature + "\n")
	if len(docs) > 0 {
		b.WriteString("\nPlan documents (on the default branch once the plan merge request is merged):\n" + strings.Join(docs, "\n") + "\n")
	}
	if planMR != "" {
		b.WriteString("\nPlan merge request: " + planMR + "\n")
	}
	b.WriteString("\n" + marker)
	return b.String()
}

// acceptanceLines reads the acceptance tests as a list of strings or as one text.
func acceptanceLines(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var one string
	if json.Unmarshal(raw, &one) == nil && strings.TrimSpace(one) != "" {
		return strings.Split(strings.TrimSpace(one), "\n")
	}
	return nil
}

// escapeBranch escapes a branch name for a URL path, keeping its slashes.
func escapeBranch(branch string) string {
	parts := strings.Split(branch, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
