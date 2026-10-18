package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"

	"yuno/internal/engine"
	"yuno/internal/intake"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// Issue intake (DESIGN 5.23): the settings a project saves (rule 1) and the poll-now
// route (rule 2).

// maxIntakeLabelLen is the longest intake label, in bytes (5.23 rule 1).
const maxIntakeLabelLen = 50

// intakeTaskSample is the input the intake gives its workflow, as a one-line sample: the
// workflow's input_schema is checked against it when the settings are saved (5.23 rule 1).
const intakeTaskSample = `{"task":"x"}`

// applyIntake checks the input's intake settings (DESIGN 5.23 rule 1) and puts the
// settings to save on next. cur is the stored project on an update and nil on a create.
//
// Absent keeps: an update without `intake` keeps the stored settings (a create takes the
// defaults, off with the label yuno), so a form that does not know the intake does not
// clear it. Present replaces the three members, an absent member taking its default (off,
// yuno, no workflow). The 422 details are on the paths intake.label, intake.workflow_id
// and intake.enabled, in that order; the workflow is looked up and its input checked
// only when intake is enabled or the workflow is a different one from the stored one, so
// a project whose workflow was deleted since can still be saved, intake off, as it was
// fetched. The error is a database one.
func (s *Server) applyIntake(ctx context.Context, in *projectInput, next, cur *store.Project) (fieldErrors, error) {
	if in.Intake == nil {
		var fe fieldErrors
		if cur != nil {
			next.IntakeEnabled, next.IntakeLabel, next.IntakeWorkflowID = cur.IntakeEnabled, cur.IntakeLabel, cur.IntakeWorkflowID
			// D-055 holds for the resulting state too: an update that keeps intake on while
			// it clears both caps is refused like enabling without one.
			if cur.IntakeEnabled && next.BudgetDayUSDMicros == nil && next.BudgetMonthUSDMicros == nil &&
				in.MaxCostUSDPerDay == nil && in.MaxCostUSDPerMonth == nil {
				fe.add("intake.enabled", "set a daily or monthly budget before enabling intake")
			}
		}
		return fe, nil
	}
	var fe fieldErrors
	it := in.Intake

	label := store.DefaultIntakeLabel
	if it.Label != nil {
		label = strings.TrimSpace(*it.Label)
	}
	if !validIntakeLabel(label) {
		fe.add("intake.label", "1 to %d characters without spaces or commas", maxIntakeLabelLen)
	}

	wid := strings.TrimSpace(it.WorkflowID)
	changed := cur == nil || cur.IntakeWorkflowID == nil || *cur.IntakeWorkflowID != wid
	switch {
	case wid == "":
		if it.Enabled {
			fe.add("intake.workflow_id", "required when intake is enabled")
		}
	case it.Enabled || changed:
		msg, err := s.intakeWorkflowProblem(ctx, wid)
		if err != nil {
			return nil, err
		}
		if msg != "" {
			fe.add("intake.workflow_id", "%s", msg)
		}
	}

	// D-055: the project's budget is the only bound on what a labelled issue can spend, so
	// intake is enabled on a project with a cap. A cap sent in this request, valid or not,
	// is the request's own business: a refused one has its own detail already.
	if it.Enabled && next.BudgetDayUSDMicros == nil && next.BudgetMonthUSDMicros == nil &&
		in.MaxCostUSDPerDay == nil && in.MaxCostUSDPerMonth == nil {
		fe.add("intake.enabled", "set a daily or monthly budget before enabling intake")
	}

	next.IntakeEnabled, next.IntakeLabel, next.IntakeWorkflowID = it.Enabled, label, nil
	if wid != "" {
		next.IntakeWorkflowID = &wid
	}
	return fe, nil
}

// validIntakeLabel is the rule of 5.23 rule 1: 1 to 50 bytes after trimming, no comma (a
// provider takes a list of labels as comma-separated text), no whitespace and no control
// character.
func validIntakeLabel(label string) bool {
	if label == "" || len(label) > maxIntakeLabelLen {
		return false
	}
	return !strings.ContainsFunc(label, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

// intakeWorkflowProblem says why a workflow cannot be the intake's, "" when it can
// (DESIGN 5.23 rule 1): a live operator workflow (not a template, not a quick-run
// wrapper) that opts in to a project and whose input_schema accepts {"task": string}.
// The error is a database one.
func (s *Server) intakeWorkflowProblem(ctx context.Context, id string) (string, error) {
	missing := "no workflow with id " + id
	// An id with a control character cannot be a workflow's, and Postgres refuses a NUL
	// in a parameter.
	if hasControl(id) || len(id) > 128 {
		return missing, nil
	}
	wf, err := store.GetWorkflow(ctx, s.pool, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return missing, nil
	case err != nil:
		return "", err
	case wf.TemplateKey != nil:
		return missing, nil
	}
	def, err := workflow.Parse(wf.Definition)
	if err != nil {
		return "the workflow's definition cannot be read", nil
	}
	if !def.Project {
		return "the workflow does not use a project", nil
	}
	if msgs := engine.ValidateInput(def.InputSchema, json.RawMessage(intakeTaskSample)); len(msgs) > 0 {
		return `the workflow's input must accept {"task": string}: ` + strings.Join(msgs, "; "), nil
	}
	return "", nil
}

// intakeLoop is the intake the poll-now route runs: the server's project resolver, provider
// client and engine (the engine may be absent in a test, which then takes nothing). The
// background loop is the coordinator's, started in Run beside the scheduler.
func (s *Server) intakeLoop() *intake.Loop {
	var eng intake.Triggerer
	if s.engine != nil {
		eng = s.engine
	}
	return &intake.Loop{Pool: s.pool, Projects: s.projects, Client: s.checkClient, Engine: eng, Log: s.log,
		PublicBaseURL: s.cfg.PublicBaseURL}
}

// pollIntake runs one intake pass for one project at once (DESIGN 5.23 rule 2): 200
// {taken, closed, error?}, 409 intake_disabled when the project's intake is off, 404 for
// a missing or deleted project. A provider or trigger error is the result's `error` (also
// the project's intake.last_error), not an HTTP error.
func (s *Server) pollIntake(w http.ResponseWriter, r *http.Request) {
	res, err := s.intakeLoop().PollProject(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, intake.ErrIntakeDisabled):
		writeError(w, http.StatusConflict, "intake_disabled", err.Error(), nil)
	case err != nil:
		s.storeError(w, r, "project", err)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}
