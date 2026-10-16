package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"yuno/internal/engine"
	"yuno/internal/sched"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

const maxScheduleBody = 256 << 10

// scheduleNow is the clock schedule writes compute next_run_at from; tests pin it.
var scheduleNow = func() time.Time { return time.Now().UTC() }

// scheduleInput is ScheduleInput. Fields left out keep their stored value on PUT, so
// the enabled toggle is {"enabled": false}; the read-only Schedule fields are accepted
// and ignored so a client can PUT back what it fetched.
type scheduleInput struct {
	WorkflowID *string         `json:"workflow_id"`
	Kind       *string         `json:"kind"`
	CronExpr   *string         `json:"cron_expr"`
	TZ         *string         `json:"tz"`
	RunAt      *string         `json:"run_at"`
	Enabled    *bool           `json:"enabled"`
	Input      json.RawMessage `json:"input"`

	ID           json.RawMessage `json:"id"`
	WorkflowName json.RawMessage `json:"workflow_name"`
	NextRunAt    json.RawMessage `json:"next_run_at"`
	LastRunAt    json.RawMessage `json:"last_run_at"`
	LastRunID    json.RawMessage `json:"last_run_id"`
	LastError    json.RawMessage `json:"last_error"`
	CreatedAt    json.RawMessage `json:"created_at"`
	UpdatedAt    json.RawMessage `json:"updated_at"`
}

// apply merges the input onto sc, validates the result (DESIGN 5.9 rule 1) and sets
// next_run_at from now. A cron expression or zone the parser refuses is reported with
// the parser's message.
func (s *Server) applySchedule(ctx context.Context, in *scheduleInput, sc *store.Schedule, now time.Time) (fieldErrors, error) {
	var fe fieldErrors
	if in.WorkflowID != nil {
		sc.WorkflowID = strings.TrimSpace(*in.WorkflowID)
	}
	if in.Kind != nil {
		sc.Kind = *in.Kind
	}
	if in.CronExpr != nil {
		expr := strings.TrimSpace(*in.CronExpr)
		sc.CronExpr = &expr
	}
	if in.TZ != nil {
		sc.TZ = strings.TrimSpace(*in.TZ)
	}
	if in.RunAt != nil {
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(*in.RunAt))
		if err != nil {
			fe.add("run_at", "must be an RFC 3339 timestamp such as 2026-10-15T09:00:00Z")
		} else {
			at = at.UTC()
			sc.RunAt = &at
		}
	}
	if in.Enabled != nil {
		sc.Enabled = *in.Enabled
	}
	if in.Input != nil {
		sc.Input = in.Input
	}
	if t := bytes.TrimSpace(sc.Input); len(t) == 0 || string(t) == "null" {
		sc.Input = json.RawMessage(`{}`)
	}
	if sc.TZ == "" {
		sc.TZ = "UTC"
	}

	switch sc.Kind {
	case store.ScheduleCron:
		if sc.CronExpr == nil || *sc.CronExpr == "" {
			fe.add("cron_expr", "is required for a cron schedule")
		} else if err := sched.ValidateCron(*sc.CronExpr); err != nil {
			fe.add("cron_expr", "%s", err.Error())
		}
		sc.RunAt = nil
	case store.ScheduleDelay:
		if sc.RunAt == nil && in.RunAt == nil {
			fe.add("run_at", "is required for a delay schedule")
		}
		sc.CronExpr = nil
	case "":
		fe.add("kind", "is required")
	default:
		fe.add("kind", "must be one of cron, delay")
	}
	if _, err := sched.LoadZone(sc.TZ); err != nil {
		fe.add("tz", "%s", err.Error())
	}

	wf, err := s.scheduleWorkflow(ctx, sc.WorkflowID, &fe)
	if err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(sc.Input, &obj) != nil {
		fe.add("input", "must be an object")
	} else if wf != nil {
		fe = append(fe, scheduleInputErrors(wf, sc.Input)...)
	}
	if len(fe) > 0 {
		return fe, nil
	}
	next, err := sched.NextRun(sc, now)
	if err != nil {
		fe.add("cron_expr", "%s", err.Error())
		return fe, nil
	}
	sc.NextRunAt = next
	// The schedule was just configured: an error from an earlier configuration no
	// longer describes it.
	sc.LastError = nil
	return nil, nil
}

// scheduleWorkflow loads the schedule's workflow, adding a detail when it is missing
// or cannot be scheduled. Shipped template rows name agents and must be copied first;
// an agent's hidden quick-run workflow may be scheduled.
func (s *Server) scheduleWorkflow(ctx context.Context, id string, fe *fieldErrors) (*store.Workflow, error) {
	if id == "" {
		fe.add("workflow_id", "is required")
		return nil, nil
	}
	wf, err := store.GetWorkflow(ctx, s.pool, id)
	if errors.Is(err, store.ErrNotFound) {
		fe.add("workflow_id", "no workflow with id '%s'", id)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if wf.TemplateKey != nil && !strings.HasPrefix(*wf.TemplateKey, engine.QuickRunTemplateKey("")) {
		fe.add("workflow_id", "is a template; create an editable copy and schedule that")
		return nil, nil
	}
	return wf, nil
}

var schedulePrinter = message.NewPrinter(language.English)

// scheduleInputErrors checks the input against the workflow's input_schema the way
// the trigger will (DESIGN 5.6 rule 11): one detail per violated field, input.<field>.
// A schema that does not compile is the workflow's problem and is left to the trigger.
func scheduleInputErrors(wf *store.Workflow, input json.RawMessage) fieldErrors {
	def, err := workflow.Parse(wf.Definition)
	if err != nil {
		return nil
	}
	if t := bytes.TrimSpace(def.InputSchema); len(t) == 0 || string(t) == "null" {
		return nil
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(def.InputSchema))
	if err != nil {
		return nil
	}
	c := jsonschema.NewCompiler()
	if c.AddResource("input-schema.json", doc) != nil {
		return nil
	}
	schema, err := c.Compile("input-schema.json")
	if err != nil {
		return nil
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(input))
	if err != nil {
		return fieldErrors{{Path: "input", Message: "must be valid JSON"}}
	}
	var ve *jsonschema.ValidationError
	if err := schema.Validate(inst); !errors.As(err, &ve) {
		return nil
	}
	var fe fieldErrors
	var walk func(v *jsonschema.ValidationError)
	walk = func(v *jsonschema.ValidationError) {
		if len(v.Causes) == 0 {
			path := "input"
			for _, p := range v.InstanceLocation {
				path += "." + p
			}
			fe = append(fe, fieldError{Path: path, Message: v.ErrorKind.LocalizedString(schedulePrinter)})
			return
		}
		for _, c := range v.Causes {
			walk(c)
		}
	}
	walk(ve)
	return fe
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	list, err := store.ListSchedules(r.Context(), s.pool)
	if err != nil {
		s.internalError(w, r, "list schedules", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request) {
	sc, err := store.GetSchedule(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "schedule", err)
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	var in scheduleInput
	if !decodeBody(w, r, &in, maxScheduleBody) {
		return
	}
	sc := &store.Schedule{Enabled: true}
	fe, err := s.applySchedule(r.Context(), &in, sc, scheduleNow())
	if err != nil {
		s.internalError(w, r, "create schedule", err)
		return
	}
	if len(fe) > 0 {
		writeValidation(w, "invalid schedule", fe)
		return
	}
	if err := store.CreateSchedule(r.Context(), s.pool, sc); err != nil {
		s.internalError(w, r, "create schedule", err)
		return
	}
	s.respondSchedule(w, r, sc.ID, http.StatusCreated)
}

// updateSchedule merges the body onto the stored schedule and recomputes next_run_at
// from now (DESIGN 5.9 rule 1).
func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request) {
	sc, err := store.GetSchedule(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "schedule", err)
		return
	}
	var in scheduleInput
	if !decodeBody(w, r, &in, maxScheduleBody) {
		return
	}
	fe, err := s.applySchedule(r.Context(), &in, sc, scheduleNow())
	if err != nil {
		s.internalError(w, r, "update schedule", err)
		return
	}
	if len(fe) > 0 {
		writeValidation(w, "invalid schedule", fe)
		return
	}
	if err := store.UpdateSchedule(r.Context(), s.pool, sc); err != nil {
		s.storeError(w, r, "schedule", err)
		return
	}
	s.respondSchedule(w, r, sc.ID, http.StatusOK)
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	if err := store.DeleteSchedule(r.Context(), s.pool, r.PathValue("id")); err != nil {
		s.storeError(w, r, "schedule", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) respondSchedule(w http.ResponseWriter, r *http.Request, id string, status int) {
	sc, err := store.GetSchedule(r.Context(), s.pool, id)
	if err != nil {
		s.storeError(w, r, "schedule", err)
		return
	}
	writeJSON(w, status, sc)
}
