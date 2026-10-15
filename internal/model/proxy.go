package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Anthropic proxy defaults (DESIGN 5.14 rule 8).
const (
	DefaultAnthropicBaseURL = "https://api.anthropic.com"
	DefaultAnthropicVersion = "2023-06-01"
	// MaxProxyBodyBytes caps a request body the proxy parses. A Messages request carries
	// the whole transcript; OpenClaw's system prompt alone is about 47 KB.
	MaxProxyBodyBytes = 20 << 20
)

// AnthropicProxyConfig configures NewAnthropicProxy.
type AnthropicProxyConfig struct {
	// Prefix is the mount path removed before forwarding, e.g. /model-proxy/anthropic.
	Prefix string
	// BaseURL is the upstream API; empty means DefaultAnthropicBaseURL.
	BaseURL string
	// APIKey is sent upstream as x-api-key, or as a bearer token when AuthScheme is
	// "bearer". When empty every request is answered 401.
	APIKey string
	// AuthScheme is "x-api-key" (default, the Anthropic API) or "bearer" (OpenRouter).
	AuthScheme string
	// ModelMap rewrites the model id of Messages requests before forwarding, e.g.
	// claude-opus-5-5 to anthropic/claude-opus-5.5 for OpenRouter. Unmapped ids pass
	// through unchanged. Nil means no rewriting.
	ModelMap func(id string) string
	// RatePerMinute bounds upstream requests with a token bucket of that size, refilled
	// evenly over a minute. Zero or negative disables the limit.
	RatePerMinute int
	Log           *slog.Logger
	// Transport is the upstream round tripper; nil means http.DefaultTransport.
	Transport http.RoundTripper
	// Now is the rate limiter's clock; nil means time.Now.
	Now func() time.Time
}

// NewAnthropicProxy returns the reverse proxy OpenClaw's Anthropic provider calls
// (DESIGN 5.14 rule 8). It replaces any incoming credentials with the api's key, adds
// anthropic-version when missing, forces adaptive thinking without display on
// Messages requests that ask for thinking, streams responses through unbuffered, logs
// one line per upstream response and enforces the per-minute rate limit.
//
// Trust: the proxy authenticates nobody. The gateway shares the api's network
// namespace and calls it over loopback with a placeholder key that is replaced here,
// so the gateway never holds the real key. The HTTP layer mounts the proxy for
// loopback callers only.
func NewAnthropicProxy(cfg AnthropicProxyConfig) (http.Handler, error) {
	base := cfg.BaseURL
	if base == "" {
		base = DefaultAnthropicBaseURL
	}
	upstream, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || upstream.Scheme == "" || upstream.Host == "" {
		return nil, fmt.Errorf("model proxy: invalid upstream URL %q", base)
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	p := &anthropicProxy{cfg: cfg, bucket: newTokenBucket(cfg.RatePerMinute, cfg.Now)}
	key := cfg.APIKey
	bearer := strings.EqualFold(cfg.AuthScheme, "bearer")
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			h := pr.Out.Header
			h.Del("Authorization")
			h.Del("X-Api-Key")
			if bearer {
				h.Set("Authorization", "Bearer "+key)
			} else {
				h.Set("X-Api-Key", key)
			}
			if h.Get("Anthropic-Version") == "" {
				h.Set("Anthropic-Version", DefaultAnthropicVersion)
			}
		},
		FlushInterval:  -1,
		Transport:      cfg.Transport,
		ModifyResponse: p.onResponse,
		ErrorHandler:   p.onError,
	}
	return p, nil
}

type anthropicProxy struct {
	cfg    AnthropicProxyConfig
	bucket *tokenBucket
	rp     *httputil.ReverseProxy
}

type proxyCallKey struct{}

// proxyCall is what the response log line needs from the request.
type proxyCall struct {
	model string
	start time.Time
}

func (p *anthropicProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.cfg.APIKey == "" {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error",
			"the model proxy has no upstream API key; set ANTHROPIC_API_KEY or OPENROUTER_API_KEY for the api service")
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, p.cfg.Prefix)
	if !ok {
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", "unknown model proxy path")
		return
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if ok, wait := p.bucket.take(); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(int(math.Ceil(wait.Seconds()))))
		writeAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error",
			fmt.Sprintf("model proxy rate limit of %d requests per minute exceeded", p.cfg.RatePerMinute))
		return
	}

	call := &proxyCall{start: p.cfg.Now()}
	out := r.WithContext(context.WithValue(r.Context(), proxyCallKey{}, call))
	u := *r.URL
	u.Path, u.RawPath = path, ""
	out.URL = &u

	if r.Method == http.MethodPost && strings.HasSuffix(path, "/v1/messages") {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxProxyBodyBytes))
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeAnthropicError(w, http.StatusRequestEntityTooLarge, "request_too_large",
					fmt.Sprintf("request body exceeds %d bytes", MaxProxyBodyBytes))
				return
			}
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "could not read request body")
			return
		}
		body, model, err := ShapeMessagesBodyFor(raw, p.cfg.ModelMap)
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		call.model = model
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.ContentLength = int64(len(body))
		out.TransferEncoding = nil
		out.GetBody = nil
	}
	p.rp.ServeHTTP(w, out)
}

// ShapeMessagesBody applies the proxy's request rules to a Messages API body: a
// present thinking parameter becomes {"type":"adaptive"} (dropping display, so no
// reasoning text is returned, and budget_tokens, which current models reject); an
// absent one is never added. It also returns the requested model for logging. A body
// without thinking is returned byte for byte.
func ShapeMessagesBody(raw []byte) (body []byte, model string, err error) {
	return ShapeMessagesBodyFor(raw, nil)
}

// ShapeMessagesBodyFor is ShapeMessagesBody with an optional model rewrite (the
// upstream's id replaces the requested one and is the model returned for logging).
func ShapeMessagesBodyFor(raw []byte, modelMap func(string) string) (body []byte, model string, err error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, "", errors.New("request body must be a JSON object")
	}
	if m, ok := fields["model"]; ok {
		_ = json.Unmarshal(m, &model)
	}
	changed := false
	if modelMap != nil && model != "" {
		if mapped := modelMap(model); mapped != model {
			mapped_, _ := json.Marshal(mapped)
			fields["model"] = mapped_
			model = mapped
			changed = true
		}
	}
	th, ok := fields["thinking"]
	if ok && string(bytes.TrimSpace(th)) != "null" {
		fields["thinking"] = json.RawMessage(`{"type":"adaptive"}`)
		changed = true
	}
	if !changed {
		return raw, model, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fields); err != nil {
		return nil, "", fmt.Errorf("re-encode request body: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), model, nil
}

// onResponse arranges the log line for when the body has been relayed, so the
// duration covers the whole stream.
func (p *anthropicProxy) onResponse(resp *http.Response) error {
	call, _ := resp.Request.Context().Value(proxyCallKey{}).(*proxyCall)
	if call == nil {
		call = &proxyCall{start: p.cfg.Now()}
	}
	status, requestID := resp.StatusCode, resp.Header.Get("Request-Id")
	resp.Body = &loggedBody{ReadCloser: resp.Body, done: func() {
		p.cfg.Log.Info("model proxy",
			"status", status,
			"request_id", requestID,
			"model", call.model,
			"duration_ms", p.cfg.Now().Sub(call.start).Milliseconds(),
		)
	}}
	return nil
}

func (p *anthropicProxy) onError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		p.cfg.Log.Debug("model proxy: caller went away", "error", err)
		return
	}
	model := ""
	if call, _ := r.Context().Value(proxyCallKey{}).(*proxyCall); call != nil {
		model = call.model
	}
	p.cfg.Log.Warn("model proxy upstream failed", "model", model, "error", err)
	writeAnthropicError(w, http.StatusBadGateway, "api_error", "the model proxy could not reach the upstream model API")
}

// OpenRouter serves the Anthropic messages format at <base>/v1/messages with bearer
// auth and ids of the form anthropic/claude-opus-5.5 [probed 2026-10-15].
const DefaultOpenRouterBaseURL = "https://openrouter.ai/api"

var trailingMinor = regexp.MustCompile(`^(.*\d)-(\d+)$`)

// OpenRouterModelID maps an Anthropic model id to OpenRouter's: the trailing minor
// version separator becomes a dot and the vendor prefix is added (claude-opus-5-5 to
// anthropic/claude-opus-5.5, claude-opus-5 to anthropic/claude-opus-5). Ids that already
// carry a vendor prefix are returned unchanged.
func OpenRouterModelID(id string) string {
	if strings.Contains(id, "/") {
		return id
	}
	return "anthropic/" + trailingMinor.ReplaceAllString(id, "$1.$2")
}

// loggedBody runs done once when the proxied body is closed.
type loggedBody struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (b *loggedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.done)
	return err
}

// writeAnthropicError answers in the Anthropic API error shape so OpenClaw's provider
// classifies the failure like an upstream one.
func writeAnthropicError(w http.ResponseWriter, status int, typ, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": typ, "message": message},
	})
}

// tokenBucket is a mutex-guarded token bucket; a nil bucket admits everything.
type tokenBucket struct {
	mu       sync.Mutex
	capacity float64
	tokens   float64
	perSec   float64
	last     time.Time
	now      func() time.Time
}

func newTokenBucket(perMinute int, now func() time.Time) *tokenBucket {
	if perMinute <= 0 {
		return nil
	}
	c := float64(perMinute)
	return &tokenBucket{capacity: c, tokens: c, perSec: c / 60, last: now(), now: now}
}

// take consumes a token, or reports how long until one is available.
func (b *tokenBucket) take() (bool, time.Duration) {
	if b == nil {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(b.capacity, b.tokens+elapsed*b.perSec)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / b.perSec * float64(time.Second))
}
