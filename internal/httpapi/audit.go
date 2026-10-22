package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"yuno/internal/actor"
	"yuno/internal/obs"
	"yuno/internal/store"
)

// Audit caps (DESIGN 5.31 rule 1), in bytes.
const (
	auditRequestMax = 16 << 10
	auditStateMax   = 64 << 10
	// auditResponseMax is how much of a response the audit keeps to redact into after;
	// a longer one is recorded by its size.
	auditResponseMax = 1 << 20
	// requestBufferMax is how much of a request body the audit and idempotency read before
	// the handler. Every handler refuses a longer body (the longest, a skill import, takes
	// 25 MiB), so the rest is passed on unread.
	requestBufferMax = 32 << 20
	// stateReadTimeout bounds the after read, which runs even when the caller has gone.
	stateReadTimeout = 10 * time.Second
)

// Audit read page size (DESIGN 5.31 rule 5).
const (
	defaultAuditLimit = 50
	maxAuditLimit     = 200
)

func isMutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// audited reports whether r gets an audit row (DESIGN 5.31 rule 1): every /api request
// that changes something, and every /api request auth refused.
func audited(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/api/") && (isMutation(r.Method) || authFrom(r.Context()).refused)
}

// audit writes the request's row before the handler runs and completes it after
// (DESIGN 5.31 rules 1 and 2). It refuses the request 503 when the row cannot be written,
// and gives the inner layers an actor.Note whose outcome, error code, approval and evidence
// the row takes over. A server without a database (tests) has no audit.
func (s *Server) audit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.pool == nil || !audited(r) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		ctx, note := actor.WithNote(r.Context())
		r = r.WithContext(ctx)
		a, _ := actor.From(ctx)
		route, target, collection := s.routeTarget(r)
		e := &store.AuditEvent{RequestID: headerText(obs.RequestID(ctx)), ActorKind: string(a.Kind), ActorID: a.ID, ActorName: a.Name,
			Via: a.Via, OnBehalfOf: a.OnBehalfOf, Method: r.Method, Route: route, Path: r.URL.EscapedPath(), Target: target}
		if isMutation(r.Method) {
			e.IdempotencyKey = headerText(truncateBytes(r.Header.Get("Idempotency-Key"), idempotencyKeyMax))
		}
		// A refused request is recorded with what it asked for: its body is not read and
		// nothing is read on its behalf.
		refused := authFrom(ctx).refused
		// Rule 2: a POST to a collection has no before; its after is the response.
		readsState := !refused && target != "" && !(collection && r.Method == http.MethodPost)
		if !refused {
			var body []byte
			body, r = requestBody(r)
			e.Request = auditJSON(r.Header.Get("Content-Type"), body, auditRequestMax)
		}
		if readsState {
			e.Before = s.readState(ctx, target)
		}
		if err := store.InsertAuditEvent(ctx, s.pool, e); err != nil {
			s.log.Error("audit insert failed", "request_id", e.RequestID, "method", r.Method, "path", e.Path, "error", err)
			writeError(w, http.StatusServiceUnavailable, "audit_unavailable", "the audit log is unavailable, so nothing was changed; try again shortly", nil)
			return
		}
		rec := &captureWriter{ResponseWriter: w, limit: auditResponseMax}
		complete := func(status int) {
			e.Status = &status
			e.Outcome = auditOutcome(status, note)
			e.ErrorCode = note.ErrorCode
			if e.ErrorCode == "" && status >= 400 && !rec.overflow {
				e.ErrorCode = errorCodeOf(rec.body.Bytes())
			}
			e.ApprovalID = note.ApprovalID
			if note.Evidence != nil {
				if b, err := json.Marshal(note.Evidence); err == nil {
					e.Evidence = capJSON(obs.RedactJSON(b), auditStateMax)
				}
			}
			bg := context.WithoutCancel(ctx)
			switch {
			case readsState:
				rctx, cancel := context.WithTimeout(bg, stateReadTimeout)
				e.After = s.readState(rctx, target)
				cancel()
			case rec.overflow:
				e.After = summaryJSON(rec.Header().Get("Content-Type"), rec.n, nil)
			default:
				e.After = auditJSON(rec.Header().Get("Content-Type"), rec.body.Bytes(), auditStateMax)
			}
			d := time.Since(start).Milliseconds()
			e.DurationMs = &d
			// A failed completion leaves the row started: an incomplete record is the
			// honest state.
			if err := store.CompleteAuditEvent(bg, s.pool, e); err != nil {
				s.log.Error("audit completion failed", "request_id", e.RequestID, "audit_id", e.ID, "error", err)
			}
		}
		defer func() {
			if p := recover(); p != nil {
				// The recoverer answers 500 internal once the panic reaches it.
				if note.ErrorCode == "" {
					note.ErrorCode = "internal"
				}
				complete(http.StatusInternalServerError)
				panic(p)
			}
		}()
		next.ServeHTTP(rec, r)
		complete(rec.statusCode())
	})
}

// auditOutcome is the row's outcome (DESIGN 5.31 rule 1): the note's when an inner layer
// set a valid one, otherwise from the status.
func auditOutcome(status int, n *actor.Note) string {
	if n != nil && n.Outcome != store.AuditStarted && slices.Contains(store.AuditOutcomes, n.Outcome) {
		return n.Outcome
	}
	switch {
	case status < 400:
		return store.AuditOK
	case status == http.StatusUnauthorized, status == http.StatusForbidden, status == http.StatusPreconditionFailed:
		return store.AuditDenied
	}
	return store.AuditFailed
}

// errorCodeOf reads the code of an Error body, "" when b is not one.
func errorCodeOf(b []byte) string {
	var e errorBody
	if json.Unmarshal(b, &e) != nil {
		return ""
	}
	return e.Error.Code
}

// ---- targets (DESIGN 5.31 rule 2) ------------------------------------------------------

// routeTarget returns r's route (its mux pattern without the method, "" when no route
// matches) and its target: the path when a GET route has the same pattern, otherwise the
// path without its last segment when that has a GET route of the pattern without its last
// segment, otherwise "". collection is true when the target's pattern ends in a literal
// segment (a list such as /api/agents), not a parameter.
func (s *Server) routeTarget(r *http.Request) (route, target string, collection bool) {
	_, pattern := s.mux.Handler(r)
	route = patternPath(pattern)
	if route == "" {
		return "", "", false
	}
	path := r.URL.EscapedPath()
	if up, ok := auditTargetUp[route]; ok {
		return route, trimSegments(path, up), false
	}
	if s.getRoute(path) == route {
		return route, path, isCollection(route)
	}
	i, j := strings.LastIndex(path, "/"), strings.LastIndex(route, "/")
	if i <= 0 || j <= 0 {
		return route, "", false
	}
	if parent, parentRoute := path[:i], route[:j]; s.getRoute(parent) == parentRoute {
		return route, parent, isCollection(parentRoute)
	}
	return route, "", false
}

// auditTargetUp names the action routes whose target is not their parent: the resource the
// action changes sits that many segments up. A restore changes the workflow, not the
// version it reads; waves, intake and ticket changes act on their project or plan, whose
// parent segment has no GET route.
var auditTargetUp = map[string]int{
	"/api/workflows/{id}/versions/{version}/restore": 3,
	"/api/projects/{id}/waves/pause":                 2,
	"/api/projects/{id}/waves/resume":                2,
	"/api/projects/{id}/intake/poll":                 2,
	"/api/plans/{id}/tickets/{ticket}/changes":       3,
}

// trimSegments drops the last n segments of path.
func trimSegments(path string, n int) string {
	for ; n > 0; n-- {
		i := strings.LastIndex(path, "/")
		if i <= 0 {
			return ""
		}
		path = path[:i]
	}
	return path
}

// getRoute is the pattern (without the method) of the GET route that serves path, "" when
// none does.
func (s *Server) getRoute(path string) string {
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return ""
	}
	_, pattern := s.mux.Handler(req)
	if !strings.HasPrefix(pattern, http.MethodGet+" ") {
		return ""
	}
	return patternPath(pattern)
}

// patternPath drops the method from a mux pattern ("PUT /api/agents/{id}").
func patternPath(pattern string) string {
	if _, p, ok := strings.Cut(pattern, " "); ok {
		return p
	}
	return pattern
}

func isCollection(route string) bool {
	last := route[strings.LastIndex(route, "/")+1:]
	return last != "" && !strings.HasPrefix(last, "{")
}

// readTarget reads target in-process, as a GET through the routes with ctx (no middleware:
// it is not audited and needs no bearer). A handler that panics reads as a 500.
func (s *Server) readTarget(ctx context.Context, target string) (status int, contentType string, body []byte) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return http.StatusInternalServerError, "", nil
	}
	mw := &memWriter{header: http.Header{}}
	func() {
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("target read panicked", "request_id", obs.RequestID(ctx), "target", target, "panic", p)
				mw.status, mw.body = http.StatusInternalServerError, bytes.Buffer{}
			}
		}()
		s.mux.ServeHTTP(mw, req)
	}()
	return mw.statusCode(), mw.header.Get("Content-Type"), mw.body.Bytes()
}

// readState is the target's state for before or after (DESIGN 5.31 rule 2): its redacted
// representation, null when it does not exist, {"error": ...} when it could not be read.
func (s *Server) readState(ctx context.Context, target string) json.RawMessage {
	status, ct, body := s.readTarget(ctx, target)
	switch status {
	case http.StatusOK:
		return auditJSON(ct, body, auditStateMax)
	case http.StatusNotFound:
		return nil
	}
	msg := fmt.Sprintf("reading %s answered %d", target, status)
	if code := errorCodeOf(body); code != "" {
		msg += " " + code
	}
	b, _ := json.Marshal(map[string]string{"error": msg})
	return b
}

// memWriter collects a response in memory.
type memWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (m *memWriter) Header() http.Header { return m.header }

func (m *memWriter) WriteHeader(code int) {
	if m.status == 0 {
		m.status = code
	}
}

func (m *memWriter) Write(b []byte) (int, error) {
	if m.status == 0 {
		m.status = http.StatusOK
	}
	return m.body.Write(b)
}

func (m *memWriter) statusCode() int {
	if m.status == 0 {
		return http.StatusOK
	}
	return m.status
}

// captureWriter passes a response through and keeps its status and up to limit bytes of
// its body; overflow is set (and the kept bytes dropped) past the limit. n counts every
// byte written.
type captureWriter struct {
	http.ResponseWriter
	status   int
	limit    int
	body     bytes.Buffer
	overflow bool
	n        int64
}

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 && code >= 200 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	c.n += int64(len(b))
	if !c.overflow {
		if c.body.Len()+len(b) > c.limit {
			c.overflow = true
			c.body = bytes.Buffer{}
		} else {
			c.body.Write(b)
		}
	}
	return c.ResponseWriter.Write(b)
}

func (c *captureWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *captureWriter) statusCode() int {
	if c.status == 0 {
		return http.StatusOK
	}
	return c.status
}

// ---- bodies ---------------------------------------------------------------------------

type bodyKey struct{}

// requestBody reads r's body once (up to requestBufferMax) and returns it with a request
// whose body reads the same bytes again, then whatever was left unread. Later calls on
// that request return the same bytes without reading.
func requestBody(r *http.Request) ([]byte, *http.Request) {
	if b, ok := r.Context().Value(bodyKey{}).([]byte); ok {
		return b, r
	}
	var b []byte
	var rest io.Reader = http.NoBody
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		b, err = io.ReadAll(io.LimitReader(r.Body, requestBufferMax))
		rest = r.Body
		if err != nil {
			rest = errReader{err}
		}
	}
	orig := r.Body
	r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, b))
	if orig != nil {
		r.Body = replayBody{Reader: io.MultiReader(bytes.NewReader(b), rest), Closer: orig}
	}
	return b, r
}

type replayBody struct {
	io.Reader
	io.Closer
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// auditJSON is a body as the audit keeps it: redacted JSON of at most max bytes (a JSON
// document or, for text, a string), or for a binary body its content type, size and
// SHA-256. Nil for an empty body.
func auditJSON(contentType string, b []byte, max int) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	if strings.HasPrefix(strings.ToLower(contentType), "multipart/") || !utf8.Valid(b) {
		sum := sha256.Sum256(b)
		return summaryJSON(contentType, int64(len(b)), sum[:])
	}
	return capJSON(obs.RedactJSON(b), max)
}

// summaryJSON describes a body that is not kept: its content type, size and, when known,
// SHA-256.
func summaryJSON(contentType string, n int64, sum []byte) json.RawMessage {
	m := map[string]any{"content_type": contentType, "bytes": n}
	if sum != nil {
		m["sha256"] = hex.EncodeToString(sum)
	}
	b, _ := json.Marshal(m)
	return b
}

// capJSON keeps a redacted JSON document of at most max bytes as it is, and cuts a longer
// one into a JSON string that fits in max with obs.Truncate's marker.
func capJSON(b []byte, max int) json.RawMessage {
	if len(b) <= max {
		return b
	}
	text := string(b)
	n := max
	for n > 0 {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(obs.Truncate(text, n))
		out := bytes.TrimRight(buf.Bytes(), "\n")
		if len(out) <= max {
			return out
		}
		n -= len(out) - max + 8
	}
	return json.RawMessage(`""`)
}

// truncateBytes cuts s to at most n bytes.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// headerText is a header value as Postgres text takes it: valid UTF-8 without NUL. A
// header may carry any byte above 0x7f, and a refused insert would refuse the request.
func headerText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "")
}

// ---- GET /api/audit (DESIGN 5.31 rule 5) ---------------------------------------------

type auditPage struct {
	Events []store.AuditEvent `json:"events"`
	// NextCursor reads the next (older) page; "" on the last page.
	NextCursor string `json:"next_cursor"`
}

// listAudit lists audit rows newest first, filtered by actor (kind or id), target,
// outcome and since, a page of at most 200 at a time.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var fe fieldErrors
	f := store.AuditFilter{Actor: strings.TrimSpace(q.Get("actor")), Target: strings.TrimSpace(q.Get("target")),
		Outcome: q.Get("outcome"), Limit: defaultAuditLimit}
	if f.Outcome != "" && !slices.Contains(store.AuditOutcomes, f.Outcome) {
		fe.add("outcome", "must be one of %s", strings.Join(store.AuditOutcomes, ", "))
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			fe.add("since", "must be an RFC 3339 time")
		} else {
			f.Since = &t
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxAuditLimit {
			fe.add("limit", "must be an integer from 1 to %d", maxAuditLimit)
		} else {
			f.Limit = n
		}
	}
	if v := q.Get("cursor"); v != "" {
		at, id, ok := decodeAuditCursor(v)
		if !ok {
			fe.add("cursor", "must be a next_cursor from a previous page")
		} else {
			f.AfterAt, f.AfterID = &at, id
		}
	}
	if len(fe) > 0 {
		writeValidation(w, "invalid query", fe)
		return
	}
	// A control character or invalid UTF-8 names no actor or target; Postgres would refuse
	// either with a 500.
	if hasControl(f.Actor) || hasControl(f.Target) || !utf8.ValidString(f.Actor) || !utf8.ValidString(f.Target) {
		writeJSON(w, http.StatusOK, auditPage{Events: []store.AuditEvent{}})
		return
	}
	limit := f.Limit
	f.Limit++
	events, err := store.ListAuditEvents(r.Context(), s.pool, f)
	if err != nil {
		s.internalError(w, r, "list audit events", err)
		return
	}
	page := auditPage{Events: events}
	if len(events) > limit {
		page.Events = events[:limit]
		last := page.Events[limit-1]
		page.NextCursor = encodeAuditCursor(last.At, last.ID)
	}
	writeJSON(w, http.StatusOK, page)
}

// The cursor is the position (at, id) of the last row of a page, opaque to the caller.
func encodeAuditCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeAuditCursor(c string) (time.Time, string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", false
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" || hasControl(id) {
		return time.Time{}, "", false
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, "", false
	}
	return at, id, true
}
