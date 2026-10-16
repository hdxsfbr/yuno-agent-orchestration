package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"yuno/internal/store"
)

func decodeSkill(t *testing.T, b []byte) store.Skill {
	t.Helper()
	var s store.Skill
	if err := json.Unmarshal(b, &s); err != nil || s.ID == "" {
		t.Fatalf("not a skill: %s", b)
	}
	return s
}

func agentWithSkills(t *testing.T, name string, skills ...string) *store.Agent {
	t.Helper()
	a := &store.Agent{Name: name, Model: "claude-opus-5-5", Skills: skills}
	mustDo(t, store.CreateAgent(context.Background(), testPool, a))
	return a
}

func TestSkillCRUD(t *testing.T) {
	pool := needDB(t)
	fe := &fakeEngine{pool: pool}
	_, ts := newTestServer(t, pool, fe, nil)
	name := "pdf-reports-" + uniqueSuffix()

	code, body := do(t, ts, http.MethodPost, "/api/skills", `{"name":"`+name+`","description":"Write PDF reports","body":"# PDF\nUse reportlab."}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	sk := decodeSkill(t, body)
	if sk.Name != name || sk.Description != "Write PDF reports" || !strings.Contains(sk.Body, "reportlab") {
		t.Fatalf("created = %+v", sk)
	}
	code, body = do(t, ts, http.MethodGet, "/api/skills/"+sk.ID, "")
	if code != http.StatusOK || decodeSkill(t, body).Name != name {
		t.Fatalf("get: %d %s", code, body)
	}
	code, body = do(t, ts, http.MethodGet, "/api/skills", "")
	var list []store.Skill
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil || !slices.ContainsFunc(list, func(s store.Skill) bool { return s.ID == sk.ID }) {
		t.Fatalf("list: %d %s", code, body)
	}
	// Same name again is refused.
	code, body = do(t, ts, http.MethodPost, "/api/skills", `{"name":"`+name+`"}`)
	if code != http.StatusUnprocessableEntity || !slices.Equal(detailPaths(t, body), []string{"name"}) || !strings.Contains(string(body), "already exists") {
		t.Fatalf("duplicate: %d %s", code, body)
	}
	// PUT back what was fetched, with a new body.
	var back map[string]any
	_, body = do(t, ts, http.MethodGet, "/api/skills/"+sk.ID, "")
	mustDo(t, json.Unmarshal(body, &back))
	back["body"] = "# PDF\nUse weasyprint."
	raw, _ := json.Marshal(back)
	code, body = do(t, ts, http.MethodPut, "/api/skills/"+sk.ID, string(raw))
	if code != http.StatusOK || !strings.Contains(decodeSkill(t, body).Body, "weasyprint") {
		t.Fatalf("put: %d %s", code, body)
	}
	if code, _ := do(t, ts, http.MethodDelete, "/api/skills/"+sk.ID, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := do(t, ts, http.MethodGet, "/api/skills/"+sk.ID, ""); code != http.StatusNotFound {
		t.Fatalf("get deleted: %d", code)
	}
	if code, _ := do(t, ts, http.MethodDelete, "/api/skills/"+sk.ID, ""); code != http.StatusNotFound {
		t.Fatalf("delete twice: %d", code)
	}
}

func TestSkillValidation(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	for _, tc := range []struct {
		body string
		want []string
	}{
		{`{"name":""}`, []string{"name"}},
		{`{"name":"PDF Reports"}`, []string{"name"}},
		{`{"name":"pdf--reports"}`, []string{"name"}},
		{`{"name":"-pdf"}`, []string{"name"}},
		{`{"name":"pdf_reports"}`, []string{"name"}},
		{`{"name":"` + strings.Repeat("a", 65) + `"}`, []string{"name"}},
		{`{"name":"ok-name","description":"` + strings.Repeat("d", 1025) + `"}`, []string{"description"}},
		{`{"name":"ok-name","color":"red"}`, []string{"color"}},
	} {
		code, body := do(t, ts, http.MethodPost, "/api/skills", tc.body)
		if code != http.StatusUnprocessableEntity || !slices.Equal(detailPaths(t, body), tc.want) {
			t.Errorf("%.60s: %d %s", tc.body, code, body)
		}
	}
	for _, ok := range []string{"pdf", "pdf-reports", "a1-b2-c3"} {
		if !skillName.MatchString(ok) {
			t.Errorf("%q refused", ok)
		}
	}
}

// A rename is carried into the agents that list the skill, a delete takes it out of
// them, and in both cases those agents, and only those, are re-materialised.
func TestSkillChangeRematerialisesUsers(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	fe := &fakeEngine{pool: pool}
	_, ts := newTestServer(t, pool, fe, nil)
	sfx := uniqueSuffix()
	old, keep := "lint-"+sfx, "keep-"+sfx
	for _, n := range []string{old, keep} {
		if code, body := do(t, ts, http.MethodPost, "/api/skills", `{"name":"`+n+`"}`); code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", n, code, body)
		}
	}
	list, err := store.ListSkillsByName(ctx, pool, []string{old})
	mustDo(t, err)
	skillID := list[0].ID
	user := agentWithSkills(t, "skill-user", keep, old)
	other := agentWithSkills(t, "skill-other", keep)

	renamed := "style-" + sfx
	code, body := do(t, ts, http.MethodPut, "/api/skills/"+skillID, `{"name":"`+renamed+`","body":"Run ruff."}`)
	if code != http.StatusOK {
		t.Fatalf("rename: %d %s", code, body)
	}
	got, err := store.GetAgent(ctx, pool, user.ID)
	mustDo(t, err)
	if !slices.Equal(got.Skills, []string{keep, renamed}) {
		t.Fatalf("user skills after rename = %v", got.Skills)
	}
	if !slices.Equal(fe.materialized, []string{user.ID}) {
		t.Fatalf("materialised %v, want only the user", fe.materialized)
	}

	fe.materialized = nil
	if code, body := do(t, ts, http.MethodDelete, "/api/skills/"+skillID, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, body)
	}
	got, err = store.GetAgent(ctx, pool, user.ID)
	mustDo(t, err)
	if !slices.Equal(got.Skills, []string{keep}) {
		t.Fatalf("user skills after delete = %v", got.Skills)
	}
	if o, _ := store.GetAgent(ctx, pool, other.ID); !slices.Equal(o.Skills, []string{keep}) {
		t.Fatalf("other agent changed: %v", o.Skills)
	}
	if !slices.Equal(fe.materialized, []string{user.ID}) {
		t.Fatalf("materialised %v after delete", fe.materialized)
	}

	// A push that fails after the change was saved is a 502 naming the agents.
	ks, _ := store.ListSkillsByName(ctx, pool, []string{keep})
	fe.materializeErr = errors.New("gateway unavailable")
	code, body = do(t, ts, http.MethodPut, "/api/skills/"+ks[0].ID, `{"name":"`+keep+`","body":"changed"}`)
	if code != http.StatusBadGateway || !strings.Contains(string(body), user.ID) || !strings.Contains(string(body), other.ID) {
		t.Fatalf("failed push: %d %s", code, body)
	}
	if s, _ := store.GetSkill(ctx, pool, ks[0].ID); s.Body != "changed" {
		t.Fatal("skill not saved before the push failed")
	}
}

// The agent's skills list is checked against the skills that exist.
func TestCheckAgentSkills(t *testing.T) {
	pool := needDB(t)
	s, _ := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	name := "known-" + uniqueSuffix()
	mustDo(t, store.CreateSkill(context.Background(), pool, &store.Skill{Name: name}))
	fe, err := s.checkAgentSkills(context.Background(), []string{name, "missing-skill-" + uniqueSuffix(), "Bad Name", name})
	mustDo(t, err)
	got := map[string]string{}
	for _, f := range fe {
		got[f.Path] = f.Message
	}
	if len(got) != 3 || !strings.HasPrefix(got["skills[1]"], "no skill named") || got["skills[2]"] == "" || !strings.Contains(got["skills[3]"], "listed twice") {
		t.Fatalf("problems = %v", got)
	}
	if fe, err := s.checkAgentSkills(context.Background(), []string{name}); err != nil || len(fe) != 0 {
		t.Fatalf("valid list: %v %v", fe, err)
	}
	if fe, err := s.checkAgentSkills(context.Background(), nil); err != nil || len(fe) != 0 {
		t.Fatalf("empty list: %v %v", fe, err)
	}
}
