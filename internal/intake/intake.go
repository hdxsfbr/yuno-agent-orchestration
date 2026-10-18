// Package intake turns labelled issues into runs and posts the outcome back on the issue
// (DESIGN 5.23, decisions D-045, D-052, D-053 and D-055). A loop polls every project with
// intake enabled: the taker creates one run per labelled issue and moves the label to
// <label>:running, the closer moves it to <label>:done or <label>:failed when the run
// ends and comments with the result. The run row is the record (trigger issue, trigger_ref
// "<project id>#<n>", reported_at "posted on the issue"); there is no table of its own.
package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/engine"
	"yuno/internal/guardrails"
	"yuno/internal/obs"
	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

const (
	// DefaultInterval is INTAKE_POLL_INTERVAL's default (DESIGN 5.23 inputs).
	DefaultInterval = 120 * time.Second
	// DefaultTimeout bounds the pass over one project (rule 2) and the closing of one run
	// (rule 5).
	DefaultTimeout = 30 * time.Second
	// DefaultCloseTimeout is the one deadline of a closer pass over all its runs (rule 2,
	// review L6).
	DefaultCloseTimeout = 5 * time.Minute
	// ListLimit is the size of one page of the issue listing (rule 2 (c)).
	ListLimit = 50
	// ListPages is how many pages one pass reads at most (rule 2 (c), review L5).
	ListPages = 4
	// ListTakeable is how many takeable issues end the reading of pages early (rule 2 (c),
	// review L5): a pull request or a confidential issue is not one.
	ListTakeable = 10
	// TakeLimit is how many new runs one pass creates per project; the rest of the issues
	// wait for the next poll (rule 2 (d), D-053).
	TakeLimit = 10
	// CloseLimit is how many finished runs one pass reports (rule 5).
	CloseLimit = 50

	// errorMax caps intake_last_error.
	errorMax = 500
	// titleLogMax is how much of an issue's title a log line carries; the issue's text is
	// never logged in full.
	titleLogMax = 80
	// reasonMax is the failure reason a comment carries (rule 5 (b)).
	reasonMax = 300
)

// ErrIntakeDisabled is PollProject's answer for a project whose intake is off (rule 2:
// 409 intake_disabled); its text is the 409's message.
var ErrIntakeDisabled = errors.New("the project's intake is off")

// ErrorNoWorkflow is intake_last_error for a project whose workflow is gone (edge cases of
// 5.23): the project is skipped until the operator chooses another.
const ErrorNoWorkflow = "intake: no workflow"

// Triggerer is the part of the engine the loop needs: the same trigger transaction any run
// goes through, with the workflow's limits, the project's budget and the agents' rates.
type Triggerer interface {
	Trigger(ctx context.Context, req engine.TriggerRequest) (*store.Run, error)
}

// Loop polls the projects with intake enabled. Client should come from
// tools.ProviderClient, so a token header never follows a redirect to another host; nil is
// that client over the default transport. Interval is INTAKE_POLL_INTERVAL, Timeout bounds
// the pass over one project and each provider step of the closer (30 s when zero),
// CloseTimeout the whole closer pass (5 minutes when zero), Now is the clock (nil: the
// system's, in UTC) and PublicBaseURL prefixes the run links in the comments (relative
// without it).
type Loop struct {
	Pool          *pgxpool.Pool
	Projects      *project.Resolver
	Client        *http.Client
	Engine        Triggerer
	Interval      time.Duration
	Timeout       time.Duration
	CloseTimeout  time.Duration
	Log           *slog.Logger
	Now           func() time.Time
	PublicBaseURL string

	// ensured holds the (project, label) pairs whose derived labels exist on the provider:
	// they are created once per process start (rule 2 (b)).
	ensured sync.Map
}

// Result is what one pass over one project did (POST /api/projects/{id}/intake/poll).
// Error is the pass's first provider or trigger error, empty when clean.
type Result struct {
	Taken  int    `json:"taken"`
	Closed int    `json:"closed"`
	Error  string `json:"error,omitempty"`
}

// Summary is what one tick did across the projects.
type Summary struct {
	Projects int
	Taken    int
	Closed   int
}

// Run passes every Interval until ctx ends, the first one at once (rule 2).
func (l *Loop) Run(ctx context.Context) {
	interval := l.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	l.log().Info("issue intake started", "interval", interval.String())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		l.TickOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// TickOnce is one pass (rule 2): the taker over every live project with intake enabled
// and a workflow, oldest first, then the closer of rule 5 over every finished issue run
// not yet reported, whatever its project. A provider error on one project is logged,
// recorded on it and the pass goes on; a database error ends the pass with a log line and
// the next tick tries again.
func (l *Loop) TickOnce(ctx context.Context) Summary {
	var sum Summary
	projects, err := store.ListIntakeProjects(ctx, l.Pool)
	if err != nil {
		l.logDB(ctx, "list the projects", err)
		return sum
	}
	taken := make(map[string]string, len(projects)) // the taker's first error per project
	for i := range projects {
		if ctx.Err() != nil {
			return sum
		}
		p := &projects[i]
		res, err := l.takePass(ctx, p)
		if err != nil {
			l.logDB(ctx, "take the issues of project "+p.Name, err)
			return sum
		}
		if err := l.record(ctx, p.ID, res.at, res.err); err != nil {
			l.logDB(ctx, "record the poll of project "+p.Name, err)
			return sum
		}
		sum.Projects++
		sum.Taken += res.taken
		taken[p.ID] = res.err
	}
	closed, closeErrs, err := l.closePass(ctx, "")
	sum.Closed = closed
	if err != nil {
		l.logDB(ctx, "report the finished runs", err)
		return sum
	}
	// A project the taker found clean whose finished runs could not be posted shows the
	// closer's first error instead (the pass's first error is the taker's when it had one).
	for i := range projects {
		id := projects[i].ID
		if msg := closeErrs[id]; msg != "" && taken[id] == "" {
			if err := l.record(ctx, id, l.now(), msg); err != nil {
				l.logDB(ctx, "record the poll of project "+projects[i].Name, err)
				return sum
			}
		}
	}
	return sum
}

// PollProject runs one pass for one project at once (rule 2, POST /api/projects/{id}/
// intake/poll): the taker and then the closer over that project's finished runs. The
// error is store.ErrNotFound for a missing or deleted project, ErrIntakeDisabled when its
// intake is off, or a database error; a provider or trigger error is in Result.Error and
// on the project, not returned.
func (l *Loop) PollProject(ctx context.Context, projectID string) (Result, error) {
	p, err := store.GetProject(ctx, l.Pool, projectID)
	if err != nil {
		return Result{}, err
	}
	if !p.IntakeEnabled {
		return Result{}, ErrIntakeDisabled
	}
	res, err := l.takePass(ctx, p)
	if err != nil {
		return Result{}, err
	}
	closed, closeErrs, err := l.closePass(ctx, p.ID)
	if err != nil {
		return Result{}, err
	}
	first := res.err
	if first == "" {
		first = closeErrs[p.ID]
	}
	if err := l.record(ctx, p.ID, res.at, first); err != nil {
		return Result{}, err
	}
	return Result{Taken: res.taken, Closed: closed, Error: first}, nil
}

// record writes the poll's bookkeeping of a project: now, and the pass's first error or
// none (rule 2). A project that is gone meanwhile is not an error.
func (l *Loop) record(ctx context.Context, projectID string, at time.Time, firstErr string) error {
	var msg *string
	if firstErr != "" {
		msg = &firstErr
	}
	err := store.SetProjectIntakePoll(ctx, l.Pool, projectID, at, msg)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// takeResult is what the taker did on one project.
type takeResult struct {
	at    time.Time
	taken int
	// err is the first error of the pass, cleaned for intake_last_error: empty when clean.
	err string
}

// fail records the first error of a pass and keeps it.
func (p *takeResult) fail(text string) {
	if p.err == "" {
		p.err = text
	}
}

// takePass is the taker's pass over one project (rule 2 (a) to (d), rule 3). The error is
// a database one, which ends the tick.
func (l *Loop) takePass(ctx context.Context, p *store.Project) (takeResult, error) {
	res := takeResult{at: l.now()}
	log := l.log().With("project_id", p.ID, "project", p.Name)

	// The workflow: gone (soft-deleted, or set null) means nothing can be taken until the
	// operator chooses another (edge cases of 5.23).
	if p.IntakeWorkflowID == nil {
		log.Warn("intake: project " + p.Name + " skipped: no workflow")
		res.fail(ErrorNoWorkflow)
		return res, nil
	}
	if _, err := store.GetWorkflow(ctx, l.Pool, *p.IntakeWorkflowID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Warn("intake: project " + p.Name + " skipped: no workflow")
			res.fail(ErrorNoWorkflow)
			return res, nil
		}
		return res, err
	}

	// (a) The budget (5.19 rule 3): at or over a cap the project is skipped, the issues
	// keep their label and are taken when the window resets.
	if err := project.CheckBudget(ctx, l.Pool, p, res.at); err != nil {
		var be *project.BudgetError
		if !errors.As(err, &be) {
			return res, err
		}
		log.Info("intake: project " + p.Name + " skipped: " + be.Error())
		res.fail(cleanError(be.Error(), ""))
		return res, nil
	}

	token, err := l.Projects.Token(ctx, p.ID)
	if err != nil {
		// No token stored, or one that cannot be opened (5.17 rule 2): the operator fixes
		// it on the Projects page.
		log.Warn("intake: project "+p.Name+" skipped: its token cannot be read", "error", cleanError(err.Error(), ""))
		res.fail(cleanError(err.Error(), ""))
		return res, nil
	}
	pc := project.ConfigOf(p.Settings(), token)
	prov := tools.IssueProviderFor(&pc, l.Client)
	pctx, cancel := context.WithTimeout(ctx, l.timeout())
	defer cancel()

	// (b) The derived labels, once per project per process. A failure is the pass's first
	// error and the pass goes on: a token that cannot create labels may still swap them,
	// and one that cannot is told so by the swap (T5.23.10).
	if err := l.ensureLabels(pctx, p.ID, p.IntakeLabel, prov); err != nil {
		msg := cleanError(err.Error(), pc.Token)
		log.Warn("intake: the labels could not be created", "error", msg)
		res.fail(msg)
	}

	// (c) The listing: pages of 50 until 10 takeable issues are found or 4 pages were read,
	// so issues that stay labelled do not hide the newer ones (review L5).
	issues, err := listIssues(pctx, prov, p.IntakeLabel)
	if err != nil {
		msg := cleanError(err.Error(), pc.Token)
		log.Warn("intake: the issues could not be listed", "error", msg)
		res.fail(msg)
		if len(issues) == 0 {
			return res, nil
		}
		// A later page failed: the issues of the pages before it are taken all the same.
	}

	// (d) The taker, issue by issue in the provider's order.
	newRuns, refused := 0, false
	for _, issue := range issues {
		if ctx.Err() != nil {
			return res, nil
		}
		if pctx.Err() != nil {
			break
		}
		if issue.IsPullRequest {
			continue
		}
		ref := store.IssueTriggerRef(p.ID, issue.Number)
		ilog := log.With("issue", issue.Number, "title", cutTitle(issue.Title))
		// Taken already: a live run, or a finished one whose outcome is not posted (rule 3,
		// review H1). The swap below repairs the label; no second run is created.
		run, err := store.TakenIssueRun(ctx, l.Pool, ref)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return res, err
		}
		if err == nil && store.IsTerminal(run.Status) {
			ilog.Info("intake: issue has a finished run whose outcome is not posted; no new run", "run_id", run.ID, "status", run.Status)
		}
		if err != nil {
			// Not taken yet. The cap of rule 2 (d), and a refusal that is the project's or the
			// workflow's rather than this issue's, leave it for the next poll.
			if refused || newRuns >= TakeLimit {
				continue
			}
			// The budget with the runs in flight counted at their ceiling (rule 2 (a), review
			// M2): the project's spend so far says nothing about what a run just started
			// will cost, so the cap is checked against what the live runs may still spend.
			full, inFlight, err := l.atBudgetInFlight(ctx, p, res.at)
			if err != nil {
				return res, err
			}
			if full {
				log.Info(fmt.Sprintf("intake: project %s at its budget with %d runs in flight", p.Name, inFlight))
				refused = true
				continue
			}
			var stop bool
			run, stop = l.trigger(pctx, p, &pc, issue, ref, ilog, &res)
			if stop {
				refused = true
			}
			if run == nil {
				continue
			}
			newRuns++
			res.taken++
			ilog.Info("intake: issue taken", "run_id", run.ID)
		}
		// (c) the swap goes first, it is the state; (d) the comment is best effort.
		if err := prov.SwapLabels(pctx, issue.Number, p.IntakeLabel+tools.IssueRunningSuffix, p.IntakeLabel); err != nil {
			msg := cleanError(err.Error(), pc.Token)
			ilog.Warn("intake: the label swap failed; the next poll tries again", "run_id", run.ID, "error", msg)
			res.fail(msg)
			continue
		}
		text := "Yuno started run " + tools.RunURL(l.PublicBaseURL, run.ID) + " for this issue."
		if err := prov.Comment(pctx, issue.Number, text); err != nil {
			ilog.Warn("intake: the start comment failed; it is not retried", "run_id", run.ID, "error", cleanError(err.Error(), pc.Token))
		}
	}
	if pctx.Err() != nil && ctx.Err() == nil {
		res.fail(fmt.Sprintf("intake: timed out after %s", l.timeout()))
	}
	return res, nil
}

// trigger creates the run for an issue (rule 3 (b)) and answers the run, or nil when none
// was created. stop says the refusal is not this issue's, so the pass takes no more. A
// refusal is logged with the issue and recorded as the pass's error; the issue stays as it
// is for the next poll. A second take by another process is the live-run index's 23505
// (store.ErrIssueBusy): the issue counts as taken, by the other process, and is not
// touched here.
func (l *Loop) trigger(ctx context.Context, p *store.Project, pc *project.Config, issue tools.Issue, ref string, log *slog.Logger, res *takeResult) (run *store.Run, stop bool) {
	if l.Engine == nil {
		res.fail("intake: the run engine is not available")
		return nil, true
	}
	task := tools.IssueTask(pc.Path(), issue, project.IssueURL(p.Settings(), issue.Number))
	input, err := json.Marshal(map[string]string{"task": task})
	if err != nil {
		res.fail(cleanError(err.Error(), ""))
		return nil, false
	}
	run, err = l.Engine.Trigger(ctx, engine.TriggerRequest{WorkflowID: *p.IntakeWorkflowID, ProjectID: p.ID,
		Trigger: store.TriggerIssue, TriggerRef: ref, Input: input})
	var (
		ve *engine.ValidationError
		rl *guardrails.RateLimitError
		be *project.BudgetError
	)
	switch {
	case err == nil:
		return run, false
	case errors.Is(err, store.ErrIssueBusy):
		log.Info("intake: issue taken by another process")
		return nil, false
	case ctx.Err() != nil:
		return nil, false
	case errors.As(err, &ve):
		// The workflow's input schema or definition changed since the settings were saved,
		// or this issue's text does not fit it; nothing is taken until that is fixed.
		stop = false
	case errors.As(err, &rl), errors.As(err, &be):
		stop = true
	case errors.Is(err, store.ErrNotFound):
		res.fail(ErrorNoWorkflow)
		log.Warn("intake: the workflow is gone", "error", cleanError(err.Error(), pc.Token))
		return nil, true
	default:
		stop = true
	}
	msg := cleanError(err.Error(), pc.Token)
	log.Warn("intake: the issue was refused", "error", msg)
	res.fail(msg)
	return nil, stop
}

// listIssues reads the labelled issues page by page, 50 to a page, until ListTakeable
// takeable ones are found or ListPages pages were read (rule 2 (c), review L5). What the
// provider lists is already free of pull requests and confidential issues. After a failure
// it returns the issues of the pages read before it with the error.
func listIssues(ctx context.Context, prov tools.IssueProvider, label string) ([]tools.Issue, error) {
	var all []tools.Issue
	for page := 1; page <= ListPages; page++ {
		issues, more, err := prov.ListLabelled(ctx, label, page, ListLimit)
		if err != nil {
			return all, err
		}
		all = append(all, issues...)
		if !more || len(all) >= ListTakeable {
			break
		}
	}
	return all, nil
}

// atBudgetInFlight reports whether the project may take no new run (rule 2 (a), review M2):
// for either window with a cap, the cap minus the window's spend minus the max_cost_usd of
// the project's live runs is at or under zero. inFlight is the number of live runs. A
// project with no cap is never full, and is not queried.
func (l *Loop) atBudgetInFlight(ctx context.Context, p *store.Project, now time.Time) (full bool, inFlight int, err error) {
	if p.BudgetDayUSDMicros == nil && p.BudgetMonthUSDMicros == nil {
		return false, 0, nil
	}
	inFlight, ceiling, err := store.LiveRunCostCeiling(ctx, l.Pool, p.ID)
	if err != nil {
		return false, 0, fmt.Errorf("project %s live runs: %w", p.ID, err)
	}
	day, month := project.Windows(now)
	for _, c := range []struct {
		w   project.Window
		cap *store.USDMicros
	}{{day, p.BudgetDayUSDMicros}, {month, p.BudgetMonthUSDMicros}} {
		if c.cap == nil {
			continue
		}
		spent, err := store.SumProjectSpend(ctx, l.Pool, p.ID, c.w.Start, c.w.End)
		if err != nil {
			return false, 0, fmt.Errorf("project %s spend: %w", p.ID, err)
		}
		if int64(*c.cap)-spent-ceiling <= 0 {
			return true, inFlight, nil
		}
	}
	return false, inFlight, nil
}

// ensureLabels creates the three derived labels of an intake label on the provider the
// first time it sees the pair in this process (rule 2 (b)); a failure leaves the pair to
// be tried at the next pass.
func (l *Loop) ensureLabels(ctx context.Context, projectID, label string, prov tools.IssueProvider) error {
	key := projectID + "\x00" + label
	if _, ok := l.ensured.Load(key); ok {
		return nil
	}
	if err := prov.EnsureLabels(ctx, tools.IntakeLabels(label)); err != nil {
		return err
	}
	l.ensured.Store(key, struct{}{})
	return nil
}

// closePass is the closer (rule 5) over the finished issue runs whose outcome is not on
// their issue yet, newest first: every project's when projectID is empty, else one
// project's. The whole pass runs under one deadline (CloseTimeout, 5 minutes), and a run
// whose provider call timed out ends it early (rule 2, review L6): a hanging provider
// holds the tick for one timeout, not one per run. It returns how many it reported and,
// per project, the first error that kept a run from being reported. The error is a
// database one.
func (l *Loop) closePass(ctx context.Context, projectID string) (closed int, errs map[string]string, err error) {
	errs = map[string]string{}
	deadline := time.Now().Add(l.closeTimeout())
	var runs []store.IssueRun
	if projectID == "" {
		runs, err = store.ListUnreportedIssueRuns(ctx, l.Pool, CloseLimit)
	} else {
		runs, err = store.ListUnreportedIssueRunsOfProject(ctx, l.Pool, projectID, CloseLimit)
	}
	if err != nil {
		return 0, errs, err
	}
	for i := range runs {
		if ctx.Err() != nil {
			return closed, errs, nil
		}
		if !time.Now().Before(deadline) {
			l.log().Warn("intake: the closer ran out of time; the other finished runs wait for the next pass", "waiting", len(runs)-i)
			break
		}
		res, err := l.closeRun(ctx, deadline, &runs[i])
		if err != nil {
			return closed, errs, err
		}
		switch {
		case res.reported:
			closed++
		case res.msg != "" && errs[runs[i].ProjectID] == "":
			errs[runs[i].ProjectID] = res.msg
		}
		if res.timedOut {
			l.log().Warn("intake: the provider did not answer in time; the closer stops here and the next pass tries again", "waiting", len(runs)-i-1)
			break
		}
	}
	return closed, errs, nil
}

// closeResult is what the closer did with one finished run: reported says its outcome is on
// the issue (or never can be, see closeRun); msg is the error to show on the project when
// it is not; timedOut says a provider call ran into its deadline, which ends the pass.
type closeResult struct {
	reported bool
	msg      string
	timedOut bool
}

// closeRun posts the outcome of one finished run on its issue (rule 5): the label swap,
// the comment, then reported_at. A provider failure in the swap or the comment is logged
// and leaves reported_at null, so the next pass tries again (the swap is idempotent, the
// comment may repeat once); a run whose token cannot be read is skipped the same way. A run
// whose project row is gone, or whose issue answers 404 or 410 on the swap, can never be
// posted: it is marked reported with a log line (review M1) and counts as no report. Every
// provider call ends at the deadline of the pass. The error is a database one.
func (l *Loop) closeRun(ctx context.Context, deadline time.Time, ir *store.IssueRun) (closeResult, error) {
	run := &ir.Run
	log := l.log().With("run_id", run.ID, "project_id", ir.ProjectID, "issue", ir.Number)
	proj, err := store.GetProject(ctx, l.Pool, ir.ProjectID)
	if errors.Is(err, store.ErrNotFound) {
		return l.nothingToPost(ctx, run, log, "its project no longer exists")
	}
	if err != nil {
		return closeResult{}, err
	}
	pc, err := l.Projects.ForRun(ctx, run)
	if err != nil {
		msg := cleanError(err.Error(), "")
		log.Warn("intake: finished run skipped: its token cannot be read", "error", msg)
		return closeResult{msg: msg}, nil
	}
	label := proj.IntakeLabel
	prov := tools.IssueProviderFor(pc, l.Client)
	pctx, cancel := context.WithTimeout(ctx, l.timeout())
	defer cancel()
	pctx, cancelDeadline := context.WithDeadline(pctx, deadline)
	defer cancelDeadline()
	// timedOut says a failed call ran into the step's or the pass's deadline, not the
	// shutdown's.
	timedOut := func() bool { return ctx.Err() == nil && pctx.Err() != nil }

	runURL := tools.RunURL(l.PublicBaseURL, run.ID)
	var add, remove, text string
	if run.Status == store.RunSucceeded {
		mr, err := store.MergeRequestByRun(ctx, l.Pool, run.ID)
		if err != nil {
			return closeResult{}, err
		}
		// A retry that succeeds replaces :failed with :done (edge cases of 5.23), so the
		// opposite outcome label goes with :running.
		add, remove, text = label+tools.IssueDoneSuffix, label+tools.IssueRunningSuffix+","+label+tools.IssueFailedSuffix, finishedText(runURL, mr)
	} else {
		reason := ""
		if run.FailureReason != nil {
			reason = cleanReason(*run.FailureReason, pc.Token)
		}
		add, remove, text = label+tools.IssueFailedSuffix, label+tools.IssueRunningSuffix+","+label+tools.IssueDoneSuffix, failedText(runURL, run.Status, reason, label)
	}

	// The labels of the three outcomes exist already when the taker passed this process
	// (rule 2 (b)); for a project that is not polled any more they are created here.
	if err := l.ensureLabels(pctx, ir.ProjectID, label, prov); err != nil {
		log.Warn("intake: the labels could not be created", "error", cleanError(err.Error(), pc.Token))
	}
	if err := prov.SwapLabels(pctx, ir.Number, add, remove); err != nil {
		msg := cleanError(err.Error(), pc.Token)
		if tools.IssueGone(err) {
			return l.nothingToPost(ctx, run, log, fmt.Sprintf("the issue is gone (%s)", msg))
		}
		log.Warn("intake: the outcome label could not be set; the next pass tries again", "error", msg)
		return closeResult{msg: msg, timedOut: timedOut()}, nil
	}
	if err := prov.Comment(pctx, ir.Number, text); err != nil {
		msg := cleanError(err.Error(), pc.Token)
		log.Warn("intake: the outcome comment failed; the next pass tries again", "error", msg)
		return closeResult{msg: msg, timedOut: timedOut()}, nil
	}
	if err := store.SetRunReportedAt(ctx, l.Pool, run.ID, l.now()); err != nil {
		return closeResult{}, err
	}
	log.Info("intake: finished run reported", "status", run.Status)
	return closeResult{reported: true}, nil
}

// nothingToPost marks a finished run reported when nothing can ever be posted for it (its
// project row is gone, its issue is gone), with the log line of rule 5, so it stops
// heading the closer's list (review M1).
func (l *Loop) nothingToPost(ctx context.Context, run *store.Run, log *slog.Logger, reason string) (closeResult, error) {
	log.Info("intake: run " + run.ID + " has nothing to post: " + reason)
	if err := store.SetRunReportedAt(ctx, l.Pool, run.ID, l.now()); err != nil {
		return closeResult{}, err
	}
	return closeResult{}, nil
}

// finishedText is the comment of a succeeded run (rule 5 (b)): the request's link when the
// run has a record (5.20 rule 6), "Pull request:" on GitHub, else the no-request line.
func finishedText(runURL string, mr *store.MergeRequest) string {
	if mr == nil {
		return "Yuno finished run " + runURL + "; it opened no merge request."
	}
	what := "Merge request:"
	if mr.Provider == string(project.ProviderGitHub) {
		what = "Pull request:"
	}
	return "Yuno finished run " + runURL + ". " + what + " " + mr.URL
}

// failedText is the comment of a run that failed, timed out or was cancelled (rule 5
// (b)); the reason, already cut, is left out when empty.
func failedText(runURL, status, reason, label string) string {
	what := "failed"
	switch status {
	case store.RunTimedOut:
		what = "timed out"
	case store.RunCancelled:
		what = "was cancelled"
	}
	text := "Yuno run " + runURL + " " + what
	if reason != "" {
		text += ": " + reason
	}
	return text + ". Remove the label " + label + tools.IssueFailedSuffix + " and add " + label + " to try again."
}

// cleanReason is a run's failure reason as a comment carries it: valid UTF-8 without NUL
// bytes, the token and every credential-shaped value redacted, trimmed, cut at 300 bytes
// on a rune boundary (rule 5 (b)).
func cleanReason(text, token string) string {
	text = strings.ToValidUTF8(text, "")
	text = strings.ReplaceAll(text, "\x00", "")
	if token != "" {
		text = strings.ReplaceAll(text, token, obs.Redacted)
	}
	return cutBytes(strings.TrimSpace(obs.Redact(text)), reasonMax)
}

// cleanError is a provider's, resolver's or engine's text as intake_last_error keeps it:
// valid UTF-8 without NUL bytes (the column refuses both, and a write that fails would end
// every pass at this project), the token and every credential-shaped value redacted, the
// first line only, capped at errorMax bytes on a rune boundary. It is the sync loop's
// cleanError (5.20), copied rather than imported.
func cleanError(text, token string) string {
	text = strings.ToValidUTF8(text, "")
	text = strings.ReplaceAll(text, "\x00", "")
	if token != "" {
		text = strings.ReplaceAll(text, token, obs.Redacted)
	}
	text = obs.Redact(text)
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		text = text[:i]
	}
	return cutBytes(strings.TrimSpace(text), errorMax)
}

// cutTitle is the first 80 bytes of an issue's title, for a log line.
func cutTitle(title string) string {
	return cutBytes(strings.ToValidUTF8(title, ""), titleLogMax)
}

// cutBytes cuts text to at most max bytes on a rune boundary.
func cutBytes(text string, max int) string {
	if len(text) <= max {
		return text
	}
	for max > 0 && !utf8.RuneStart(text[max]) {
		max--
	}
	return text[:max]
}

func (l *Loop) timeout() time.Duration {
	if l.Timeout > 0 {
		return l.Timeout
	}
	return DefaultTimeout
}

func (l *Loop) closeTimeout() time.Duration {
	if l.CloseTimeout > 0 {
		return l.CloseTimeout
	}
	return DefaultCloseTimeout
}

func (l *Loop) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

func (l *Loop) log() *slog.Logger {
	if l.Log != nil {
		return l.Log
	}
	return slog.New(slog.DiscardHandler)
}

// logDB logs a database failure unless it is the shutdown's own.
func (l *Loop) logDB(ctx context.Context, what string, err error) {
	if ctx.Err() != nil {
		return
	}
	l.log().Warn("intake: "+what+" failed", "error", err)
}
