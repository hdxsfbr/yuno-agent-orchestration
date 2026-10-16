package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"yuno/internal/obs"
	"yuno/internal/store"
)

const (
	maxSkillBody        = 512 << 10
	maxSkillNameLen     = 64
	maxSkillDescription = 1024
	maxSkillBodyBytes   = 256 << 10
)

// skillName is the skills table's name rule (DESIGN section 4); it is also the
// directory name under skills/ in the agent's workspace.
var skillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// skillInput is SkillInput; the read-only Skill fields are accepted and ignored so a
// client can PUT back what it fetched.
type skillInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`

	ID        json.RawMessage `json:"id"`
	CreatedAt json.RawMessage `json:"created_at"`
	UpdatedAt json.RawMessage `json:"updated_at"`
}

func (in *skillInput) validate() fieldErrors {
	var fe fieldErrors
	checkSkillName(&fe, "name", in.Name)
	if utf8.RuneCountInString(in.Description) > maxSkillDescription {
		fe.add("description", "must be at most %d characters", maxSkillDescription)
	}
	if len(in.Body) > maxSkillBodyBytes {
		fe.add("body", "must be at most %d bytes", maxSkillBodyBytes)
	}
	return fe
}

func checkSkillName(fe *fieldErrors, path, name string) {
	switch {
	case name == "":
		fe.add(path, "is required")
	case len(name) > maxSkillNameLen:
		fe.add(path, "must be at most %d characters", maxSkillNameLen)
	case !skillName.MatchString(name):
		fe.add(path, "must be lowercase letters and digits in words separated by single hyphens, e.g. pdf-reports")
	}
}

// checkAgentSkills validates an agent's skills list against the skills that exist
// (paths skills[i]); the agent form's save calls it next to its own checks.
func (s *Server) checkAgentSkills(ctx context.Context, names []string) (fieldErrors, error) {
	var fe fieldErrors
	if len(names) == 0 {
		return nil, nil
	}
	existing, err := store.ListSkillsByName(ctx, s.pool, names)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, sk := range existing {
		known[sk.Name] = true
	}
	for i, n := range names {
		path := fmt.Sprintf("skills[%d]", i)
		switch {
		case !skillName.MatchString(n):
			checkSkillName(&fe, path, n)
		case slices.Index(names, n) < i:
			fe.add(path, "%q is listed twice", n)
		case !known[n]:
			fe.add(path, "no skill named %q", n)
		}
	}
	return fe, nil
}

func (s *Server) listSkills(w http.ResponseWriter, r *http.Request) {
	list, err := store.ListSkills(r.Context(), s.pool)
	if err != nil {
		s.internalError(w, r, "list skills", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getSkill(w http.ResponseWriter, r *http.Request) {
	sk, err := store.GetSkill(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "skill", err)
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

func (s *Server) createSkill(w http.ResponseWriter, r *http.Request) {
	var in skillInput
	if !decodeBody(w, r, &in, maxSkillBody) {
		return
	}
	if fe := in.validate(); len(fe) > 0 {
		writeValidation(w, "invalid skill", fe)
		return
	}
	if fe, err := s.skillNameTaken(r.Context(), in.Name, ""); err != nil || len(fe) > 0 {
		s.skillNameError(w, r, fe, err)
		return
	}
	sk := &store.Skill{Name: in.Name, Description: in.Description, Body: in.Body}
	if err := store.CreateSkill(r.Context(), s.pool, sk); err != nil {
		if isUniqueViolation(err) {
			writeValidation(w, "invalid skill", nameTakenErrors(in.Name))
			return
		}
		s.internalError(w, r, "create skill", err)
		return
	}
	writeJSON(w, http.StatusCreated, sk)
}

// updateSkill replaces a skill. A rename is carried into every agent's skills list in
// the same transaction, and every agent that uses the skill is re-materialised so its
// workspace gets the new SKILL.md (DESIGN 5.14 rule 4).
func (s *Server) updateSkill(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cur, err := store.GetSkill(ctx, s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "skill", err)
		return
	}
	var in skillInput
	if !decodeBody(w, r, &in, maxSkillBody) {
		return
	}
	if fe := in.validate(); len(fe) > 0 {
		writeValidation(w, "invalid skill", fe)
		return
	}
	if fe, err := s.skillNameTaken(ctx, in.Name, cur.ID); err != nil || len(fe) > 0 {
		s.skillNameError(w, r, fe, err)
		return
	}
	next := &store.Skill{ID: cur.ID, Name: in.Name, Description: in.Description, Body: in.Body}
	var users []string
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := store.UpdateSkill(ctx, tx, next); err != nil {
			return err
		}
		var err error
		users, err = renameAgentSkill(ctx, tx, cur.Name, next.Name)
		return err
	})
	if isUniqueViolation(err) {
		writeValidation(w, "invalid skill", nameTakenErrors(in.Name))
		return
	}
	if err != nil {
		s.storeError(w, r, "skill", err)
		return
	}
	saved, err := store.GetSkill(ctx, s.pool, cur.ID)
	if err != nil {
		s.storeError(w, r, "skill", err)
		return
	}
	if !s.rematerialize(w, r, cur.ID, users) {
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// deleteSkill removes a skill and takes it out of every agent that listed it; those
// agents are re-materialised, which removes skills/<name>/ from their workspaces.
func (s *Server) deleteSkill(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cur, err := store.GetSkill(ctx, s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "skill", err)
		return
	}
	var users []string
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := store.DeleteSkill(ctx, tx, cur.ID); err != nil {
			return err
		}
		var err error
		users, err = renameAgentSkill(ctx, tx, cur.Name, "")
		return err
	})
	if err != nil {
		s.storeError(w, r, "skill", err)
		return
	}
	if !s.rematerialize(w, r, cur.ID, users) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// renameAgentSkill rewrites the skill name in every live agent's skills list (to ""
// removes it) and returns the ids of the agents that list it.
func renameAgentSkill(ctx context.Context, tx pgx.Tx, from, to string) ([]string, error) {
	agents, err := store.ListAgents(ctx, tx)
	if err != nil {
		return nil, err
	}
	var users []string
	for i := range agents {
		a := &agents[i]
		if !slices.Contains(a.Skills, from) {
			continue
		}
		users = append(users, a.ID)
		if from == to {
			continue
		}
		var skills []string
		for _, n := range a.Skills {
			switch {
			case n != from:
				skills = append(skills, n)
			case to != "" && !slices.Contains(a.Skills, to):
				skills = append(skills, to)
			}
		}
		a.Skills = skills
		if err := store.UpdateAgent(ctx, tx, a); err != nil {
			return nil, fmt.Errorf("agent %s: %w", a.ID, err)
		}
	}
	return users, nil
}

// rematerialize pushes every agent that uses a changed skill. The skill change is
// already saved, so a failure answers 502 with the skill id and the agents that were
// not pushed (DESIGN section 6), as an agent save does.
func (s *Server) rematerialize(w http.ResponseWriter, r *http.Request, skillID string, agentIDs []string) bool {
	var failed []string
	var lastErr error
	for _, id := range agentIDs {
		if _, err := s.materialize(r.Context(), id); err != nil {
			s.log.Warn("skill saved but agent not materialised", "skill_id", skillID, "agent_id", id, "error", err)
			failed, lastErr = append(failed, id), err
		}
	}
	if len(failed) == 0 {
		return true
	}
	writeError(w, http.StatusBadGateway, "materialize_failed",
		"the skill was saved but agents using it could not be pushed to OpenClaw: "+obs.Redact(lastErr.Error()),
		map[string]any{"id": skillID, "agent_ids": failed})
	return false
}

// skillNameTaken reports a name another skill already has.
func (s *Server) skillNameTaken(ctx context.Context, name, selfID string) (fieldErrors, error) {
	same, err := store.ListSkillsByName(ctx, s.pool, []string{name})
	if err != nil {
		return nil, err
	}
	for _, sk := range same {
		if sk.ID != selfID {
			return nameTakenErrors(name), nil
		}
	}
	return nil, nil
}

func (s *Server) skillNameError(w http.ResponseWriter, r *http.Request, fe fieldErrors, err error) {
	if err != nil {
		s.internalError(w, r, "skill name", err)
		return
	}
	writeValidation(w, "invalid skill", fe)
}

// isUniqueViolation catches the race the name check cannot: two saves of one name at
// once both pass it and the unique index refuses the second.
func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func nameTakenErrors(name string) fieldErrors {
	return fieldErrors{{Path: "name", Message: fmt.Sprintf("a skill named %q already exists", name)}}
}
