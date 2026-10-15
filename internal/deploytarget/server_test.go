package deploytarget

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	store, err := Open(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(Handler(store, slog.New(slog.NewTextHandler(io.Discard, nil))))
}

func post(t *testing.T, srv *httptest.Server, key, body string) (int, Release) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/releases", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rel Release
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &rel)
	return resp.StatusCode, rel
}

// T5.4.4: two POSTs with the same key produce one release.
func TestIdempotentCreate(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	body := `{"version":"1.0.0","notes":"n","files":[{"path":"index.html","content_b64":"` + base64.StdEncoding.EncodeToString([]byte("<h1>hi</h1>")) + `"}]}`

	s1, r1 := post(t, srv, "k1", body)
	s2, r2 := post(t, srv, "k1", body)
	if s1 != 201 || s2 != 200 || r1.ID == "" || r1.ID != r2.ID {
		t.Fatalf("got %d/%s and %d/%s", s1, r1.ID, s2, r2.ID)
	}
	s3, r3 := post(t, srv, "k2", body)
	if s3 != 201 || r3.ID == r1.ID {
		t.Fatalf("different key should create: %d %s", s3, r3.ID)
	}

	resp, _ := http.Get(srv.URL + "/live/index.html")
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "<h1>hi</h1>" {
		t.Fatalf("live: %d %q", resp.StatusCode, b)
	}
	resp, _ = http.Get(srv.URL + "/releases/" + r1.ID + "/index.html")
	if resp.StatusCode != 200 {
		t.Fatalf("release file: %d", resp.StatusCode)
	}
	resp, _ = http.Get(srv.URL + "/releases/" + r1.ID + "/meta.json")
	if resp.StatusCode != 404 {
		t.Fatalf("meta.json must be hidden: %d", resp.StatusCode)
	}
}

func TestCreateValidation(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	if s, _ := post(t, srv, "", `{"version":"1","files":[]}`); s != 400 {
		t.Errorf("missing key: %d", s)
	}
	if s, _ := post(t, srv, "k", `{"version":"","files":[{"path":"a","content_b64":"YQ=="}]}`); s != 400 {
		t.Errorf("missing version: %d", s)
	}
	if s, _ := post(t, srv, "k", `{"version":"1","files":[{"path":"../a","content_b64":"YQ=="}]}`); s != 400 {
		t.Errorf("traversal: %d", s)
	}
	if s, _ := post(t, srv, "k", `{"version":"1","files":[{"path":"a","content_b64":"!!"}]}`); s != 400 {
		t.Errorf("bad base64: %d", s)
	}
}
