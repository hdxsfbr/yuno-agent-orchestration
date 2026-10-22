package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"net/http"
	"strings"
	"sync"
)

// etagOf is the version of a JSON body (DESIGN 5.31 rule 4): the hex of the first 8 bytes
// of its SHA-256, quoted.
func etagOf(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

func isJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "application/json"
}

// versions gives every /api GET 200 JSON response an ETag, and checks a PUT, PATCH or
// DELETE that sends If-Match against its target's current ETag (DESIGN 5.31 rule 4).
// Without If-Match a change runs as before.
func (s *Server) versions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			ew := &etagWriter{ResponseWriter: w}
			next.ServeHTTP(ew, r)
			ew.finish()
		case http.MethodPut, http.MethodPatch, http.MethodDelete:
			if r.Header.Get("If-Match") == "" {
				next.ServeHTTP(w, r)
				return
			}
			s.ifMatch(next, w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// ifMatch takes the target's lock, reads it and compares its ETag with If-Match: on a
// mismatch it answers 412 version_conflict with the current ETag ("" when the target does
// not exist), on a match the handler runs while the lock is held. A request with no target
// (DESIGN 5.31 rule 2) has no version to compare and runs as without If-Match.
func (s *Server) ifMatch(next http.Handler, w http.ResponseWriter, r *http.Request) {
	_, target, _ := s.routeTarget(r)
	if target == "" {
		next.ServeHTTP(w, r)
		return
	}
	unlock := s.versionLocks.lock(target)
	defer unlock()
	current := ""
	if status, ct, body := s.readTarget(r.Context(), target); status == http.StatusOK && isJSON(ct) {
		current = etagOf(body)
	}
	if !etagMatches(r.Header.Get("If-Match"), current) {
		writeError(w, http.StatusPreconditionFailed, "version_conflict",
			"it changed since you read it; read it again and repeat the change with its current ETag",
			map[string]string{"current_etag": current})
		return
	}
	next.ServeHTTP(w, r)
}

// etagMatches reports whether an If-Match list names current: "*" matches any current
// version, and quotes and a weak prefix are ignored.
func etagMatches(ifMatch, current string) bool {
	if current == "" {
		return false
	}
	for _, tag := range strings.Split(ifMatch, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" || normalizeETag(tag) == normalizeETag(current) {
			return true
		}
	}
	return false
}

func normalizeETag(tag string) string {
	return strings.Trim(strings.TrimPrefix(tag, "W/"), `"`)
}

// etagWriter holds back a 200 JSON response to put its ETag in front; any other response
// (an event stream, a file, an error) passes straight through.
type etagWriter struct {
	http.ResponseWriter
	status    int
	buffering bool
	buf       bytes.Buffer
}

func (e *etagWriter) WriteHeader(code int) {
	if e.status != 0 {
		if !e.buffering {
			e.ResponseWriter.WriteHeader(code)
		}
		return
	}
	if code < 200 {
		e.ResponseWriter.WriteHeader(code)
		return
	}
	e.status = code
	if code == http.StatusOK && isJSON(e.Header().Get("Content-Type")) {
		e.buffering = true
		return
	}
	e.ResponseWriter.WriteHeader(code)
}

func (e *etagWriter) Write(b []byte) (int, error) {
	if e.status == 0 {
		e.WriteHeader(http.StatusOK)
	}
	if e.buffering {
		return e.buf.Write(b)
	}
	return e.ResponseWriter.Write(b)
}

func (e *etagWriter) Flush() {
	if e.status == 0 {
		e.WriteHeader(http.StatusOK)
	}
	if e.buffering {
		return
	}
	if f, ok := e.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (e *etagWriter) Unwrap() http.ResponseWriter { return e.ResponseWriter }

// finish sends a held-back response with its ETag.
func (e *etagWriter) finish() {
	if !e.buffering {
		return
	}
	e.Header().Set("ETag", etagOf(e.buf.Bytes()))
	e.ResponseWriter.WriteHeader(http.StatusOK)
	_, _ = e.ResponseWriter.Write(e.buf.Bytes())
}

// targetLocks are the per-target locks of If-Match (DESIGN 5.31 rule 4), in this process.
// A target's entry lives while someone holds or waits for it.
type targetLocks struct {
	mu sync.Mutex
	m  map[string]*targetLock
}

type targetLock struct {
	sync.Mutex
	refs int
}

// lock takes the lock of key and returns its unlock.
func (l *targetLocks) lock(key string) func() {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]*targetLock{}
	}
	t := l.m[key]
	if t == nil {
		t = &targetLock{}
		l.m[key] = t
	}
	t.refs++
	l.mu.Unlock()
	t.Lock()
	return func() {
		t.Unlock()
		l.mu.Lock()
		if t.refs--; t.refs == 0 {
			delete(l.m, key)
		}
		l.mu.Unlock()
	}
}
