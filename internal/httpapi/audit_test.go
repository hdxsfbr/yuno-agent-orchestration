package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"yuno/internal/actor"
	"yuno/internal/store"
)

// send makes a request with extra headers and returns the status, the headers and the body.
func send(t *testing.T, h http.Handler, ctx context.Context, method, path, body string, hdr map[string]string) (int, http.Header, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(ctx, method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Header(), rec.Body.Bytes()
}

// auditRows are the audit rows of a path, oldest first.
func auditRows(t *testing.T, path string) []store.AuditEvent {
	t.Helper()
	ctx := context.Background()
	rows, err := testPool.Query(ctx, `SELECT id FROM audit_events WHERE path = $1 ORDER BY at, id`, path)
	mustDo(t, err)
	var ids []string
	for rows.Next() {
		var id string
		mustDo(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]store.AuditEvent, 0, len(ids))
	for _, id := range ids {
		e, err := store.GetAuditEvent(ctx, testPool, id)
		mustDo(t, err)
		out = append(out, *e)
	}
	return out
}

// jsonField reads one top-level string field of a JSON document.
func jsonField(t *testing.T, raw json.RawMessage, field string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not a JSON object: %s", raw)
	}
	s, _ := m[field].(string)
	return s
}

func chiefActor(agentID string) actor.Actor {
	return actor.Actor{Kind: actor.Agent, ID: agentID, Name: "Chief", Via: actor.ViaChief, OnBehalfOf: "telegram:1001"}
}

// DESIGN 5.31 rules 1 and 2: one row per change, whoever makes it (the UI, an API client,
// the Chief through an in-process dispatch); none for a read.
func TestAuditRowPerMutation(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	fe := &fakeEngine{pool: pool}
	s, _ := newTestServer(t, pool, fe, nil)
	h := s.Handler()
	sfx := uniqueSuffix()
	agentID := testAgent(t, "audit-"+sfx)
	path := "/api/agents/" + agentID

	put := func(c context.Context, name string, hdr map[string]string) {
		t.Helper()
		if code, _, body := send(t, h, c, http.MethodPut, path, `{"name":"`+name+`","model":"claude-opus-5-5"}`, hdr); code != http.StatusOK {
			t.Fatalf("PUT %s: %d %s", name, code, body)
		}
	}
	put(ctx, "audit-ui-"+sfx, map[string]string{"X-Yuno-Client": "web"})
	put(ctx, "audit-api-"+sfx, nil)
	// The Chief's dispatch carries its actor; a header cannot change it.
	put(actor.With(ctx, chiefActor("chief-"+sfx)), "audit-chief-"+sfx, map[string]string{"X-Yuno-Client": "web"})
	if code, _, _ := send(t, h, ctx, http.MethodGet, path, "", nil); code != http.StatusOK {
		t.Fatalf("GET %d", code)
	}

	rows := auditRows(t, path)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (none for the GET): %+v", len(rows), rows)
	}
	want := []struct{ kind, id, name, via, behalf, before, after string }{
		{"operator", "", "", "ui", "", "audit-" + sfx, "audit-ui-" + sfx},
		{"operator", "", "", "api", "", "audit-ui-" + sfx, "audit-api-" + sfx},
		{"agent", "chief-" + sfx, "Chief", "chief", "telegram:1001", "audit-api-" + sfx, "audit-chief-" + sfx},
	}
	for i, w := range want {
		e := rows[i]
		if e.ActorKind != w.kind || e.ActorID != w.id || e.ActorName != w.name || e.Via != w.via || e.OnBehalfOf != w.behalf {
			t.Errorf("row %d actor = %s %q %q %s %q", i, e.ActorKind, e.ActorID, e.ActorName, e.Via, e.OnBehalfOf)
		}
		if e.Method != http.MethodPut || e.Route != "/api/agents/{id}" || e.Target != path || e.Status == nil || *e.Status != 200 ||
			e.Outcome != store.AuditOK || e.ErrorCode != "" || e.RequestID == "" || e.DurationMs == nil {
			t.Errorf("row %d = %+v", i, e)
		}
		if got := jsonField(t, e.Request, "name"); got != w.after {
			t.Errorf("row %d request name %q", i, got)
		}
		if got := jsonField(t, e.Before, "name"); got != w.before {
			t.Errorf("row %d before name %q, want %q", i, got, w.before)
		}
		if got := jsonField(t, e.After, "name"); got != w.after {
			t.Errorf("row %d after name %q, want %q", i, got, w.after)
		}
	}

	// A POST to a collection has no before; its after is the response.
	code, _, body := send(t, h, ctx, http.MethodPost, "/api/agents", `{"name":"audit-new-`+sfx+`","model":"claude-opus-5-5"}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/agents: %d %s", code, body)
	}
	var created store.Agent
	mustDo(t, json.Unmarshal(body, &created))
	var rec *store.AuditEvent
	for _, e := range auditRows(t, "/api/agents") {
		var after struct{ ID string }
		if json.Unmarshal(e.After, &after) == nil && after.ID == created.ID {
			rec = &e
		}
	}
	if rec == nil || rec.Target != "/api/agents" || rec.Before != nil || rec.Outcome != store.AuditOK || *rec.Status != http.StatusCreated {
		t.Fatalf("create row = %+v", rec)
	}

	// An action's target is its parent: the run, read before and after the cancel.
	run, err := createTestRun(ctx, pool, store.TriggerManual, json.RawMessage(`{}`), store.Limits{MaxSteps: 5})
	mustDo(t, err)
	if code, _, body := send(t, h, ctx, http.MethodPost, "/api/runs/"+run.ID+"/cancel", "", nil); code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", code, body)
	}
	cancel := auditRows(t, "/api/runs/"+run.ID+"/cancel")
	if len(cancel) != 1 || cancel[0].Target != "/api/runs/"+run.ID || cancel[0].Route != "/api/runs/{id}/cancel" ||
		jsonField(t, cancel[0].Before, "id") != run.ID || jsonField(t, cancel[0].After, "id") != run.ID || cancel[0].Request != nil {
		t.Fatalf("cancel rows = %+v", cancel)
	}

	// An action with no target records the request and the response; a 404 failed.
	approve := "/api/approvals/no-such-approval-" + sfx + "/approve"
	if code, _, _ := send(t, h, ctx, http.MethodPost, approve, `{"reason":"ok"}`, nil); code != http.StatusNotFound {
		t.Fatalf("approve: %d", code)
	}
	ap := auditRows(t, approve)
	if len(ap) != 1 || ap[0].Target != "" || ap[0].Outcome != store.AuditFailed || ap[0].ErrorCode != "not_found" ||
		jsonField(t, ap[0].Request, "reason") != "ok" || !strings.Contains(string(ap[0].After), "not_found") {
		t.Fatalf("approve rows = %+v", ap)
	}
}

// DESIGN 5.31 rule 1: a request auth refused is recorded as anonymous and denied, its body
// unread; the platform gate's note sets the outcome, approval and evidence of a row.
func TestAuditDenied(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	cfg := testConfig()
	cfg.APIToken = "audit-test-api-token-not-a-secret"
	s, _ := newTestServerDeps(t, Deps{Cfg: cfg, Pool: pool}, &fakeEngine{pool: pool})
	h := s.Handler()
	sfx := uniqueSuffix()
	bearer := map[string]string{"Authorization": "Bearer " + cfg.APIToken}

	path := "/api/agents/refused-" + sfx
	if code, _, _ := send(t, h, ctx, http.MethodPut, path, `{"name":"x"}`, map[string]string{"Authorization": "Bearer wrong"}); code != http.StatusUnauthorized {
		t.Fatalf("PUT without the bearer: %d", code)
	}
	if code, _, _ := send(t, h, ctx, http.MethodGet, path, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("GET without the bearer: %d", code)
	}
	rows := auditRows(t, path)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	for i, e := range rows {
		if e.ActorKind != "anonymous" || e.Outcome != store.AuditDenied || e.Status == nil || *e.Status != http.StatusUnauthorized ||
			e.ErrorCode != "unauthorized" || e.Request != nil || e.Before != nil || e.Route != "/api/agents/{id}" {
			t.Errorf("row %d = %+v", i, e)
		}
	}
	// The document is open, and a read with the bearer is not audited.
	if code, _, _ := send(t, h, ctx, http.MethodGet, "/api/openapi.yaml", "", nil); code != http.StatusOK {
		t.Fatalf("openapi: %d", code)
	}
	if len(auditRows(t, "/api/openapi.yaml")) != 0 {
		t.Fatal("the open document was audited")
	}
	agentID := testAgent(t, "audit-denied-"+sfx)
	if code, _, _ := send(t, h, ctx, http.MethodGet, "/api/agents/"+agentID, "", bearer); code != http.StatusOK {
		t.Fatalf("GET with the bearer: %d", code)
	}
	if code, _, _ := send(t, h, ctx, http.MethodPut, "/api/agents/"+agentID, `{"name":"audit-denied-2-`+sfx+`","model":"claude-opus-5-5"}`, bearer); code != http.StatusOK {
		t.Fatalf("PUT with the bearer: %d", code)
	}
	if got := auditRows(t, "/api/agents/"+agentID); len(got) != 1 || got[0].ActorKind != "operator" || got[0].Via != "api" {
		t.Fatalf("rows with the bearer = %+v", got)
	}

	// The platform gate tells the row its outcome through the note.
	gate := func(outcome, code, approval string, status int) http.Handler {
		return s.auth(s.withActor(s.audit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := actor.NoteFrom(r.Context())
			n.Outcome, n.ErrorCode, n.ApprovalID = outcome, code, approval
			n.Evidence = map[string]any{"newest_message": "approve 4821", "token": "c_" + strings.Repeat("a", 32)}
			writeJSON(w, status, map[string]string{"status": outcome})
		}))))
	}
	agentCtx := actor.With(ctx, chiefActor("chief-"+sfx))
	for _, tc := range []struct {
		outcome, code, approval string
		status                  int
	}{
		{store.AuditDenied, "not_available_to_agents", "", http.StatusForbidden},
		{store.AuditPendingApproval, "", "appr-" + sfx, http.StatusAccepted},
	} {
		p := "/api/projects/gate-" + tc.outcome + "-" + sfx
		if code, _, _ := send(t, gate(tc.outcome, tc.code, tc.approval, tc.status), agentCtx, http.MethodDelete, p, "", bearer); code != tc.status {
			t.Fatalf("%s: %d", tc.outcome, code)
		}
		got := auditRows(t, p)
		if len(got) != 1 || got[0].Outcome != tc.outcome || got[0].ErrorCode != tc.code || got[0].ApprovalID != tc.approval ||
			got[0].ActorKind != "agent" || jsonField(t, got[0].Evidence, "newest_message") != "approve 4821" ||
			jsonField(t, got[0].Evidence, "token") != "[redacted]" {
			t.Fatalf("%s rows = %+v", tc.outcome, got)
		}
	}
}

// DESIGN 5.31 rule 1: when the row cannot be written, the change is refused 503 and does
// not run; reads still work.
func TestAuditFailClosed(t *testing.T) {
	needDB(t)
	ctx := context.Background()
	closed, err := store.Open(ctx, os.Getenv("DATABASE_URL"))
	mustDo(t, err)
	closed.Close()
	fe := &fakeEngine{}
	s, _ := newTestServer(t, closed, fe, nil)
	code, _, body := send(t, s.Handler(), ctx, http.MethodPost, "/api/runs", `{"agent_id":"ag","task":"t"}`, nil)
	if code != http.StatusServiceUnavailable || decodeError(t, body).Error.Code != "audit_unavailable" {
		t.Fatalf("POST with the audit down: %d %s", code, body)
	}
	if len(fe.triggers) != 0 {
		t.Fatalf("the run was started: %v", fe.triggers)
	}
	if code, _, _ := send(t, s.Handler(), ctx, http.MethodGet, "/api/status", "", nil); code != http.StatusOK {
		t.Fatalf("GET /api/status with the audit down: %d", code)
	}
}

// DESIGN 5.31 rules 1 and 2: request, before and after are redacted; a token never appears
// in the row, by its key or by its shape.
func TestAuditBeforeAfterRedacted(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	s, _ := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	sfx := uniqueSuffix()
	oldToken, newToken, shaped := acceptedTokenPrefix+"old-"+sfx, acceptedTokenPrefix+"new-"+sfx, "glpat-"+strings.Repeat("Z", 20)+sfx
	p := seedProject(t, testCipher(t), "audit-"+sfx, oldToken)
	path := "/api/projects/" + p.ID
	body := `{"name":"audit-` + sfx + `","repo_url":"` + p.RepoURL + `","provider":"gitlab","description":"deploy with ` + shaped +
		`","token":"` + newToken + `"}`
	if code, _, b := send(t, s.Handler(), ctx, http.MethodPut, path, body, nil); code != http.StatusOK {
		t.Fatalf("PUT: %d %s", code, b)
	}
	rows := auditRows(t, path)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	e := rows[0]
	if jsonField(t, e.Request, "token") != "[redacted]" || !strings.Contains(jsonField(t, e.Request, "description"), "[redacted]") {
		t.Fatalf("request = %s", e.Request)
	}
	if jsonField(t, e.Before, "description") != "" || !strings.Contains(jsonField(t, e.After, "description"), "deploy with [redacted]") {
		t.Fatalf("before = %s, after = %s", e.Before, e.After)
	}
	var whole string
	mustDo(t, pool.QueryRow(ctx, `SELECT row_to_json(a)::text FROM audit_events a WHERE id = $1`, e.ID).Scan(&whole))
	for _, secret := range []string{oldToken, newToken, shaped} {
		if strings.Contains(whole, secret) {
			t.Fatalf("the row carries %q: %s", secret, whole)
		}
	}
}

// DESIGN 5.31 rule 2: the target of each kind of route.
func TestAuditTargets(t *testing.T) {
	s, err := New(Deps{Cfg: testConfig()})
	mustDo(t, err)
	for _, tc := range []struct {
		method, path, route, target string
		collection                  bool
	}{
		{"PUT", "/api/agents/a1", "/api/agents/{id}", "/api/agents/a1", false},
		{"DELETE", "/api/agents/a1", "/api/agents/{id}", "/api/agents/a1", false},
		{"POST", "/api/agents", "/api/agents", "/api/agents", true},
		{"POST", "/api/runs/r1/cancel", "/api/runs/{id}/cancel", "/api/runs/r1", false},
		{"POST", "/api/agents/a1/materialize", "/api/agents/{id}/materialize", "/api/agents/a1", false},
		{"PUT", "/api/agents/a1/files/AGENTS.md", "/api/agents/{id}/files/{name}", "/api/agents/a1/files", true},
		{"DELETE", "/api/projects/p1/facts/f1", "/api/projects/{id}/facts/{factId}", "/api/projects/p1/facts", true},
		{"PUT", "/api/skills/s1/files/scripts/run.sh", "/api/skills/{id}/files/{path...}", "/api/skills/s1/files/scripts/run.sh", false},
		{"POST", "/api/workflows/validate", "/api/workflows/validate", "/api/workflows", true},
		{"POST", "/api/approvals/x1/approve", "/api/approvals/{id}/approve", "", false},
		{"PUT", "/api/channels/telegram/binding", "/api/channels/telegram/binding", "", false},
		{"POST", "/api/projects/p1/waves/pause", "/api/projects/{id}/waves/pause", "/api/projects/p1", false},
		{"POST", "/api/projects/p1/waves/resume", "/api/projects/{id}/waves/resume", "/api/projects/p1", false},
		{"POST", "/api/projects/p1/intake/poll", "/api/projects/{id}/intake/poll", "/api/projects/p1", false},
		{"POST", "/api/workflows/w1/versions/3/restore", "/api/workflows/{id}/versions/{version}/restore", "/api/workflows/w1", false},
		{"POST", "/api/plans/pl1/tickets/T9/changes", "/api/plans/{id}/tickets/{ticket}/changes", "/api/plans/pl1", false},
		{"POST", "/api/nowhere", "", "", false},
	} {
		route, target, collection := s.routeTarget(httptest.NewRequest(tc.method, tc.path, nil))
		if route != tc.route || target != tc.target || collection != tc.collection {
			t.Errorf("%s %s = %q %q %v, want %q %q %v", tc.method, tc.path, route, target, collection, tc.route, tc.target, tc.collection)
		}
	}
}

// DESIGN 5.31 rule 1: the caps, and a binary body kept as its size and digest.
func TestAuditBodies(t *testing.T) {
	big := `{"items":["` + strings.Repeat(`<\"x\">`, 20000) + `"]}`
	for _, max := range []int{auditRequestMax, auditStateMax} {
		got := capJSON([]byte(big), max)
		if len(got) > max || !json.Valid(got) || !strings.Contains(string(got), "[truncated ") {
			t.Fatalf("cap %d: %d bytes, valid %v", max, len(got), json.Valid(got))
		}
	}
	if got := capJSON([]byte(`{"a":1}`), auditRequestMax); string(got) != `{"a":1}` {
		t.Fatalf("small = %s", got)
	}
	if got := auditJSON("application/zip", []byte{0xff, 0x00, 0x01}, auditRequestMax); jsonField(t, got, "sha256") == "" ||
		!bytes.Contains(got, []byte(`"bytes":3`)) {
		t.Fatalf("binary = %s", got)
	}
	if got := auditJSON("multipart/form-data; boundary=x", []byte("--x\r\n"), auditRequestMax); jsonField(t, got, "content_type") == "" {
		t.Fatalf("multipart = %s", got)
	}
	if got := auditJSON("text/plain", []byte("token sk-ant-"+strings.Repeat("a", 20)), auditRequestMax); string(got) != `"token [redacted]"` {
		t.Fatalf("text = %s", got)
	}
	if auditJSON("application/json", nil, auditRequestMax) != nil {
		t.Fatal("an empty body is not null")
	}
}

// DESIGN 5.31 rule 5: newest first, paged by an opaque cursor, filtered; bad parameters
// are refused 422; the response matches the contract.
func TestAuditList(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	s, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	sfx := uniqueSuffix()
	target := "/api/agents/list-" + sfx
	var ids []string
	for i := range 5 {
		e := &store.AuditEvent{ActorKind: "agent", ActorID: "lister-" + sfx, Via: "chief", Method: "PUT", Route: "/api/agents/{id}",
			Path: target, Target: target}
		mustDo(t, store.InsertAuditEvent(ctx, pool, e))
		status, outcome := 200, store.AuditOK
		if i == 3 {
			status, outcome = 412, store.AuditDenied
		}
		e.Status, e.Outcome = &status, outcome
		mustDo(t, store.CompleteAuditEvent(ctx, pool, e))
		ids = append(ids, e.ID)
	}
	h := s.Handler()
	list := func(query string) auditPage {
		t.Helper()
		code, hdr, body := send(t, h, ctx, http.MethodGet, "/api/audit?"+query, "", nil)
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", query, code, body)
		}
		if hdr.Get("ETag") == "" {
			t.Fatalf("%s: no ETag", query)
		}
		var p auditPage
		mustDo(t, json.Unmarshal(body, &p))
		return p
	}
	var seen []string
	cursor := ""
	for range 3 {
		p := list("target=" + target + "&limit=2&cursor=" + cursor)
		for _, e := range p.Events {
			seen = append(seen, e.ID)
		}
		cursor = p.NextCursor
		if cursor == "" {
			break
		}
	}
	if strings.Join(seen, ",") != strings.Join([]string{ids[4], ids[3], ids[2], ids[1], ids[0]}, ",") || cursor != "" {
		t.Fatalf("pages = %v (cursor %q), want %v newest first", seen, cursor, ids)
	}
	if p := list("actor=lister-" + sfx + "&outcome=denied"); len(p.Events) != 1 || p.Events[0].ID != ids[3] {
		t.Fatalf("by actor and outcome = %+v", p)
	}
	if p := list("actor=agent&target=" + target + "&since=2999-01-01T00:00:00Z"); len(p.Events) != 0 {
		t.Fatalf("since the future = %+v", p)
	}
	for _, q := range []string{"target=%00", "actor=%E9"} {
		if p := list(q); len(p.Events) != 0 {
			t.Fatalf("%s = %+v", q, p)
		}
	}
	for _, bad := range []string{"limit=0", "limit=201", "outcome=maybe", "since=yesterday", "cursor=bm90LWEtY3Vyc29y"} {
		code, _, body := send(t, h, ctx, http.MethodGet, "/api/audit?"+bad, "", nil)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d %s", bad, code, body)
		}
	}
	router := contract(t)
	for _, st := range []conformanceStep{
		{http.MethodGet, "/api/audit", "", http.StatusOK},
		{http.MethodGet, "/api/audit?target=" + target + "&limit=2", "", http.StatusOK},
		{http.MethodGet, "/api/audit?outcome=maybe", "", http.StatusUnprocessableEntity},
	} {
		if code, body := checkConformance(t, router, ts.URL, st.method, st.path, st.body, st.want < 300); code != st.want {
			t.Fatalf("%s %s: %d %s", st.method, st.path, code, body)
		}
	}
}
