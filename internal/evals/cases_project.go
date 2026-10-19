package evals

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"yuno/internal/store"
)

// ProjectSpec is a case's project on the fake forge (DESIGN 5.29 rule 17): the runner
// creates a repository on the forge and a project for it through the api, runs the case's
// run on that project, and deletes the project at the end. Steps then act on the forge
// and wait for the platform while the run goes.
type ProjectSpec struct {
	// DefaultBranch of the repository; main when empty.
	DefaultBranch string `yaml:"default_branch"`
	// Intake switches the project's issue intake (DESIGN 5.23) on or off; nil is off.
	Intake *IntakeSpec `yaml:"intake"`
	// MaxCostUSDPerDay is the project's daily cap; intake needs one (D-055).
	MaxCostUSDPerDay float64 `yaml:"max_cost_usd_per_day"`
	// Issues are opened on the forge before the project exists.
	Issues []IssueSeed `yaml:"issues"`
	// TimeoutSeconds replaces the runner's per-case timeout for the case's run; the steps
	// have their own.
	TimeoutSeconds int `yaml:"timeout_seconds"`
	// PlatformRuns are the scripts of the runs the platform starts on the project by itself
	// (the runs intake creates for issues, the wave watcher's ship runs), which no request of
	// the runner names a script in. Per trigger they are used in the order listed.
	PlatformRuns []PlatformRun `yaml:"platform_runs"`
}

// PlatformRun is the script of one run the platform starts on the project: the next run of
// Trigger (issue or plan) takes it. The agents of that run are the workflow's own.
type PlatformRun struct {
	Trigger string       `yaml:"trigger"`
	Script  []ScriptStep `yaml:"script"`
}

// IntakeSpec is the project's intake: Workflow names an existing workflow by its name.
type IntakeSpec struct {
	Enabled  bool   `yaml:"enabled"`
	Label    string `yaml:"label"`
	Workflow string `yaml:"workflow"`
}

// IssueSeed is an issue opened on the forge.
type IssueSeed struct {
	Title  string   `yaml:"title"`
	Body   string   `yaml:"body"`
	Labels []string `yaml:"labels"`
}

// CaseStep is one step of a project case: exactly one of Forge, Wait, Hold, Approve,
// PauseWaves, ResumeWaves or PollIntake. Steps run in order, beside the case's run, and a failed step fails the case.
type CaseStep struct {
	Forge *ForgeStep `yaml:"forge"`
	Wait  *WaitStep  `yaml:"wait"`
	// Approve decides pending approvals of the project's runs (the run's own and the runs
	// the platform starts), the case's actions.approvals reaching only the former.
	Approve *ApproveStep `yaml:"approve"`
	// PollIntake asks the api to poll the project's issues now (POST
	// /api/projects/{id}/intake/poll) instead of waiting for the interval.
	PollIntake bool `yaml:"poll_intake"`
	// Hold passes only if something does not happen for a while.
	Hold *HoldStep `yaml:"hold"`
	// PauseWaves pauses the project's waves with this reason (POST
	// /api/projects/{id}/waves/pause); ResumeWaves resumes them.
	PauseWaves  string `yaml:"pause_waves"`
	ResumeWaves bool   `yaml:"resume_waves"`
}

// HoldStep fails if the plan starts a ship run for wave NoShipRun within Seconds (the plan
// detail's ship_runs), which the case uses to show a paused project ships nothing.
type HoldStep struct {
	NoShipRun int `yaml:"no_ship_run"`
	Seconds   int `yaml:"seconds"`
}

// ForgeStep acts on, or checks, the fake forge: exactly one member is set.
type ForgeStep struct {
	// MergeOpen merges the open, non-draft merge requests, until Count of them have been
	// merged by this step (1 when 0): the platform opens them while the step waits.
	MergeOpen *MergeOpenStep `yaml:"merge_open"`
	// Pipeline records a pipeline on the default branch's head: passed, failed, running or
	// pending.
	Pipeline string `yaml:"pipeline"`
	// CreateIssue opens an issue on the forge.
	CreateIssue *IssueSeed `yaml:"create_issue"`
	// ExpectIssues waits until every expectation is met by some issue, or fails.
	ExpectIssues []IssueExpect `yaml:"expect_issues"`
	// TimeoutSeconds bounds merge_open and expect_issues; 60 when 0.
	TimeoutSeconds int `yaml:"timeout_seconds"`
}

// MergeOpenStep is the merge_open action.
type MergeOpenStep struct {
	Count int `yaml:"count"`
}

// IssueExpect is met by an issue whose title contains TitleContains (when set), that
// carries every one of Labels and, when State is set, is in that state (opened or closed).
type IssueExpect struct {
	TitleContains string   `yaml:"title_contains"`
	Labels        []string `yaml:"labels"`
	State         string   `yaml:"state"`
}

// WaitStep waits for the project's plan (DESIGN 5.29): exactly one of PlanStatus (some
// plan of the project has that status), PlanEvent (some plan has an event of that kind, of
// wave Wave when that is set) or TicketStatus (some plan has tickets in wave Wave, all of
// them in that status).
type WaitStep struct {
	PlanStatus     string `yaml:"plan_status"`
	PlanEvent      string `yaml:"plan_event"`
	TicketStatus   string `yaml:"ticket_status"`
	Wave           int    `yaml:"wave"`
	TimeoutSeconds int    `yaml:"timeout_seconds"` // 60 when 0
}

// ApproveStep waits for pending approvals of the project's runs and decides them: Count
// of them (1 when 0), only those at node Node when that is set.
type ApproveStep struct {
	Decision       string `yaml:"decision"` // approve | reject
	Reason         string `yaml:"reason"`
	Node           string `yaml:"node"`
	Count          int    `yaml:"count"`
	TimeoutSeconds int    `yaml:"timeout_seconds"` // 60 when 0
}

// Bounds for the project block, in seconds and counts.
const (
	defaultStepTimeout = 60
	maxStepTimeout     = 1800
	maxProjectTimeout  = 3600
	maxStepCount       = 50
)

var (
	// planStatusNames and planEventNames are what a wait step may name.
	planStatusNames = []string{store.PlanPublishing, store.PlanWaitingPlanMerge, store.PlanBuilding, store.PlanShipping,
		store.PlanAccepting, store.PlanDone, store.PlanCancelled}
	planEventNames = []string{store.PlanEventPublished, store.PlanEventWaveReleased, store.PlanEventSmokeTestWaiting,
		store.PlanEventWaveDone, store.PlanEventAcceptWaiting, store.PlanEventFeatureDone, store.PlanEventEscalated,
		store.PlanEventPaused, store.PlanEventResumed, store.PlanEventRetried, store.PlanEventCancelled}
	ticketStatusNames = []string{store.TicketPending, store.TicketReleased, store.TicketBuilding, store.TicketInReview,
		store.TicketMerged, store.TicketFailed, store.TicketClosed}
	platformTriggers    = []string{store.TriggerIssue, store.TriggerPlan}
	forgePipelineStates = []string{"passed", "failed", "running", "pending"}
	issueStates         = []string{"opened", "closed"}
	// intakeLabelPattern follows the api's rule for a label: no space, no comma.
	intakeLabelPattern = regexp.MustCompile(`^[^\s,]{1,50}$`)
	branchNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,60}$`)
)

// stepTimeoutSeconds is a step's timeout: its own, else the default.
func stepTimeoutSeconds(n int) int {
	if n > 0 {
		return n
	}
	return defaultStepTimeout
}

// validateProject checks the project block and the steps.
func (c Case) validateProject(add func(string, ...any)) {
	if c.Project == nil {
		if len(c.Steps) > 0 {
			add("steps need a project block: they act on the project's forge repository")
		}
		return
	}
	p := c.Project
	if p.DefaultBranch != "" && !branchNamePattern.MatchString(p.DefaultBranch) {
		add("project.default_branch %q is not a branch name", p.DefaultBranch)
	}
	if p.MaxCostUSDPerDay < 0 {
		add("project.max_cost_usd_per_day must not be negative")
	}
	if p.TimeoutSeconds < 0 || p.TimeoutSeconds > maxProjectTimeout {
		add("project.timeout_seconds %d must be between 0 and %d", p.TimeoutSeconds, maxProjectTimeout)
	}
	if in := p.Intake; in != nil {
		if in.Label != "" && !intakeLabelPattern.MatchString(in.Label) {
			add("project.intake.label %q must be 1 to 50 characters without spaces or commas", in.Label)
		}
		if in.Enabled && strings.TrimSpace(in.Workflow) == "" {
			add("project.intake.workflow is required when intake is enabled: the name of the workflow an issue starts")
		}
		if in.Enabled && p.MaxCostUSDPerDay <= 0 {
			add("project.intake.enabled needs project.max_cost_usd_per_day: the api refuses intake on a project without a cap")
		}
	}
	for i, is := range p.Issues {
		checkIssueSeed(fmt.Sprintf("project.issues[%d]", i), is, add)
	}
	for i, pr := range p.PlatformRuns {
		at := fmt.Sprintf("project.platform_runs[%d]", i)
		if !slices.Contains(platformTriggers, pr.Trigger) {
			add("%s.trigger %q must be one of %s", at, pr.Trigger, strings.Join(platformTriggers, ", "))
		}
		if len(pr.Script) == 0 {
			add("%s.script is required", at)
		}
		for j, st := range pr.Script {
			checkScriptStep(fmt.Sprintf("%s.script step %d", at, j+1), st, add)
		}
	}
	if len(c.Steps) > maxStepCount {
		add("steps has %d entries, at most %d", len(c.Steps), maxStepCount)
	}
	for i, s := range c.Steps {
		s.validate(fmt.Sprintf("steps[%d]", i), add)
	}
}

func checkIssueSeed(at string, is IssueSeed, add func(string, ...any)) {
	if strings.TrimSpace(is.Title) == "" {
		add("%s.title is required", at)
	}
	for j, l := range is.Labels {
		if strings.TrimSpace(l) == "" || strings.Contains(l, ",") {
			add("%s.labels[%d] %q must be a label without a comma", at, j, l)
		}
	}
}

func checkStepTimeout(at string, n int, add func(string, ...any)) {
	if n < 0 || n > maxStepTimeout {
		add("%s.timeout_seconds %d must be between 0 and %d", at, n, maxStepTimeout)
	}
}

func (s CaseStep) validate(at string, add func(string, ...any)) {
	set := 0
	for _, b := range []bool{s.Forge != nil, s.Wait != nil, s.Approve != nil, s.PollIntake, s.Hold != nil,
		s.PauseWaves != "", s.ResumeWaves} {
		if b {
			set++
		}
	}
	if set != 1 {
		add("%s must set exactly one of forge, wait, hold, approve, pause_waves, resume_waves or poll_intake, got %d", at, set)
		return
	}
	switch {
	case s.Hold != nil:
		h := s.Hold
		if h.NoShipRun < 1 {
			add("%s.hold.no_ship_run %d must be a wave number from 1", at, h.NoShipRun)
		}
		if h.Seconds < 1 || h.Seconds > maxStepTimeout {
			add("%s.hold.seconds %d must be between 1 and %d", at, h.Seconds, maxStepTimeout)
		}
	case s.Forge != nil:
		s.Forge.validate(at+".forge", add)
	case s.Wait != nil:
		w := s.Wait
		checkStepTimeout(at+".wait", w.TimeoutSeconds, add)
		named := 0
		for _, v := range []string{w.PlanStatus, w.PlanEvent, w.TicketStatus} {
			if v != "" {
				named++
			}
		}
		switch {
		case named != 1:
			add("%s.wait must set exactly one of plan_status, plan_event or ticket_status", at)
		case w.TicketStatus != "" && !slices.Contains(ticketStatusNames, w.TicketStatus):
			add("%s.wait.ticket_status %q must be one of %s", at, w.TicketStatus, strings.Join(ticketStatusNames, ", "))
		case w.TicketStatus != "" && w.Wave < 1:
			add("%s.wait.ticket_status needs wave", at)
		case w.PlanStatus != "" && !slices.Contains(planStatusNames, w.PlanStatus):
			add("%s.wait.plan_status %q must be one of %s", at, w.PlanStatus, strings.Join(planStatusNames, ", "))
		case w.PlanEvent != "" && !slices.Contains(planEventNames, w.PlanEvent):
			add("%s.wait.plan_event %q must be one of %s", at, w.PlanEvent, strings.Join(planEventNames, ", "))
		}
		if w.Wave < 0 {
			add("%s.wait.wave must not be negative", at)
		}
		if w.Wave != 0 && w.PlanStatus != "" {
			add("%s.wait.wave belongs to plan_event and ticket_status", at)
		}
	case s.Approve != nil:
		a := s.Approve
		checkStepTimeout(at+".approve", a.TimeoutSeconds, add)
		if a.Decision != DecisionApprove && a.Decision != DecisionReject {
			add("%s.approve.decision %q must be %q or %q", at, a.Decision, DecisionApprove, DecisionReject)
		}
		if a.Count < 0 || a.Count > maxStepCount {
			add("%s.approve.count %d must be between 0 and %d", at, a.Count, maxStepCount)
		}
	}
}

func (f *ForgeStep) validate(at string, add func(string, ...any)) {
	checkStepTimeout(at, f.TimeoutSeconds, add)
	set := 0
	for _, b := range []bool{f.MergeOpen != nil, f.Pipeline != "", f.CreateIssue != nil, len(f.ExpectIssues) > 0} {
		if b {
			set++
		}
	}
	if set != 1 {
		add("%s must set exactly one of merge_open, pipeline, create_issue or expect_issues, got %d", at, set)
		return
	}
	switch {
	case f.MergeOpen != nil:
		if f.MergeOpen.Count < 0 || f.MergeOpen.Count > maxStepCount {
			add("%s.merge_open.count %d must be between 0 and %d", at, f.MergeOpen.Count, maxStepCount)
		}
	case f.Pipeline != "":
		if !slices.Contains(forgePipelineStates, f.Pipeline) {
			add("%s.pipeline %q must be one of %s", at, f.Pipeline, strings.Join(forgePipelineStates, ", "))
		}
	case f.CreateIssue != nil:
		checkIssueSeed(at+".create_issue", *f.CreateIssue, add)
	default:
		for i, e := range f.ExpectIssues {
			if e.TitleContains == "" && len(e.Labels) == 0 && e.State == "" {
				add("%s.expect_issues[%d] sets none of title_contains, labels or state", at, i)
			}
			if e.State != "" && !slices.Contains(issueStates, e.State) {
				add("%s.expect_issues[%d].state %q must be %s", at, i, e.State, strings.Join(issueStates, " or "))
			}
		}
	}
}
