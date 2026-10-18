package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// Projects are the repositories runs work on (DESIGN 5.17): records with the git token
// sealed at rest and never returned.

const (
	maxProjectBody        = 64 << 10
	maxProjectNameLen     = 64
	maxProjectDescription = 1024
	maxProjectTokenLen    = 4096
	// maxProjectInstructions is the byte cap of the operator's instructions after
	// trimming (DESIGN 5.18 rule 1); maxProjectBudgetUSD is the largest cap (5.19 rule 1).
	maxProjectInstructions = 8192
	maxProjectBudgetUSD    = 1000000
)

// projectInput is ProjectInput; the read-only Project fields are accepted and ignored
// so a client can PUT back what it fetched. Token is write-only (DESIGN 5.17 rule 2).
type projectInput struct {
	Name           string `json:"name"`
	RepoURL        string `json:"repo_url"`
	Provider       string `json:"provider"`
	DefaultBranch  string `json:"default_branch"`
	SandboxNetwork bool   `json:"sandbox_network"`
	Description    string `json:"description"`
	Token          string `json:"token"`
	// Instructions and the two caps (USD) replace the stored ones on an update: absent
	// clears them (DESIGN 5.18 rule 1, 5.19 rule 1). The caps are read as numbers and
	// converted to micro-USD once checked, so a value too large for the micro-USD
	// integer is still answered with the rule's text.
	Instructions       string   `json:"instructions"`
	MaxCostUSDPerDay   *float64 `json:"max_cost_usd_per_day"`
	MaxCostUSDPerMonth *float64 `json:"max_cost_usd_per_month"`

	// Intake is the project's issue intake settings (DESIGN 5.23 rule 1): present
	// replaces the three, absent on an update keeps the stored ones (switched on purpose,
	// not cleared by a form that does not know it). applyIntake checks and applies it.
	Intake *intakeInput `json:"intake"`

	ID        json.RawMessage `json:"id"`
	TokenSet  json.RawMessage `json:"token_set"`
	CreatedAt json.RawMessage `json:"created_at"`
	UpdatedAt json.RawMessage `json:"updated_at"`
	LastRun   json.RawMessage `json:"last_run"`
	Spend     json.RawMessage `json:"spend"`
}

// intakeInput is ProjectIntakeInput. Label is a pointer to tell an absent member (the
// default, yuno) from an empty one (422). The members after WorkflowID belong to the
// Project view of intake and are accepted and ignored, so a client can PUT back what it
// fetched.
type intakeInput struct {
	Enabled    bool    `json:"enabled"`
	Label      *string `json:"label"`
	WorkflowID string  `json:"workflow_id"`

	WorkflowName json.RawMessage `json:"workflow_name"`
	PolledAt     json.RawMessage `json:"polled_at"`
	LastError    json.RawMessage `json:"last_error"`
}

// validate checks the input (DESIGN 5.17 rules 1 to 3) and returns the project it
// describes, provider resolved and branch defaulted, with the trimmed token ("" for
// none). A create needs a token; an update without one keeps the stored token.
func (in *projectInput) validate(create bool) (*store.Project, string, fieldErrors) {
	var fe fieldErrors
	p := &store.Project{Name: strings.TrimSpace(in.Name), SandboxNetwork: in.SandboxNetwork, Description: in.Description}
	switch n := utf8.RuneCountInString(p.Name); {
	case n == 0:
		fe.add("name", "is required")
	case n > maxProjectNameLen:
		fe.add("name", "must be at most %d characters", maxProjectNameLen)
	case hasControl(p.Name):
		fe.add("name", "must not contain control characters")
	}
	if strings.TrimSpace(in.RepoURL) == "" {
		fe.add("repo_url", "is required")
	} else if u, err := project.ValidateRepoURL(in.RepoURL); err != nil {
		// A URL that does not parse is not echoed: it may carry credentials.
		var ue *url.Error
		if errors.As(err, &ue) {
			fe.add("repo_url", "is not a valid URL")
		} else {
			fe.add("repo_url", "%s", err.Error())
		}
	} else {
		p.RepoURL = u
	}
	if prov, err := project.ParseProvider(in.Provider); err != nil {
		fe.add("provider", "%s", err.Error())
	} else {
		if prov == "" && p.RepoURL != "" {
			prov = project.InferProvider(p.RepoURL)
		}
		p.Provider = string(prov)
	}
	if b, err := project.ValidateBranch(in.DefaultBranch); err != nil {
		fe.add("default_branch", "%s", err.Error())
	} else {
		p.DefaultBranch = b
	}
	if utf8.RuneCountInString(in.Description) > maxProjectDescription {
		fe.add("description", "must be at most %d characters", maxProjectDescription)
	} else if strings.ContainsFunc(in.Description, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		fe.add("description", "must not contain control characters")
	}
	// DESIGN 5.18 rule 1: trimmed, at most 8192 bytes after trimming, no control
	// character but newline and tab; whitespace only is empty.
	p.Instructions = strings.TrimSpace(in.Instructions)
	if len(p.Instructions) > maxProjectInstructions {
		fe.add("instructions", "at most %d bytes", maxProjectInstructions)
	}
	if strings.ContainsFunc(p.Instructions, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		fe.add("instructions", "contains control characters")
	}
	p.BudgetDayUSDMicros = budgetCap(&fe, "max_cost_usd_per_day", in.MaxCostUSDPerDay)
	p.BudgetMonthUSDMicros = budgetCap(&fe, "max_cost_usd_per_month", in.MaxCostUSDPerMonth)
	token := strings.TrimSpace(in.Token)
	switch {
	case token == "" && create:
		fe.add("token", "is required: a git token with write access to the repository")
	case len(token) > maxProjectTokenLen:
		fe.add("token", "must be at most %d characters", maxProjectTokenLen)
	case strings.IndexFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		fe.add("token", "must not contain spaces or line breaks")
	}
	return p, token, fe
}

// budgetCap checks one budget cap (DESIGN 5.19 rule 1): absent or null clears it, a set
// one is above 0 and at most 1000000 USD. It returns the cap in micro-USD, rounded to
// the micro as the API boundary does elsewhere; a cap too small to be one micro is not
// above 0.
func budgetCap(fe *fieldErrors, field string, usd *float64) *store.USDMicros {
	if usd == nil {
		return nil
	}
	// The bound comes first: a number beyond the range of micro-USD must not be converted.
	if *usd > maxProjectBudgetUSD {
		fe.add(field, "at most %d", maxProjectBudgetUSD)
		return nil
	}
	micros := store.USDMicros(math.Round(*usd * 1e6))
	if *usd <= 0 || micros <= 0 {
		fe.add(field, "must be greater than 0")
		return nil
	}
	return &micros
}

// budgetRefusal answers a trigger refused by a project's budget (DESIGN 5.19 rule 3):
// 429 project_budget_reached with the refusal's text and the window's numbers in USD.
// engineError maps *project.BudgetError to it.
func budgetRefusal(w http.ResponseWriter, be *project.BudgetError) {
	writeError(w, http.StatusTooManyRequests, "project_budget_reached", be.Error(), map[string]any{
		"project_id":   be.ProjectID,
		"window":       be.Window,
		"max_cost_usd": store.USDMicros(be.CapMicros).USD(),
		"spent_usd":    store.USDMicros(be.SpendMicros).USD(),
		"until":        be.WindowEnd.UTC().Format(time.RFC3339),
	})
}

// hasControl reports a control character (a NUL would fail in Postgres with a 500).
func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

// hostOf is the lower-cased host of a clone URL, "" when it does not parse.
func hostOf(repoURL string) string {
	u, err := url.Parse(repoURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// writeTokensUnavailable answers a write that would store a token without
// PROJECT_TOKEN_KEY (DESIGN 5.17 rule 2).
func writeTokensUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, "project_tokens_unavailable", "PROJECT_TOKEN_KEY is not set; no token can be stored", nil)
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := store.ListProjects(ctx, s.pool)
	if err == nil {
		err = store.FillProjectLastRuns(ctx, s.pool, list)
	}
	if err == nil {
		err = store.FillProjectSpend(ctx, s.pool, list, time.Now().UTC())
	}
	if err != nil {
		s.internalError(w, r, "list projects", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.projectView(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "project", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// projectView is a live project with its last run (DESIGN 5.17 rule 3) and its spend in
// the current UTC day and month (DESIGN 5.19 rule 7).
func (s *Server) projectView(ctx context.Context, id string) (*store.Project, error) {
	p, err := store.GetProject(ctx, s.pool, id)
	if err != nil {
		return nil, err
	}
	one := []store.Project{*p}
	if err := store.FillProjectLastRuns(ctx, s.pool, one); err != nil {
		return nil, err
	}
	if err := store.FillProjectSpend(ctx, s.pool, one, time.Now().UTC()); err != nil {
		return nil, err
	}
	return &one[0], nil
}

// createProject stores a project with its token sealed under the new id (DESIGN 5.17
// rules 1 to 3); the answer carries token_set, never the token.
func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in projectInput
	if !decodeBody(w, r, &in, maxProjectBody) {
		return
	}
	p, token, fe := in.validate(true)
	ife, err := s.applyIntake(ctx, &in, p, nil)
	if err != nil {
		s.internalError(w, r, "check the intake settings", err)
		return
	}
	if fe = append(fe, ife...); len(fe) > 0 {
		writeValidation(w, "invalid project", fe)
		return
	}
	if s.cipher == nil {
		writeTokensUnavailable(w)
		return
	}
	if fe, err := s.projectNameTaken(ctx, p.Name, ""); err != nil || len(fe) > 0 {
		s.projectNameError(w, r, fe, err)
		return
	}
	p.ID = store.NewID()
	blob, err := s.cipher.Seal(p.ID, token)
	if err != nil {
		s.internalError(w, r, "seal project token", err)
		return
	}
	if err := store.CreateProject(ctx, s.pool, p, blob); err != nil {
		if isUniqueViolation(err) {
			writeValidation(w, "invalid project", projectNameTakenErrors(p.Name))
			return
		}
		s.internalError(w, r, "create project", err)
		return
	}
	// The answer is the project as a get shows it, spend included (DESIGN 5.19 rule 7).
	created, err := s.projectView(ctx, p.ID)
	if err != nil {
		s.internalError(w, r, "read the created project", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// updateProject replaces a project's settings. An absent or empty token keeps the
// stored one; a new one is sealed and replaces it in the same transaction, and runs in
// flight read it when they next clone or push (DESIGN 5.17 rules 2 and 6).
func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cur, err := store.GetProject(ctx, s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "project", err)
		return
	}
	var in projectInput
	if !decodeBody(w, r, &in, maxProjectBody) {
		return
	}
	next, token, fe := in.validate(false)
	ife, err := s.applyIntake(ctx, &in, next, cur)
	if err != nil {
		s.internalError(w, r, "check the intake settings", err)
		return
	}
	if fe = append(fe, ife...); len(fe) > 0 {
		writeValidation(w, "invalid project", fe)
		return
	}
	if token != "" && s.cipher == nil {
		writeTokensUnavailable(w)
		return
	}
	// DESIGN 5.17 rule 2: the stored token is for the host it was entered for; a change
	// of host or provider could otherwise aim it at another server through the check or
	// a clone, which is the one way an API caller could read a write-only token.
	if token == "" && (hostOf(next.RepoURL) != hostOf(cur.RepoURL) || next.Provider != cur.Provider) {
		writeValidation(w, "invalid project", fieldErrors{{Path: "token", Message: "enter the token again when the repository host or the provider changes"}})
		return
	}
	if fe, err := s.projectNameTaken(ctx, next.Name, cur.ID); err != nil || len(fe) > 0 {
		s.projectNameError(w, r, fe, err)
		return
	}
	next.ID = cur.ID
	var blob []byte
	if token != "" {
		if blob, err = s.cipher.Seal(cur.ID, token); err != nil {
			s.internalError(w, r, "seal project token", err)
			return
		}
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := store.UpdateProject(ctx, tx, next); err != nil {
			return err
		}
		if blob != nil {
			return store.SetProjectToken(ctx, tx, cur.ID, blob)
		}
		return nil
	})
	if isUniqueViolation(err) {
		writeValidation(w, "invalid project", projectNameTakenErrors(next.Name))
		return
	}
	if err != nil {
		s.storeError(w, r, "project", err)
		return
	}
	saved, err := s.projectView(ctx, cur.ID)
	if err != nil {
		s.storeError(w, r, "project", err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// deleteProject is a soft delete (DESIGN 5.17 rule 3): the project leaves the lists,
// runs that name it keep their snapshot, and the row keeps its token so a run in
// flight still clones and pushes.
func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	if err := store.DeleteProject(r.Context(), s.pool, r.PathValue("id")); err != nil {
		s.storeError(w, r, "project", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkProject verifies the stored token against the provider without a clone (DESIGN
// 5.17 rule 5). Everything that goes wrong after the project is found, a token that no
// longer opens included, is a result with ok false, not an HTTP error.
func (s *Server) checkProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := store.GetProject(ctx, s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "project", err)
		return
	}
	if !p.TokenSet {
		writeJSON(w, http.StatusOK, checkFailed("No token is stored; enter one on the Projects page."))
		return
	}
	token, err := s.projects.Token(ctx, p.ID)
	switch {
	case errors.Is(err, project.ErrNoKey):
		writeJSON(w, http.StatusOK, checkFailed("PROJECT_TOKEN_KEY is not set; the stored token cannot be read."))
		return
	case errors.Is(err, project.ErrCannotRead):
		writeJSON(w, http.StatusOK, checkFailed(sentence(project.ErrCannotRead.Error())))
		return
	case err != nil:
		s.internalError(w, r, "read project token", err)
		return
	}
	res := tools.CheckProject(ctx, s.checkClient, project.ConfigOf(p.Settings(), token), "")
	s.log.Info("project checked", "project_id", p.ID, "ok", res.OK)
	writeJSON(w, http.StatusOK, res)
}

func checkFailed(message string) tools.CheckResult {
	return tools.CheckResult{Message: message, CheckedAt: time.Now().UTC()}
}

// sentence capitalises a message and ends it with a period.
func sentence(msg string) string {
	if msg == "" {
		return msg
	}
	msg = strings.ToUpper(msg[:1]) + msg[1:]
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return msg
}

// projectNameTaken reports a name another live project holds, ignoring case (DESIGN
// 5.17 rule 1); the unique index settles a race.
func (s *Server) projectNameTaken(ctx context.Context, name, selfID string) (fieldErrors, error) {
	other, err := store.GetProjectByName(ctx, s.pool, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if other.ID != selfID {
		return projectNameTakenErrors(name), nil
	}
	return nil, nil
}

func (s *Server) projectNameError(w http.ResponseWriter, r *http.Request, fe fieldErrors, err error) {
	if err != nil {
		s.internalError(w, r, "project name", err)
		return
	}
	writeValidation(w, "invalid project", fe)
}

func projectNameTakenErrors(name string) fieldErrors {
	return fieldErrors{{Path: "name", Message: fmt.Sprintf("a project named %q already exists", name)}}
}
