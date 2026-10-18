package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/config"
	"yuno/internal/store"
)

// artifactStoreDir is the artifact directory the conformance server of
// TestOpenAPIConformance is configured with (testConfig sets ArtifactDir to it), so the
// steps of artifactConformanceSteps can put files where the download handler looks.
func artifactStoreDir() string { return filepath.Join(os.TempDir(), "yuno-httpapi-artifacts") }

// artifactServer is a server whose artifact directory is dir ("" for a disabled store).
func artifactServer(t *testing.T, pool *pgxpool.Pool, mutate func(*config.Config)) (string, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	cfg := testConfig()
	cfg.ArtifactDir = dir
	if mutate != nil {
		mutate(&cfg)
	}
	_, ts := newTestServerDeps(t, Deps{Cfg: cfg, Pool: pool}, &fakeEngine{pool: pool})
	return dir, ts
}

// putArtifact stores a file the way the collector does: the content under
// <dir>/<run id>/<artifact id> and the row at path with the given content type.
func putArtifact(t *testing.T, dir string, run *store.Run, path, contentType, content string) store.RunArtifact {
	t.Helper()
	at := time.Now().UTC()
	a := store.RunArtifact{ID: store.NewID(), RunID: run.ID, NodeID: "writer", Visit: 1, Path: path, Size: int64(len(content)),
		SHA256: strings.Repeat("ab", 32), ContentType: contentType, CreatedAt: at, UpdatedAt: at}
	got, _, err := store.UpsertRunArtifact(t.Context(), testPool, a)
	mustDo(t, err)
	if dir != "" {
		mustDo(t, os.MkdirAll(filepath.Join(dir, run.ID), 0o755))
		mustDo(t, os.WriteFile(filepath.Join(dir, run.ID, got.ID), []byte(content), 0o644))
	}
	return *got
}

func newArtifactRun(t *testing.T, pool *pgxpool.Pool) *store.Run {
	t.Helper()
	run, err := createTestRun(t.Context(), pool, store.TriggerManual, json.RawMessage(`{}`), store.Limits{MaxSteps: 10})
	mustDo(t, err)
	return run
}

// artifactRequest sends a request and returns the response with its body read.
func artifactRequest(t *testing.T, ts *httptest.Server, method, path string, header http.Header) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, nil)
	mustDo(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := ts.Client().Do(req)
	mustDo(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	mustDo(t, err)
	return resp, body
}

// DESIGN 5.25 rule 5 (T5.25.5): the list is the run's artifacts by path as the contract's
// Artifact, [] (never null) for a run that delivered nothing, never another run's, and 404
// for an unknown run.
func TestListArtifacts(t *testing.T) {
	pool := needDB(t)
	dir, ts := artifactServer(t, pool, nil)
	run, other := newArtifactRun(t, pool), newArtifactRun(t, pool)

	if code, body := do(t, ts, http.MethodGet, "/api/runs/"+run.ID+"/artifacts", ""); code != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("no artifacts: %d %s, want 200 []", code, body)
	}
	code, body := do(t, ts, http.MethodGet, "/api/runs/no-such-run/artifacts", "")
	if code != http.StatusNotFound || decodeError(t, body).Error.Code != "not_found" {
		t.Fatalf("unknown run: %d %s", code, body)
	}

	z := putArtifact(t, dir, run, "z-last.txt", "text/plain; charset=utf-8", "z")
	nested := putArtifact(t, dir, run, "reports/final.md", "text/markdown", "# final")
	patch := putArtifact(t, dir, run, "diff.patch", "text/x-patch", "diff --git")
	putArtifact(t, dir, other, "not-mine.txt", "text/plain", "other run")

	code, body = do(t, ts, http.MethodGet, "/api/runs/"+run.ID+"/artifacts", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	var got []store.RunArtifact
	mustDo(t, json.Unmarshal(body, &got))
	if len(got) != 3 || got[0].ID != patch.ID || got[1].ID != nested.ID || got[2].ID != z.ID {
		t.Fatalf("list = %+v, want diff.patch, reports/final.md, z-last.txt by path", got)
	}
	var fields []map[string]any
	mustDo(t, json.Unmarshal(body, &fields))
	for _, name := range []string{"id", "run_id", "node_id", "visit", "path", "size", "sha256", "content_type", "generated", "created_at", "updated_at"} {
		if _, ok := fields[1][name]; !ok {
			t.Errorf("an artifact lacks %q: %s", name, body)
		}
	}
	if got[1].RunID != run.ID || got[1].Size != int64(len("# final")) || got[1].ContentType != "text/markdown" {
		t.Errorf("artifact = %+v", got[1])
	}
}

// DESIGN 5.25 rule 5 (T5.25.5): a download is an attachment, never inline: the stored
// content type except for what a browser renders (HTML, XHTML, SVG, whatever the
// parameters or case), which is sent as plain text; the length of the file; the base name
// of the path as the file name with quotes, control characters and backslashes removed;
// nosniff; and no caching.
func TestDownloadArtifactHeaders(t *testing.T) {
	pool := needDB(t)
	dir, ts := artifactServer(t, pool, nil)
	run := newArtifactRun(t, pool)
	// Invisible characters, written as code points so none sits in this file.
	rlo, zwsp, lsep, psep, bom := string(rune(0x202e)), string(rune(0x200b)), string(rune(0x2028)), string(rune(0x2029)), string(rune(0xfeff))

	for _, c := range []struct {
		name, path, stored, content string
		wantType, wantFile          string
	}{
		{"markdown", "report.md", "text/markdown", "# Report\n", "text/markdown", "report.md"},
		{"nested path", "reports/2026/q3/summary.csv", "text/csv", "a,b\n1,2\n", "text/csv", "summary.csv"},
		{"json", "data.json", "application/json", `{"a":1}`, "application/json", "data.json"},
		{"detected text", "notes", "text/plain; charset=utf-8", "hello", "text/plain; charset=utf-8", "notes"},
		{"patch", "diff.patch", "text/x-patch", "diff --git a/x b/x\n", "text/x-patch", "diff.patch"},
		{"png", "chart.png", "image/png", "\x89PNG\r\n\x1a\n", "image/png", "chart.png"},
		{"binary", "blob.bin", "application/octet-stream", "\x00\x01\x02", "application/octet-stream", "blob.bin"},
		{"empty file", "empty.txt", "text/plain; charset=utf-8", "", "text/plain; charset=utf-8", "empty.txt"},
		{"html", "page.html", "text/html; charset=utf-8", "<script>alert(1)</script>", "text/plain; charset=utf-8", "page.html"},
		{"html without a parameter", "page2.html", "text/html", "<h1>x</h1>", "text/plain; charset=utf-8", "page2.html"},
		{"html in capitals", "page3.html", "TEXT/HTML; Charset=UTF-8", "<h1>x</h1>", "text/plain; charset=utf-8", "page3.html"},
		{"xhtml", "page.xhtml", "application/xhtml+xml", "<html xmlns=\"http://www.w3.org/1999/xhtml\"/>", "text/plain; charset=utf-8", "page.xhtml"},
		{"svg", "logo.svg", "image/svg+xml", "<svg xmlns=\"http://www.w3.org/2000/svg\" onload=\"alert(1)\"/>", "text/plain; charset=utf-8", "logo.svg"},
		{"a type that cannot be read", "odd.dat", "not a media type;;", "x", "application/octet-stream", "odd.dat"},
		{"quotes in the name", `drafts/final "v2".md`, "text/markdown", "x", "text/markdown", "final v2.md"},
		{"control characters in the name", "a\x01b\r\nSet-Cookie: x=1\x7f.txt", "text/plain", "x", "text/plain", "abSet-Cookie: x=1.txt"},
		{"a backslash that would escape the closing quote", `dir/name\`, "text/plain", "x", "text/plain", "name"},
		{"unicode", "café/résumé.md", "text/markdown", "x", "text/markdown", "résumé.md"},
		{"only characters that are removed", `"""`, "text/plain", "x", "text/plain", "artifact"},
		// Review L5: every XML type is sent as plain text, whatever it is called.
		{"application/xml", "feed.xml", "application/xml", "<a/>", "text/plain; charset=utf-8", "feed.xml"},
		{"text/xml with a parameter", "feed2.xml", "text/xml; charset=utf-8", "<a/>", "text/plain; charset=utf-8", "feed2.xml"},
		{"rss", "feed.rss", "application/rss+xml", "<rss/>", "text/plain; charset=utf-8", "feed.rss"},
		{"atom", "feed.atom", "application/atom+xml", "<feed/>", "text/plain; charset=utf-8", "feed.atom"},
		{"a vendor xml type", "map.kml", "application/vnd.google-earth.kml+xml", "<kml/>", "text/plain; charset=utf-8", "map.kml"},
		{"xml in capitals", "feed3.xml", "APPLICATION/XML", "<a/>", "text/plain; charset=utf-8", "feed3.xml"},
		{"json is not xml", "data2.json", "application/json", `{}`, "application/json", "data2.json"},
		// Review L4: format characters and the line and paragraph separators are dropped from
		// the name, as control characters are, so a saved name cannot read as another type.
		{"a right-to-left override", "reports/fatura" + rlo + "fdp.exe", "text/plain", "x", "text/plain", "faturafdp.exe"},
		{"separators, zero-width space and byte order mark", "a" + lsep + "b" + psep + "c" + zwsp + "d" + bom + ".md", "text/markdown", "x", "text/markdown", "abcd.md"},
		{"letters of other scripts are kept", "relatório/日本語.md", "text/markdown", "x", "text/markdown", "日本語.md"},
		{"only format characters", rlo + zwsp, "text/plain", "x", "text/plain", "artifact"},
	} {
		a := putArtifact(t, dir, run, c.path, c.stored, c.content)
		resp, body := artifactRequest(t, ts, http.MethodGet, "/api/runs/"+run.ID+"/artifacts/"+a.ID, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d: %s", c.name, resp.StatusCode, body)
			continue
		}
		if string(body) != c.content {
			t.Errorf("%s: body = %q, want %q", c.name, body, c.content)
		}
		want := map[string]string{
			"Content-Type":           c.wantType,
			"Content-Length":         itoa(int64(len(c.content))),
			"Content-Disposition":    `attachment; filename="` + c.wantFile + `"`,
			"X-Content-Type-Options": "nosniff",
			"Cache-Control":          "private, no-store",
		}
		for header, value := range want {
			if got := resp.Header.Get(header); got != value {
				t.Errorf("%s: %s = %q, want %q", c.name, header, got, value)
			}
		}
		if len(resp.Header.Values("Content-Disposition")) != 1 || resp.Header.Get("Set-Cookie") != "" {
			t.Errorf("%s: headers = %v", c.name, resp.Header)
		}
	}
}

// DESIGN 5.25 rule 5 (T5.25.5): 404 `not_found` when the artifact is not this run's (another
// run's id, an unknown id, an unknown run), and 404 with the message `artifact file is
// missing` when the row's file is gone or is not a file, never 500.
func TestDownloadArtifactNotFound(t *testing.T) {
	pool := needDB(t)
	dir, ts := artifactServer(t, pool, nil)
	run, other := newArtifactRun(t, pool), newArtifactRun(t, pool)
	mine := putArtifact(t, dir, run, "mine.txt", "text/plain", "mine")
	theirs := putArtifact(t, dir, other, "theirs.txt", "text/plain", "theirs")
	gone := putArtifact(t, dir, run, "gone.txt", "text/plain", "gone")
	mustDo(t, os.Remove(filepath.Join(dir, run.ID, gone.ID)))
	adir := putArtifact(t, dir, run, "a-directory.txt", "text/plain", "x")
	mustDo(t, os.Remove(filepath.Join(dir, run.ID, adir.ID)))
	mustDo(t, os.Mkdir(filepath.Join(dir, run.ID, adir.ID), 0o755))
	// A link in the store must not lead out of it.
	secret := filepath.Join(t.TempDir(), "secret")
	mustDo(t, os.WriteFile(secret, []byte("secret"), 0o644))
	linked := putArtifact(t, dir, run, "linked.txt", "text/plain", "x")
	mustDo(t, os.Remove(filepath.Join(dir, run.ID, linked.ID)))
	mustDo(t, os.Symlink(secret, filepath.Join(dir, run.ID, linked.ID)))

	for name, c := range map[string]struct{ path, message string }{
		"another run's artifact":  {"/api/runs/" + run.ID + "/artifacts/" + theirs.ID, "artifact not found"},
		"the same id, wrong run":  {"/api/runs/" + other.ID + "/artifacts/" + mine.ID, "artifact not found"},
		"an unknown artifact":     {"/api/runs/" + run.ID + "/artifacts/no-such-artifact", "artifact not found"},
		"an unknown run":          {"/api/runs/no-such-run/artifacts/" + mine.ID, "artifact not found"},
		"a missing file":          {"/api/runs/" + run.ID + "/artifacts/" + gone.ID, "artifact file is missing"},
		"a directory":             {"/api/runs/" + run.ID + "/artifacts/" + adir.ID, "artifact file is missing"},
		"a link out of the store": {"/api/runs/" + run.ID + "/artifacts/" + linked.ID, "artifact file is missing"},
	} {
		code, body := do(t, ts, http.MethodGet, c.path, "")
		e := decodeError(t, body)
		if code != http.StatusNotFound || e.Error.Code != "not_found" || e.Error.Message != c.message {
			t.Errorf("%s: %d %s, want 404 not_found %q", name, code, body, c.message)
		}
		if strings.Contains(string(body), "secret") {
			t.Errorf("%s: the body leaks the link's target: %s", name, body)
		}
	}
	// The two that exist still download.
	for _, a := range []store.RunArtifact{mine} {
		if code, body := do(t, ts, http.MethodGet, "/api/runs/"+run.ID+"/artifacts/"+a.ID, ""); code != http.StatusOK || string(body) != "mine" {
			t.Errorf("%s: %d %s", a.Path, code, body)
		}
	}
}

// DESIGN 5.25 rule 4 and T5.25.7: with the store disabled (no artifact directory) the list
// is empty and a download is 404; an unknown run is still 404.
func TestArtifactEndpointsWithADisabledStore(t *testing.T) {
	pool := needDB(t)
	_, ts := artifactServer(t, pool, func(c *config.Config) { c.ArtifactDir = "" })
	run := newArtifactRun(t, pool)
	a := putArtifact(t, "", run, "report.md", "text/markdown", "# r") // a row, but no store to read it from

	if code, body := do(t, ts, http.MethodGet, "/api/runs/"+run.ID+"/artifacts", ""); code != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("list: %d %s, want 200 []", code, body)
	}
	if code, body := do(t, ts, http.MethodGet, "/api/runs/"+run.ID+"/artifacts/"+a.ID, ""); code != http.StatusNotFound {
		t.Fatalf("download: %d %s, want 404", code, body)
	}
	if code, _ := do(t, ts, http.MethodGet, "/api/runs/no-such-run/artifacts", ""); code != http.StatusNotFound {
		t.Fatalf("unknown run: %d", code)
	}
}

// A download is a plain GET: HEAD answers the headers alone and a Range request the part
// asked for, with the same safe headers.
func TestDownloadArtifactHeadAndRange(t *testing.T) {
	pool := needDB(t)
	dir, ts := artifactServer(t, pool, nil)
	run := newArtifactRun(t, pool)
	a := putArtifact(t, dir, run, "digits.txt", "text/plain; charset=utf-8", "0123456789")
	path := "/api/runs/" + run.ID + "/artifacts/" + a.ID

	resp, body := artifactRequest(t, ts, http.MethodHead, path, nil)
	if resp.StatusCode != http.StatusOK || len(body) != 0 || resp.Header.Get("Content-Length") != "10" ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("HEAD: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	resp, body = artifactRequest(t, ts, http.MethodGet, path, http.Header{"Range": {"bytes=2-5"}})
	if resp.StatusCode != http.StatusPartialContent || string(body) != "2345" || resp.Header.Get("Content-Disposition") == "" ||
		resp.Header.Get("Cache-Control") != "private, no-store" || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Errorf("Range: %d %q %v", resp.StatusCode, body, resp.Header)
	}
}

// The download sits behind the same bearer token as the rest of /api when API_TOKEN is
// set: the run page fetches it with the token and saves it through a blob.
func TestDownloadArtifactNeedsTheTokenWhenOneIsSet(t *testing.T) {
	pool := needDB(t)
	dir, ts := artifactServer(t, pool, func(c *config.Config) { c.APIToken = "api-token-for-the-artifact-test" })
	run := newArtifactRun(t, pool)
	a := putArtifact(t, dir, run, "report.md", "text/markdown", "# r")
	path := "/api/runs/" + run.ID + "/artifacts/" + a.ID

	if resp, _ := artifactRequest(t, ts, http.MethodGet, path, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a token: %d", resp.StatusCode)
	}
	if resp, _ := artifactRequest(t, ts, http.MethodGet, "/api/runs/"+run.ID+"/artifacts", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("list without a token: %d", resp.StatusCode)
	}
	resp, body := artifactRequest(t, ts, http.MethodGet, path, http.Header{"Authorization": {"Bearer api-token-for-the-artifact-test"}})
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, []byte("# r")) {
		t.Errorf("with the token: %d %q", resp.StatusCode, body)
	}
}

// artifactConformanceSteps exercises both artifact paths against the contract (DESIGN
// 5.25 rule 5): the list with rows put there by hand, the list of a run with none, the
// download of a text file and of an HTML file, and the 404s (another run's artifact, an
// unknown artifact, an unknown run, a row whose file is gone). It writes the files to
// artifactStoreDir(), so the server the steps run against must be configured with
// ArtifactDir = artifactStoreDir(): the coordinator sets that in testConfig() and appends
// the steps to TestOpenAPIConformance.
func artifactConformanceSteps(t *testing.T) []conformanceStep {
	t.Helper()
	dir := artifactStoreDir()
	pool := needDB(t)
	mustDo(t, os.MkdirAll(dir, 0o755))
	run, other, empty := newArtifactRun(t, pool), newArtifactRun(t, pool), newArtifactRun(t, pool)
	t.Cleanup(func() {
		for _, r := range []*store.Run{run, other, empty} {
			_ = os.RemoveAll(filepath.Join(dir, r.ID))
		}
	})
	report := putArtifact(t, dir, run, "reports/summary.md", "text/markdown", "# Summary\n")
	page := putArtifact(t, dir, run, "page.html", "text/html; charset=utf-8", "<h1>page</h1>")
	gone := putArtifact(t, dir, run, "gone.txt", "text/plain; charset=utf-8", "gone")
	mustDo(t, os.Remove(filepath.Join(dir, run.ID, gone.ID)))
	theirs := putArtifact(t, dir, other, "theirs.txt", "text/plain; charset=utf-8", "theirs")

	base := "/api/runs/" + run.ID + "/artifacts"
	return []conformanceStep{
		{http.MethodGet, base, "", http.StatusOK},
		{http.MethodGet, "/api/runs/" + empty.ID + "/artifacts", "", http.StatusOK},
		{http.MethodGet, "/api/runs/no-such-run/artifacts", "", http.StatusNotFound},
		{http.MethodGet, base + "/" + report.ID, "", http.StatusOK},
		{http.MethodGet, base + "/" + page.ID, "", http.StatusOK},
		{http.MethodGet, base + "/" + theirs.ID, "", http.StatusNotFound},
		{http.MethodGet, base + "/no-such-artifact", "", http.StatusNotFound},
		{http.MethodGet, base + "/" + gone.ID, "", http.StatusNotFound},
		{http.MethodGet, "/api/runs/no-such-run/artifacts/" + report.ID, "", http.StatusNotFound},
	}
}

// TestArtifactConformanceSteps runs the artifact steps against the contract on a server
// of its own, configured as the conformance server must be.
func TestArtifactConformanceSteps(t *testing.T) {
	pool := needDB(t)
	router := contract(t)
	_, ts := artifactServer(t, pool, func(c *config.Config) { c.ArtifactDir = artifactStoreDir() })
	for _, st := range artifactConformanceSteps(t) {
		code, body := checkConformance(t, router, ts.URL, st.method, st.path, st.body, st.want < 300)
		if code != st.want {
			t.Fatalf("%s %s: status %d, want %d: %s", st.method, st.path, code, st.want, body)
		}
	}
}

// Review L1: an id with a NUL in it is no run's and no artifact's: 404 as an unknown id is,
// where the database would refuse the parameter and the handler answer 500.
func TestArtifactRoutesAnswerNotFoundForIDsWithANUL(t *testing.T) {
	pool := needDB(t)
	dir, ts := artifactServer(t, pool, nil)
	run := newArtifactRun(t, pool)
	a := putArtifact(t, dir, run, "report.md", "text/markdown", "# r")
	for name, path := range map[string]string{
		"the list of a run":              "/api/runs/no%00such/artifacts",
		"a download, the run":            "/api/runs/no%00such/artifacts/" + a.ID,
		"a download, the artifact":       "/api/runs/" + run.ID + "/artifacts/no%00such",
		"a download, both":               "/api/runs/%00/artifacts/%00",
		"a control character in the run": "/api/runs/no%01such/artifacts",
	} {
		code, body := do(t, ts, http.MethodGet, path, "")
		if code != http.StatusNotFound || decodeError(t, body).Error.Code != "not_found" {
			t.Errorf("%s: %d %s, want 404 not_found", name, code, body)
		}
	}
	if code, _ := do(t, ts, http.MethodGet, "/api/runs/"+run.ID+"/artifacts/"+a.ID, ""); code != http.StatusOK {
		t.Errorf("the real artifact: %d", code)
	}
}
