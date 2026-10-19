package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"yuno/internal/skillbundle"
	"yuno/internal/store"
)

type zf struct {
	name string
	data string
	mode os.FileMode
}

func zipBytes(t *testing.T, files ...zf) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		h := &zip.FileHeader{Name: f.name, Method: zip.Deflate}
		if f.mode == 0 {
			f.mode = 0o644
		}
		h.SetMode(f.mode)
		w, err := zw.CreateHeader(h)
		mustDo(t, err)
		_, err = w.Write([]byte(f.data))
		mustDo(t, err)
	}
	mustDo(t, zw.Close())
	return buf.Bytes()
}

func uploadZip(t *testing.T, ts *httptest.Server, query string, zipData []byte) (int, []byte) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "bundle.zip")
	mustDo(t, err)
	_, err = fw.Write(zipData)
	mustDo(t, err)
	mustDo(t, mw.Close())
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/skills/import"+query, &body)
	mustDo(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := ts.Client().Do(req)
	mustDo(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func skillMD(name, desc, body string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n" + body
}

func errCode(t *testing.T, b []byte) string {
	t.Helper()
	return decodeError(t, b).Error.Code
}

func listedFiles(t *testing.T, ts *httptest.Server, id string) map[string]int {
	t.Helper()
	code, body := do(t, ts, http.MethodGet, "/api/skills/"+id+"/files", "")
	var fl []struct {
		Path string `json:"path"`
		Size int    `json:"size"`
		Mode int    `json:"mode"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &fl) != nil {
		t.Fatalf("list files: %d %s", code, body)
	}
	out := map[string]int{}
	for _, f := range fl {
		out[f.Path] = f.Mode
	}
	return out
}

// T5.27.5: a zip imports with its single top folder stripped, front matter read and modes
// kept; the export gives the bundle back and imports to the same skill.
func TestSkillZipImportAndExport(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	name := "zipped-" + uniqueSuffix()
	z := zipBytes(t,
		zf{name: name + "/SKILL.md", data: skillMD(name, "Zip description", "# Zip\n\nBody text.\n")},
		zf{name: name + "/scripts/run.sh", data: "#!/bin/sh\necho hi\n", mode: 0o755},
		zf{name: name + "/refs/a.md", data: "reference\n"},
	)
	code, body := uploadZip(t, ts, "", z)
	if code != http.StatusCreated {
		t.Fatalf("import: %d %s", code, body)
	}
	sk := decodeSkill(t, body)
	if sk.Name != name || sk.Description != "Zip description" || sk.Body != "# Zip\n\nBody text.\n" || sk.Source != "upload" || sk.FilesCount != 2 {
		t.Fatalf("skill = %+v", sk)
	}
	t.Cleanup(func() { _ = store.DeleteSkill(context.Background(), pool, sk.ID) })
	if got := listedFiles(t, ts, sk.ID); got["scripts/run.sh"] != 0o755 || got["refs/a.md"] != 0o644 || len(got) != 2 {
		t.Fatalf("files = %v", got)
	}
	code, body = do(t, ts, http.MethodGet, "/api/skills/"+sk.ID, "")
	if code != http.StatusOK || decodeSkill(t, body).FilesCount != 2 {
		t.Fatalf("get: %d %s", code, body)
	}
	_, body = do(t, ts, http.MethodGet, "/api/skills", "")
	var list []store.Skill
	mustDo(t, json.Unmarshal(body, &list))
	if i := slices.IndexFunc(list, func(s store.Skill) bool { return s.ID == sk.ID }); i < 0 || list[i].Source != "upload" || list[i].FilesCount != 2 {
		t.Fatalf("list entry = %+v", list)
	}

	// 409 without replace.
	code, body = uploadZip(t, ts, "", z)
	if code != http.StatusConflict || errCode(t, body) != "skill_exists" {
		t.Fatalf("duplicate: %d %s", code, body)
	}

	// Export, then import the export under a new name by deleting the original first.
	resp, err := ts.Client().Get(ts.URL + "/api/skills/" + sk.ID + "/export")
	mustDo(t, err)
	exported, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/zip" ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), `filename="`+name+`.zip"`) {
		t.Fatalf("export: %d %v", resp.StatusCode, resp.Header)
	}
	zr, err := zip.NewReader(bytes.NewReader(exported), int64(len(exported)))
	mustDo(t, err)
	modes := map[string]os.FileMode{}
	var md string
	for _, f := range zr.File {
		modes[f.Name] = f.Mode().Perm()
		if f.Name == name+"/SKILL.md" {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			md = string(b)
		}
	}
	if modes[name+"/scripts/run.sh"] != 0o755 || modes[name+"/refs/a.md"] != 0o644 || modes[name+"/SKILL.md"] != 0o644 || len(modes) != 3 {
		t.Fatalf("export modes = %v", modes)
	}
	if !strings.HasPrefix(md, "---\nname: "+name+"\ndescription: \"Zip description\"\n---\n\n# Zip") {
		t.Fatalf("exported SKILL.md = %q", md)
	}
	if c, _ := do(t, ts, http.MethodDelete, "/api/skills/"+sk.ID, ""); c != http.StatusNoContent {
		t.Fatal("delete")
	}
	code, body = uploadZip(t, ts, "", exported)
	if code != http.StatusCreated {
		t.Fatalf("re-import: %d %s", code, body)
	}
	again := decodeSkill(t, body)
	t.Cleanup(func() { _ = store.DeleteSkill(context.Background(), pool, again.ID) })
	if again.Body != sk.Body || again.Description != sk.Description || again.FilesCount != 2 || listedFiles(t, ts, again.ID)["scripts/run.sh"] != 0o755 {
		t.Fatalf("round trip = %+v", again)
	}
}

func TestSkillZipImportErrors(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	name := "bad-" + uniqueSuffix()
	good := zf{name: "SKILL.md", data: skillMD(name, "d", "b")}

	// Missing field and not multipart.
	code, body := do(t, ts, http.MethodPost, "/api/skills/import", `{}`)
	if code != http.StatusBadRequest {
		t.Errorf("json body: %d %s", code, body)
	}
	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	_ = mw.WriteField("other", "x")
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/skills/import", &mp)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := ts.Client().Do(req)
	mustDo(t, err)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity || !slices.Equal(detailPaths(t, b), []string{"file"}) {
		t.Errorf("no file: %d %s", resp.StatusCode, b)
	}
	if code, body := uploadZip(t, ts, "?replace=maybe", zipBytes(t, good)); code != http.StatusUnprocessableEntity {
		t.Errorf("replace=maybe: %d %s", code, body)
	}

	// T5.27.6 over HTTP: traversal, symlink and several SKILL.md are 400, caps are 413,
	// a bad name is 422. Nothing is stored.
	var many []zf
	many = append(many, good)
	for i := 0; i <= skillbundle.MaxFiles; i++ {
		many = append(many, zf{name: fmt.Sprintf("f/%03d", i), data: "x"})
	}
	for _, tc := range []struct {
		what  string
		z     []byte
		want  int
		check string
	}{
		{"traversal", zipBytes(t, good, zf{name: "../evil", data: "x"}), 400, ""},
		{"absolute", zipBytes(t, good, zf{name: "/evil", data: "x"}), 400, ""},
		{"symlink", zipBytes(t, good, zf{name: "ln", data: "/etc/passwd", mode: os.ModeSymlink | 0o777}), 400, ""},
		{"two SKILL.md", zipBytes(t, zf{name: "a/SKILL.md", data: good.data}, zf{name: "b/SKILL.md", data: good.data}), 400, "several SKILL.md"},
		{"none", zipBytes(t, zf{name: "readme", data: "x"}), 400, ""},
		{"not a zip", []byte("plain"), 400, ""},
		{"201 files", zipBytes(t, many...), 413, "200 files"},
		{"big file", zipBytes(t, good, zf{name: "big", data: strings.Repeat("a", skillbundle.MaxFileBytes+1)}), 413, "per-file"},
		{"bad name", zipBytes(t, zf{name: "SKILL.md", data: skillMD("Bad_Name", "d", "b")}), 422, ""},
	} {
		code, body := uploadZip(t, ts, "", tc.z)
		if code != tc.want || (tc.check != "" && !strings.Contains(string(body), tc.check)) {
			t.Errorf("%s: %d %s", tc.what, code, body)
		}
	}
	if same, _ := store.ListSkillsByName(context.Background(), pool, []string{name}); len(same) != 0 {
		t.Errorf("a refused import stored %v", same)
	}
	// A request over 25 MiB is 413 before anything is read into memory.
	big := zipBytes(t, good, zf{name: "x.bin", data: strings.Repeat("a", 10)})
	big = append(big, make([]byte, 26<<20)...)
	if code, body := uploadZip(t, ts, "", big); code != http.StatusRequestEntityTooLarge {
		t.Errorf("26 MiB: %d %s", code, body)
	}
}

// T5.27.7: replace swaps body, source and files together, re-materialises the agents that
// hold the skill, and a refused replace leaves the skill as it was.
func TestSkillZipReplace(t *testing.T) {
	pool := needDB(t)
	fe := &fakeEngine{pool: pool}
	_, ts := newTestServer(t, pool, fe, nil)
	name := "repl-" + uniqueSuffix()
	v1 := zipBytes(t, zf{name: "SKILL.md", data: skillMD(name, "one", "body one")}, zf{name: "old.txt", data: "old"}, zf{name: "keep.sh", data: "#!/bin/sh", mode: 0o755})
	code, body := uploadZip(t, ts, "", v1)
	if code != http.StatusCreated {
		t.Fatalf("v1: %d %s", code, body)
	}
	sk := decodeSkill(t, body)
	t.Cleanup(func() { _ = store.DeleteSkill(context.Background(), pool, sk.ID) })
	user := agentWithSkills(t, "repl-user", name)
	fe.mu.Lock()
	fe.materialized = nil
	fe.mu.Unlock()

	// Refused (cap): the skill is untouched.
	tooBig := zipBytes(t, zf{name: "SKILL.md", data: skillMD(name, "bad", "bad body")}, zf{name: "big", data: strings.Repeat("a", skillbundle.MaxFileBytes+1)})
	if code, _ := uploadZip(t, ts, "?replace=true", tooBig); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("refused replace: %d", code)
	}
	_, body = do(t, ts, http.MethodGet, "/api/skills/"+sk.ID, "")
	if cur := decodeSkill(t, body); cur.Body != "body one" || cur.FilesCount != 2 {
		t.Fatalf("after refused replace = %+v", cur)
	}

	v2 := zipBytes(t, zf{name: name + "/SKILL.md", data: "---\nname: " + name + "\ndescription: two\nlicense: MIT\n---\nbody two"}, zf{name: name + "/new.txt", data: "new"})
	code, body = uploadZip(t, ts, "?replace=true", v2)
	if code != http.StatusOK {
		t.Fatalf("replace: %d %s", code, body)
	}
	got := decodeSkill(t, body)
	if got.ID != sk.ID || got.Description != "two" || got.Body != "body two" || got.License != "MIT" || got.Source != "upload" || got.FilesCount != 1 {
		t.Fatalf("replaced = %+v", got)
	}
	if files := listedFiles(t, ts, sk.ID); len(files) != 1 || files["new.txt"] != 0o644 {
		t.Fatalf("files after replace = %v", files)
	}
	fe.mu.Lock()
	mat := slices.Clone(fe.materialized)
	fe.mu.Unlock()
	if !slices.Equal(mat, []string{user.ID}) {
		t.Fatalf("materialised %v, want the holder", mat)
	}
}

// The unit of the replace is one transaction: when storing the files fails the body, the
// source and the old files are all as before.
func TestSkillReplaceIsOneTransaction(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	sk := &store.Skill{Name: "tx-" + uniqueSuffix(), Description: "d", Body: "old body"}
	mustDo(t, store.CreateSkill(ctx, pool, sk))
	t.Cleanup(func() { _ = store.DeleteSkill(ctx, pool, sk.ID) })
	mustDo(t, store.ReplaceSkillFiles(ctx, pool, sk.ID, []store.SkillFile{{Path: "old.txt", Content: []byte("old"), Mode: 0o644}}))

	tx, err := pool.Begin(ctx)
	mustDo(t, err)
	mustDo(t, store.UpdateSkill(ctx, tx, &store.Skill{ID: sk.ID, Name: sk.Name, Description: "new", Body: "new body"}))
	mustDo(t, store.SetSkillSource(ctx, tx, sk.ID, "upload", "", "", "MIT"))
	err = store.ReplaceSkillFiles(ctx, tx, sk.ID, []store.SkillFile{
		{Path: "new.txt", Content: []byte("new"), Mode: 0o644},
		{Path: "bad.txt", Content: []byte("x"), Mode: 0o777}, // violates the mode check
	})
	if err == nil {
		t.Fatal("bad mode accepted")
	}
	_ = tx.Rollback(ctx)
	cur, err := store.GetSkill(ctx, pool, sk.ID)
	mustDo(t, err)
	files, err := store.ListSkillFileMeta(ctx, pool, sk.ID)
	mustDo(t, err)
	if cur.Body != "old body" || cur.Source != "user" || len(files) != 1 || files[0].Path != "old.txt" {
		t.Fatalf("after rollback: %+v %+v", cur, files)
	}
}

// fakeGit scripts the git runner: a clone writes the repository, the other commands
// succeed. It records every command it was asked to run.
type fakeGit struct {
	files map[string]string // path in the clone -> content
	sha   string
	fail  string
	cmds  [][]string
}

func (g *fakeGit) run(ctx context.Context, dir string, args ...string) (string, error) {
	g.cmds = append(g.cmds, args)
	sub := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		sub = args[i]
		break
	}
	switch sub {
	case "clone":
		if g.fail != "" {
			return "", errors.New(g.fail)
		}
		dest := args[len(args)-1]
		for p, c := range g.files {
			full := filepath.Join(dest, p)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return "", err
			}
			mode := os.FileMode(0o644)
			if strings.HasSuffix(p, ".sh") {
				mode = 0o755
			}
			if err := os.WriteFile(full, []byte(c), mode); err != nil {
				return "", err
			}
			_ = os.Chmod(full, mode)
		}
	case "rev-parse":
		return g.sha + "\n", nil
	}
	return "", nil
}

func (g *fakeGit) installed(t *testing.T, client *http.Client) {
	t.Helper()
	old := newGitImporter
	newGitImporter = func(hosts []string) *skillbundle.GitImporter {
		return &skillbundle.GitImporter{Hosts: hosts, Run: g.run, Client: client, TempDir: t.TempDir()}
	}
	t.Cleanup(func() { newGitImporter = old })
}

type rt func(*http.Request) (*http.Response, error)

func (f rt) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okClient() *http.Client {
	return &http.Client{Transport: rt(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
	})}
}

// T5.27.8: the host allowlist, credentials, scheme and redirects; a good import records the
// repository and the resolved commit; a failing clone is a 502 with git's first line.
func TestSkillGitImport(t *testing.T) {
	pool := needDB(t)
	cfg := testConfig()
	cfg.SkillImportHosts = []string{"github.com", "gitlab.com"}
	_, ts := newTestServerDeps(t, Deps{Cfg: cfg, Pool: pool}, &fakeEngine{pool: pool})
	name := "gitted-" + uniqueSuffix()
	sha := strings.Repeat("ab", 20)
	g := &fakeGit{sha: sha, files: map[string]string{
		"skills/demo/SKILL.md":       skillMD(name, "From git", "git body"),
		"skills/demo/scripts/run.sh": "#!/bin/sh\n",
		"README.md":                  "root",
	}}
	g.installed(t, okClient())
	post := func(body string) (int, []byte) { return do(t, ts, http.MethodPost, "/api/skills/import-git", body) }

	for what, body := range map[string]string{
		"host not allowed": `{"url":"https://evil.example/o/r"}`,
		"credentials":      `{"url":"https://user:pw@github.com/o/r"}`,
		"http":             `{"url":"http://github.com/o/r"}`,
		"ssh":              `{"url":"ssh://git@github.com/o/r"}`,
		"look-alike host":  `{"url":"https://github.com.evil.example/o/r"}`,
		"flag as ref":      `{"url":"https://github.com/o/r","ref":"--upload-pack=x"}`,
		"escaping path":    `{"url":"https://github.com/o/r","path":"../x"}`,
		"unknown field":    `{"url":"https://github.com/o/r","extra":1}`,
	} {
		code, b := post(body)
		if code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", what, code, b)
		}
	}
	if len(g.cmds) != 0 {
		t.Fatalf("git ran for a refused request: %v", g.cmds)
	}

	// A redirect off the allowlist is refused before git runs.
	g.installed(t, &http.Client{Transport: rt(func(r *http.Request) (*http.Response, error) {
		h := http.Header{}
		h.Set("Location", "https://evil.example/o/r/info/refs?service=git-upload-pack")
		return &http.Response{StatusCode: 302, Header: h, Body: http.NoBody, Request: r}, nil
	})})
	code, b := post(`{"url":"https://github.com/o/r"}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "redirects off the allowed hosts") || len(g.cmds) != 0 {
		t.Fatalf("redirect: %d %s (git ran %d times)", code, b, len(g.cmds))
	}

	// A good import.
	g.installed(t, okClient())
	code, b = post(`{"url":"https://github.com/o/r/","path":"skills/demo","ref":"main"}`)
	if code != http.StatusCreated {
		t.Fatalf("import: %d %s", code, b)
	}
	sk := decodeSkill(t, b)
	t.Cleanup(func() { _ = store.DeleteSkill(context.Background(), pool, sk.ID) })
	if sk.Name != name || sk.Source != "git" || sk.SourceURL != "https://github.com/o/r" || sk.SourceRef != sha || sk.FilesCount != 1 || sk.Body != "git body" {
		t.Fatalf("skill = %+v", sk)
	}
	if f := listedFiles(t, ts, sk.ID); f["scripts/run.sh"] != 0o755 || len(f) != 1 {
		t.Fatalf("files = %v", f)
	}
	joined := fmt.Sprint(g.cmds)
	for _, want := range []string{"protocol.https.allow=always", "http.followRedirects=false", "--depth", "--no-checkout", "sparse-checkout", "skills/demo"} {
		if !strings.Contains(joined, want) {
			t.Errorf("git commands lack %q: %s", want, joined)
		}
	}
	// Again: 409 without replace, 200 with it.
	if code, b := post(`{"url":"https://github.com/o/r","path":"skills/demo"}`); code != http.StatusConflict {
		t.Errorf("again: %d %s", code, b)
	}
	if code, b := do(t, ts, http.MethodPost, "/api/skills/import-git?replace=true", `{"url":"https://github.com/o/r","path":"skills/demo"}`); code != http.StatusOK {
		t.Errorf("replace: %d %s", code, b)
	}

	// A clone that fails: 502 import_failed with git's line.
	g.fail = "fatal: repository 'https://github.com/o/gone/' not found"
	code, b = post(`{"url":"https://github.com/o/gone"}`)
	e := decodeError(t, b).Error
	if code != http.StatusBadGateway || e.Code != "import_failed" || e.Message != g.fail {
		t.Errorf("failure: %d %s", code, b)
	}
	// No SKILL.md in the checked-out path: 400.
	g.fail, g.files = "", map[string]string{"x/readme.md": "x"}
	if code, b := post(`{"url":"https://github.com/o/r","path":"x"}`); code != http.StatusBadRequest {
		t.Errorf("no SKILL.md: %d %s", code, b)
	}
}

// T5.27.10 at the server: the library import adds what is missing, is idempotent and never
// overwrites a skill of the same name.
func TestServerImportsSkillLibrary(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	lib := t.TempDir()
	fresh, mine := "lib-fresh-"+uniqueSuffix(), "lib-mine-"+uniqueSuffix()
	for _, n := range []string{fresh, mine} {
		dir := filepath.Join(lib, "openclaw", n)
		mustDo(t, os.MkdirAll(dir, 0o755))
		mustDo(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMD(n, "from the library", "library body")), 0o644))
		mustDo(t, os.WriteFile(filepath.Join(dir, "tool.sh"), []byte("#!/bin/sh\n"), 0o755))
	}
	mustDo(t, os.WriteFile(filepath.Join(lib, "openclaw", "LICENSE"), []byte("MIT License"), 0o644))
	own := &store.Skill{Name: mine, Description: "operator's", Body: "operator's text"}
	mustDo(t, store.CreateSkill(ctx, pool, own))
	t.Cleanup(func() {
		_ = store.DeleteSkill(ctx, pool, own.ID)
		if same, _ := store.ListSkillsByName(ctx, pool, []string{fresh}); len(same) == 1 {
			_ = store.DeleteSkill(ctx, pool, same[0].ID)
		}
	})
	cfg := testConfig()
	cfg.SkillsLibraryDir = lib
	s, ts := newTestServerDeps(t, Deps{Cfg: cfg, Pool: pool}, &fakeEngine{pool: pool})
	for i := 0; i < 2; i++ { // the second run changes nothing
		s.ImportSkillLibrary(ctx)
	}
	same, err := store.ListSkillsByName(ctx, pool, []string{fresh, mine})
	mustDo(t, err)
	if len(same) != 2 {
		t.Fatalf("skills = %+v", same)
	}
	for _, sk := range same {
		switch sk.Name {
		case fresh:
			if sk.Source != "library" || sk.License != "MIT" || sk.SourceURL != "https://github.com/openclaw/openclaw/tree/main/skills/"+fresh || sk.FilesCount != 2 {
				t.Errorf("imported = %+v", sk)
			}
			if f := listedFiles(t, ts, sk.ID); f["tool.sh"] != 0o755 || f["LICENSE"] != 0o644 {
				t.Errorf("files = %v", f)
			}
		case mine:
			if sk.Source != "user" || sk.Body != "operator's text" || sk.FilesCount != 0 {
				t.Errorf("existing skill changed: %+v", sk)
			}
		}
	}
}

// T5.27.11: edit, read, delete and export a bundle's files; the agents that hold the skill
// are re-materialised on every change.
func TestSkillFileAPI(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	fe := &fakeEngine{pool: pool}
	_, ts := newTestServer(t, pool, fe, nil)
	name := "files-" + uniqueSuffix()
	sk := &store.Skill{Name: name, Description: "d", Body: "b"}
	mustDo(t, store.CreateSkill(ctx, pool, sk))
	t.Cleanup(func() { _ = store.DeleteSkill(ctx, pool, sk.ID) })
	user := agentWithSkills(t, "files-user", name)
	other := agentWithSkills(t, "files-other")
	base := "/api/skills/" + sk.ID
	materialized := func() []string {
		fe.mu.Lock()
		defer fe.mu.Unlock()
		out := slices.Clone(fe.materialized)
		fe.materialized = nil
		return out
	}
	materialized()

	if code, b := do(t, ts, http.MethodGet, base+"/files", ""); code != http.StatusOK || strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("empty list: %d %s", code, b)
	}

	// Create, with a mode.
	code, b := do(t, ts, http.MethodPut, base+"/files/scripts/run.sh", `{"content":"#!/bin/sh\necho 1\n","mode":493}`)
	var fb skillFileBody
	if code != http.StatusOK || json.Unmarshal(b, &fb) != nil || fb.Path != "scripts/run.sh" || fb.Mode != 493 || fb.Size != 17 || fb.Content != "#!/bin/sh\necho 1\n" {
		t.Fatalf("create: %d %s", code, b)
	}
	if m := materialized(); !slices.Equal(m, []string{user.ID}) {
		t.Fatalf("materialised %v, want only the holder %s (not %s)", m, user.ID, other.ID)
	}
	// Replace keeps the mode when none is sent; the fetched shape can be PUT back.
	code, b = do(t, ts, http.MethodPut, base+"/files/scripts/run.sh", `{"path":"scripts/run.sh","size":3,"content":"v2\n"}`)
	if code != http.StatusOK || !strings.Contains(string(b), `"mode":493`) || !strings.Contains(string(b), `"content":"v2\n"`) {
		t.Fatalf("replace: %d %s", code, b)
	}
	code, b = do(t, ts, http.MethodGet, base+"/files/scripts/run.sh", "")
	if code != http.StatusOK || !strings.Contains(string(b), `"content":"v2\n"`) {
		t.Fatalf("get: %d %s", code, b)
	}
	if got := listedFiles(t, ts, sk.ID); got["scripts/run.sh"] != 493 || len(got) != 1 {
		t.Fatalf("list = %v", got)
	}
	cur, _ := store.GetSkill(ctx, pool, sk.ID)
	if cur.FilesCount != 1 {
		t.Fatalf("files_count = %d", cur.FilesCount)
	}

	// Binary and large files are listed but not shown: 415.
	mustDo(t, store.PutSkillFile(ctx, pool, sk.ID, &store.SkillFile{Path: "img.bin", Content: []byte{0xff, 0xfe, 0, 1}, Mode: 0o644}))
	mustDo(t, store.PutSkillFile(ctx, pool, sk.ID, &store.SkillFile{Path: "big.txt", Content: bytes.Repeat([]byte("a"), 256<<10+1), Mode: 0o644}))
	for _, p := range []string{"img.bin", "big.txt"} {
		if code, b := do(t, ts, http.MethodGet, base+"/files/"+p, ""); code != http.StatusUnsupportedMediaType {
			t.Errorf("get %s: %d %s", p, code, b)
		}
	}
	if code, _ := do(t, ts, http.MethodGet, base+"/files/nothing.txt", ""); code != http.StatusNotFound {
		t.Errorf("missing file: %d", code)
	}

	// Refusals: bad paths and modes (422), over 256 KiB (413), unknown skill (404).
	for _, tc := range []struct {
		path, body string
		want       int
	}{
		{"SKILL.md", `{"content":"x"}`, 422},
		{".hidden/x", `{"content":"x"}`, 422},
		{"a%5Cb", `{"content":"x"}`, 422},
		{"a/x", `{"content":"x","mode":511}`, 422},
		{"a/x", `{}`, 422},
		{"a/x", `{"content":"x","other":1}`, 422},
		{"a/x", `{"content":"` + strings.Repeat("a", 256<<10+1) + `"}`, 413},
	} {
		if code, b := do(t, ts, http.MethodPut, base+"/files/"+tc.path, tc.body); code != tc.want {
			t.Errorf("PUT %s %.40s: %d %.200s", tc.path, tc.body, code, b)
		}
	}
	if code, _ := do(t, ts, http.MethodPut, "/api/skills/nope/files/a.txt", `{"content":"x"}`); code != http.StatusNotFound {
		t.Errorf("unknown skill: %d", code)
	}
	materialized()

	// Delete.
	if code, b := do(t, ts, http.MethodDelete, base+"/files/scripts/run.sh", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, b)
	}
	if m := materialized(); !slices.Equal(m, []string{user.ID}) {
		t.Fatalf("materialised after delete: %v", m)
	}
	if code, _ := do(t, ts, http.MethodDelete, base+"/files/scripts/run.sh", ""); code != http.StatusNotFound {
		t.Errorf("delete twice: %d", code)
	}
	if _, ok := listedFiles(t, ts, sk.ID)["scripts/run.sh"]; ok {
		t.Error("deleted file still listed")
	}

	// A failed materialise keeps the edit and answers 502.
	fe.mu.Lock()
	fe.materializeErr = errors.New("gateway down")
	fe.mu.Unlock()
	code, b = do(t, ts, http.MethodPut, base+"/files/kept.txt", `{"content":"kept"}`)
	if code != http.StatusBadGateway || errCode(t, b) != "materialize_failed" {
		t.Fatalf("502: %d %s", code, b)
	}
	if f, err := store.GetSkillFile(ctx, pool, sk.ID, "kept.txt"); err != nil || string(f.Content) != "kept" {
		t.Fatalf("edit not stored: %v", err)
	}
	// Export includes the files with their modes.
	resp, err := ts.Client().Get(ts.URL + base + "/export")
	mustDo(t, err)
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	mustDo(t, err)
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	slices.Sort(names)
	if want := []string{name + "/SKILL.md", name + "/big.txt", name + "/img.bin", name + "/kept.txt"}; !slices.Equal(names, want) {
		t.Fatalf("export entries = %v", names)
	}
	if code, _ := do(t, ts, http.MethodGet, "/api/skills/nope/export", ""); code != http.StatusNotFound {
		t.Errorf("export unknown: %d", code)
	}
}

func TestSkillFileCaps(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	sk := &store.Skill{Name: "caps-" + uniqueSuffix()}
	mustDo(t, store.CreateSkill(ctx, pool, sk))
	t.Cleanup(func() { _ = store.DeleteSkill(ctx, pool, sk.ID) })
	var files []store.SkillFile
	for i := 0; i < skillbundle.MaxFiles; i++ {
		files = append(files, store.SkillFile{Path: fmt.Sprintf("f%03d.txt", i), Content: []byte("x"), Mode: 0o644})
	}
	mustDo(t, store.ReplaceSkillFiles(ctx, pool, sk.ID, files))
	base := "/api/skills/" + sk.ID + "/files/"
	if code, b := do(t, ts, http.MethodPut, base+"one-more.txt", `{"content":"x"}`); code != http.StatusRequestEntityTooLarge {
		t.Errorf("201st file: %d %s", code, b)
	}
	if code, b := do(t, ts, http.MethodPut, base+"f000.txt", `{"content":"replaced"}`); code != http.StatusOK {
		t.Errorf("replace at the cap: %d %s", code, b)
	}
	// Total size: 19 files of 1 MiB fill the budget, the next one does not fit.
	var big []store.SkillFile
	for i := 0; i < 20; i++ {
		big = append(big, store.SkillFile{Path: fmt.Sprintf("big%d", i), Content: make([]byte, 1<<20), Mode: 0o644})
	}
	mustDo(t, store.ReplaceSkillFiles(ctx, pool, sk.ID, big))
	if code, b := do(t, ts, http.MethodPut, base+"extra.txt", `{"content":"x"}`); code != http.StatusRequestEntityTooLarge {
		t.Errorf("past 20 MiB: %d %s", code, b)
	}
}

// Finding 9: a 422 from an import carries the reasons in its message.
func TestSkillImportValidationMessage(t *testing.T) {
	pool := needDB(t)
	cfg := testConfig()
	cfg.SkillImportHosts = []string{"github.com"}
	_, ts := newTestServerDeps(t, Deps{Cfg: cfg, Pool: pool}, &fakeEngine{pool: pool})
	code, b := do(t, ts, http.MethodPost, "/api/skills/import-git", `{"url":"https://evil.example/o/r"}`)
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &e)
	if code != http.StatusUnprocessableEntity || !strings.HasPrefix(e.Error.Message, `invalid skill: url: host "evil.example" is not allowed (`) {
		t.Fatalf("%d %s", code, b)
	}
	var fe fieldErrors
	fe.add("name", "must be lowercase")
	fe.add("description", "is required")
	rec := httptest.NewRecorder()
	writeReasons(rec, "invalid skill", fe)
	if !strings.Contains(rec.Body.String(), `"message":"invalid skill: name: must be lowercase; description: is required"`) {
		t.Fatalf("joined reasons: %s", rec.Body.String())
	}
}
