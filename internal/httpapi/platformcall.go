package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"net/http"

	"yuno/internal/actor"
	"yuno/internal/tools"
)

// platformResponseMax is how much of a response body platform_call keeps; its size and
// SHA-256 cover the whole body (DESIGN 5.32 rule 4).
const platformResponseMax = 1 << 20

// inProcessAddr is the remote address of a platform_call request: not a loopback one, so
// the model proxy refuses it like any caller from outside.
const inProcessAddr = "in-process:0"

// platformDispatcher is platform_call's way into the api (DESIGN 5.32 rule 4): the
// request goes through Handler(), every middleware included, with the server's own bearer
// and the agent actor in its context, which no network request can set.
type platformDispatcher struct {
	s *Server
	// handler replaces Handler() when set: a chain with other layers in the slots DESIGN 5.31 rule 6
	// names.
	handler http.Handler
}

func (d platformDispatcher) Dispatch(ctx context.Context, req tools.PlatformRequest) (*tools.PlatformResponse, error) {
	var body io.Reader = http.NoBody
	if req.Body != nil {
		body = bytes.NewReader(req.Body)
	}
	r, err := http.NewRequestWithContext(actor.With(ctx, req.Actor), req.Method, req.Target, body)
	if err != nil {
		return nil, &tools.ModelError{Message: "invalid request: " + err.Error()}
	}
	r.RemoteAddr, r.RequestURI = inProcessAddr, req.Target
	if req.Body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if tok := d.s.cfg.APIToken; tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	if req.IdempotencyKey != "" {
		r.Header.Set(tools.HeaderIdempotencyKey, req.IdempotencyKey)
	}
	if req.IfMatch != "" {
		r.Header.Set(tools.HeaderIfMatch, req.IfMatch)
	}
	h := d.handler
	if h == nil {
		h = d.s.Handler()
	}
	w := newResponseBuffer(platformResponseMax)
	h.ServeHTTP(w, r)
	return w.response(), nil
}

// responseBuffer is the http.ResponseWriter of an in-process request: the status, the
// headers, the first max bytes of the body, and the size and hash of all of it.
type responseBuffer struct {
	header http.Header
	status int
	body   bytes.Buffer
	max    int
	size   int64
	sum    hash.Hash
}

func newResponseBuffer(max int) *responseBuffer {
	return &responseBuffer{header: http.Header{}, max: max, sum: sha256.New()}
}

func (b *responseBuffer) Header() http.Header { return b.header }

func (b *responseBuffer) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

func (b *responseBuffer) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	b.size += int64(len(p))
	b.sum.Write(p)
	if room := b.max - b.body.Len(); room > 0 {
		b.body.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// Flush lets a handler that flushes run; nothing streams to anyone.
func (b *responseBuffer) Flush() {}

func (b *responseBuffer) response() *tools.PlatformResponse {
	status := b.status
	if status == 0 {
		status = http.StatusOK
	}
	return &tools.PlatformResponse{Status: status, Header: b.header, Body: b.body.Bytes(), Size: b.size,
		SHA256: hex.EncodeToString(b.sum.Sum(nil))}
}
