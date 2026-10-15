package tools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yuno/internal/deploytarget"
)

// countingTarget is the real deploy target behind a counter and a failure switch.
type countingTarget struct {
	srv     *httptest.Server
	posts   atomic.Int32
	mu      sync.Mutex
	fail    []int         // statuses to answer before passing requests through, in order
	body    string        // body for the injected failures
	delay   time.Duration // before each POST is handled
	keys    []string
	created atomic.Int32
}

func newCountingTarget(t *testing.T) *countingTarget {
	t.Helper()
	st, err := deploytarget.Open(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	h := deploytarget.Handler(st, slog.New(slog.DiscardHandler))
	ct := &countingTarget{}
	ct.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			ct.posts.Add(1)
			ct.mu.Lock()
			ct.keys = append(ct.keys, r.Header.Get("Idempotency-Key"))
			var status int
			if len(ct.fail) > 0 {
				status, ct.fail = ct.fail[0], ct.fail[1:]
			}
			body, delay := ct.body, ct.delay
			ct.mu.Unlock()
			time.Sleep(delay)
			if status != 0 {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
				return
			}
			rec := &statusCapture{ResponseWriter: w}
			h.ServeHTTP(rec, r)
			if rec.status == http.StatusCreated {
				ct.created.Add(1)
			}
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ct.srv.Close)
	return ct
}

func (ct *countingTarget) failNext(body string, statuses ...int) {
	ct.mu.Lock()
	ct.fail, ct.body = statuses, body
	ct.mu.Unlock()
}

type statusCapture struct {
	http.ResponseWriter
	status int
}

func (s *statusCapture) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func deployTool(ct *countingTarget) *Tool {
	r := NewRegistry()
	r.Register(NewDeployRelease(DeployConfig{TargetURL: ct.srv.URL, MaxBytes: 1 << 20, PublicBaseURL: "http://yuno.test"}))
	t, _ := r.Get(DeployReleaseName)
	return t
}

// T5.7.5 (deploy_release): the happy path against the real deploy target.
func TestDeployReleaseHappyPath(t *testing.T) {
	ct := newCountingTarget(t)
	tool := deployTool(ct)
	ws := t.TempDir()
	runDir := filepath.Join(ws, "runs", "run-1")
	must(t, os.MkdirAll(filepath.Join(runDir, "site"), 0o755))
	must(t, os.WriteFile(filepath.Join(runDir, "site", "index.html"), []byte("<h1>hi</h1>"), 0o644))
	must(t, os.WriteFile(filepath.Join(runDir, "app.py"), []byte("print(1)"), 0o644))

	call := Call{RunID: "run-1", Workspace: ws, IdempotencyKey: strings.Repeat("ab", 32),
		Input: json.RawMessage(`{"run_token":"r_x","version":"1.2.0","notes":"first","paths":["site","runs/run-1/app.py"]}`)}
	res, err := tool.Invoke(context.Background(), call)
	if err != nil || res.IsError {
		t.Fatalf("deploy: %+v %v", res, err)
	}
	var out deployOutput
	must(t, json.Unmarshal(res.Output, &out))
	if out.ReleaseID == "" || out.Version != "1.2.0" || out.Replayed ||
		out.URL != "http://yuno.test/deploy/releases/"+out.ReleaseID+"/" {
		t.Fatalf("output = %s", res.Output)
	}
	if ct.keys[0] != call.IdempotencyKey {
		t.Fatalf("Idempotency-Key header = %q", ct.keys[0])
	}
	resp, err := http.Get(ct.srv.URL + "/releases/" + out.ReleaseID + "/site/index.html")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "<h1>hi</h1>" {
		t.Fatalf("published file = %q", b)
	}

	// The target answers a known key with 200 and the original release.
	again, _ := tool.Invoke(context.Background(), call)
	var out2 deployOutput
	must(t, json.Unmarshal(again.Output, &out2))
	if out2.ReleaseID != out.ReleaseID || !out2.Replayed || ct.created.Load() != 1 {
		t.Fatalf("second post: %s (created %d)", again.Output, ct.created.Load())
	}
}

func TestDeployReleaseRefusals(t *testing.T) {
	ct := newCountingTarget(t)
	tool := deployTool(ct)
	ws := t.TempDir()
	runDir := filepath.Join(ws, "runs", "run-1")
	must(t, os.MkdirAll(runDir, 0o755))
	must(t, os.WriteFile(filepath.Join(runDir, "ok.txt"), []byte("ok"), 0o644))
	must(t, os.WriteFile(filepath.Join(ws, "secret.txt"), []byte("outside"), 0o644))
	must(t, os.Symlink(filepath.Join(ws, "secret.txt"), filepath.Join(runDir, "escape.txt")))
	must(t, os.WriteFile(filepath.Join(runDir, "big.bin"), make([]byte, 2<<20), 0o644))

	tests := []struct {
		name, paths, want string
	}{
		{"parent traversal", `["../secret.txt"]`, "parent traversal"},
		{"absolute path", `["/etc/passwd"]`, "must be relative"},
		{"missing file", `["nope.txt"]`, "not found"},
		{"symlink out of the run directory", `["escape.txt"]`, "links are not packaged"},
		{"over the size limit", `["big.bin"]`, "exceed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tool.Invoke(context.Background(), Call{RunID: "run-1", Workspace: ws, IdempotencyKey: "k",
				Input: json.RawMessage(`{"run_token":"r","version":"1","notes":"","paths":` + tc.paths + `}`)})
			if err != nil || !res.IsError || res.Transient || !strings.Contains(res.Text, tc.want) {
				t.Fatalf("result = %+v, %v; want a permanent error containing %q", res, err, tc.want)
			}
		})
	}
	if ct.posts.Load() != 0 {
		t.Fatalf("refused calls reached the target %d times", ct.posts.Load())
	}
	res, _ := tool.Invoke(context.Background(), Call{RunID: "no-such-run", Workspace: ws, IdempotencyKey: "k",
		Input: json.RawMessage(`{"run_token":"r","version":"1","notes":"","paths":["ok.txt"]}`)})
	if !res.IsError || !strings.Contains(res.Text, "does not exist") {
		t.Fatalf("missing run directory: %+v", res)
	}
}

func TestDeployReleaseClassifiesFailures(t *testing.T) {
	ct := newCountingTarget(t)
	tool := deployTool(ct)
	ws := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(ws, "runs", "r"), 0o755))
	must(t, os.WriteFile(filepath.Join(ws, "runs", "r", "a.txt"), []byte("a"), 0o644))
	call := Call{RunID: "r", Workspace: ws, IdempotencyKey: "k1", Input: json.RawMessage(`{"run_token":"r","version":"1","notes":"","paths":["a.txt"]}`)}
	tests := []struct {
		name          string
		status        int
		body          string
		wantTransient bool
		want          string
	}{
		{"5xx is transient", http.StatusBadGateway, "upstream down", true, "HTTP 502"},
		{"4xx is permanent", http.StatusBadRequest, "version is required", false, "version is required"},
		{"non-JSON body is permanent", http.StatusCreated, "<html>oops</html>", false, "not JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ct.failNext(tc.body, tc.status)
			res, err := tool.Invoke(context.Background(), call)
			if err != nil || !res.IsError || res.Transient != tc.wantTransient || !strings.Contains(res.Text, tc.want) {
				t.Fatalf("result = %+v, %v", res, err)
			}
		})
	}
	t.Run("connection refused is transient", func(t *testing.T) {
		r := NewRegistry()
		r.Register(NewDeployRelease(DeployConfig{TargetURL: "http://127.0.0.1:1", MaxBytes: 1 << 20}))
		down, _ := r.Get(DeployReleaseName)
		res, err := down.Invoke(context.Background(), call)
		if err != nil || !res.IsError || !res.Transient {
			t.Fatalf("result = %+v, %v", res, err)
		}
	})
}
