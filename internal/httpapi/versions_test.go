package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"testing"
	"time"

	"yuno/internal/store"
)

var etagShape = regexp.MustCompile(`^"[0-9a-f]{16}"$`)

// getETag reads path and returns its ETag, checking it is the version of the body.
func getETag(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	code, hdr, body := send(t, h, context.Background(), http.MethodGet, path, "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, code, body)
	}
	tag := hdr.Get("ETag")
	if !etagShape.MatchString(tag) || tag != etagOf(body) {
		t.Fatalf("GET %s: ETag %q for %s", path, tag, body)
	}
	return tag
}

// currentETag reads the current_etag of a 412.
func currentETag(t *testing.T, body []byte) string {
	t.Helper()
	e := decodeError(t, body)
	if e.Error.Code != "version_conflict" {
		t.Fatalf("not a version conflict: %s", body)
	}
	var d struct {
		CurrentETag string `json:"current_etag"`
	}
	mustDo(t, json.Unmarshal(e.Error.Details, &d))
	return d.CurrentETag
}

// DESIGN 5.31 rule 4: a stale If-Match is refused 412 with the current ETag and changes
// nothing; the audit records it denied.
func TestETagIfMatch412(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	s, _ := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	h := s.Handler()
	sfx := uniqueSuffix()
	agentID := testAgent(t, "etag-"+sfx)
	path := "/api/agents/" + agentID
	tag := getETag(t, h, path)

	code, _, body := send(t, h, ctx, http.MethodPut, path, `{"name":"etag-stale-`+sfx+`","model":"claude-opus-5-5"}`,
		map[string]string{"If-Match": `"0000000000000000"`})
	if code != http.StatusPreconditionFailed || currentETag(t, body) != tag {
		t.Fatalf("stale PUT: %d %s", code, body)
	}
	if a, err := store.GetAgent(ctx, pool, agentID); err != nil || a.Name != "etag-"+sfx {
		t.Fatalf("agent after a refused PUT = %+v, %v", a, err)
	}
	rows := auditRows(t, path)
	if len(rows) != 1 || rows[0].Outcome != store.AuditDenied || rows[0].ErrorCode != "version_conflict" {
		t.Fatalf("audit = %+v", rows)
	}
	// A DELETE of something that no longer exists has no current version.
	code, _, body = send(t, h, ctx, http.MethodDelete, "/api/agents/gone-"+sfx, "", map[string]string{"If-Match": tag})
	if code != http.StatusPreconditionFailed || currentETag(t, body) != "" {
		t.Fatalf("DELETE of a missing agent: %d %s", code, body)
	}
}

// DESIGN 5.31 rule 4: every GET 200 JSON response has an ETag; a matching If-Match lets
// the change run, and the change makes a new version; nothing changes without If-Match.
func TestETagIfMatchOK(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	s, _ := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	h := s.Handler()
	sfx := uniqueSuffix()
	agentID := testAgent(t, "etag-ok-"+sfx)
	path := "/api/agents/" + agentID
	tag := getETag(t, h, path)
	getETag(t, h, "/api/agents")

	put := func(name, ifMatch string) (int, []byte) {
		hdr := map[string]string{}
		if ifMatch != "" {
			hdr["If-Match"] = ifMatch
		}
		code, _, body := send(t, h, ctx, http.MethodPut, path, `{"name":"`+name+`","model":"claude-opus-5-5"}`, hdr)
		return code, body
	}
	if code, body := put("etag-ok-2-"+sfx, tag); code != http.StatusOK {
		t.Fatalf("PUT with the current ETag: %d %s", code, body)
	}
	next := getETag(t, h, path)
	if next == tag {
		t.Fatal("the version did not change")
	}
	if code, body := put("etag-ok-3-"+sfx, tag); code != http.StatusPreconditionFailed || currentETag(t, body) != next {
		t.Fatalf("PUT with the old ETag: %d %s", code, body)
	}
	// Unquoted, weak, listed and "*" forms match too.
	for i, form := range []func(string) string{
		func(tag string) string { return tag[1 : len(tag)-1] },
		func(tag string) string { return "W/" + tag },
		func(tag string) string { return `"0000000000000000", ` + tag },
	} {
		if code, body := put(fmt.Sprintf("etag-ok-form-%d-%s", i, sfx), form(next)); code != http.StatusOK {
			t.Fatalf("If-Match %s: %d %s", form(next), code, body)
		}
		next = getETag(t, h, path)
	}
	if code, body := put("etag-ok-star-"+sfx, "*"); code != http.StatusOK {
		t.Fatalf("If-Match *: %d %s", code, body)
	}
	// Without If-Match nothing is compared (the UI today).
	if code, body := put("etag-ok-plain-"+sfx, ""); code != http.StatusOK {
		t.Fatalf("PUT without If-Match: %d %s", code, body)
	}
	// Errors, documents and streams carry no ETag.
	for _, p := range []string{"/api/agents/no-such-agent-" + sfx, "/api/openapi.yaml"} {
		if _, hdr, _ := send(t, h, ctx, http.MethodGet, p, "", nil); hdr.Get("ETag") != "" {
			t.Fatalf("GET %s has an ETag", p)
		}
	}
	if code, body := put("etag-ok-final-"+sfx, next); code != http.StatusPreconditionFailed {
		t.Fatalf("PUT with a version two changes old: %d %s", code, body)
	}
}

// DESIGN 5.31 rule 4 holds back only JSON: an event stream still reaches the client while
// it is open, without an ETag.
func TestEventStreamNotHeldBack(t *testing.T) {
	_, ts := newTestServer(t, nil, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	mustDo(t, err)
	resp, err := ts.Client().Do(req)
	mustDo(t, err)
	defer resp.Body.Close()
	buf := make([]byte, len(": connected"))
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != ": connected" {
		t.Fatalf("stream start = %q, %v", buf, err)
	}
	if resp.Header.Get("ETag") != "" || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream headers = %v", resp.Header)
	}
}

// DESIGN 5.31 rule 4: the per-target lock holds a second change of the same target until
// the first is done, and leaves other targets free.
func TestVersionLockSerializes(t *testing.T) {
	var l targetLocks
	unlock := l.lock("/api/agents/a1")
	other := l.lock("/api/agents/a2")
	got := make(chan struct{})
	released := make(chan struct{})
	go func() {
		u := l.lock("/api/agents/a1")
		close(got)
		u()
		close(released)
	}()
	select {
	case <-got:
		t.Fatal("the second holder got the lock while the first held it")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the second holder never got the lock")
	}
	other()
	<-released
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) != 0 {
		t.Fatalf("locks left = %v", l.m)
	}
}
