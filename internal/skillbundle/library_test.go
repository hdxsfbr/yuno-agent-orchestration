package skillbundle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/store"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		ctx := context.Background()
		pool, err := store.Open(ctx, url)
		if err != nil {
			fmt.Fprintln(os.Stderr, "skillbundle tests: open database:", err)
			os.Exit(1)
		}
		if err := store.Migrate(ctx, pool, slog.New(slog.DiscardHandler)); err != nil {
			fmt.Fprintln(os.Stderr, "skillbundle tests: migrate:", err)
			os.Exit(1)
		}
		testPool = pool
	}
	code := m.Run()
	if testPool != nil {
		testPool.Close()
	}
	os.Exit(code)
}

// inTx runs fn in a transaction that is rolled back, so the shared test database is left
// as it was and no committed library skill can collide with other packages' tests.
func inTx(t *testing.T, fn func(ctx context.Context, tx pgx.Tx)) {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	fn(ctx, tx)
}

var quiet = slog.New(slog.DiscardHandler)

const realLibrary = "../../skills-library"

// T5.27.10 against the real skills-library/: every skill imports, a second run imports
// nothing, and an existing skill of the same name is never overwritten.
func TestImportLibraryReal(t *testing.T) {
	inTx(t, func(ctx context.Context, tx pgx.Tx) {
		// A skill the operator already has: left alone.
		mine := &store.Skill{Name: "github", Description: "mine", Body: "my own text"}
		if err := store.CreateSkill(ctx, tx, mine); err != nil {
			t.Skipf("a committed skill named github exists in the test database: %v", err)
		}
		res, err := ImportLibrary(ctx, tx, realLibrary, quiet)
		if err != nil || res.Imported != 16 || res.Existing != 1 || res.Failed != 0 {
			t.Fatalf("first run = %+v, %v", res, err)
		}
		again, err := ImportLibrary(ctx, tx, realLibrary, quiet)
		if err != nil || again.Imported != 0 || again.Existing != 17 || again.Failed != 0 {
			t.Fatalf("second run = %+v, %v", again, err)
		}
		got, err := store.GetSkill(ctx, tx, mine.ID)
		if err != nil || got.Body != "my own text" || got.Source != store.SkillSourceUser || got.FilesCount != 0 {
			t.Fatalf("existing skill touched: %+v %v", got, err)
		}

		// A fresh transaction inside: the whole library into an empty slate (the github
		// skill is removed first).
		if err := store.DeleteSkill(ctx, tx, mine.ID); err != nil {
			t.Fatal(err)
		}
		res, err = ImportLibrary(ctx, tx, realLibrary, quiet)
		if err != nil || res.Imported != 1 {
			t.Fatalf("github import = %+v, %v", res, err)
		}
		all, err := store.ListSkills(ctx, tx)
		if err != nil {
			t.Fatal(err)
		}
		bySource := map[string]int{}
		byName := map[string]store.Skill{}
		for _, s := range all {
			byName[s.Name] = s
			if s.Source == store.SkillSourceLibrary {
				bySource[strings.TrimPrefix(strings.SplitN(s.SourceURL, "/", 5)[2], "www.")]++
			}
		}
		if bySource["github.com"] != 15 || bySource["clawhub.ai"] != 2 {
			t.Fatalf("by host = %v", bySource)
		}
		check := func(name, license, urlPrefix string, files ...string) {
			t.Helper()
			s, ok := byName[name]
			if !ok || s.Source != "library" || s.License != license || !strings.HasPrefix(s.SourceURL, urlPrefix) || !strings.HasSuffix(s.SourceURL, "/"+name) || s.Body == "" || s.Description == "" {
				t.Fatalf("%s = %+v", name, s)
			}
			fl, err := store.ListSkillFiles(ctx, tx, s.ID)
			if err != nil {
				t.Fatal(err)
			}
			have := map[string]bool{}
			for _, f := range fl {
				have[f.Path] = true
			}
			for _, f := range files {
				if !have[f] {
					t.Errorf("%s lacks %s (has %d files)", name, f, len(fl))
				}
			}
			if len(fl) != s.FilesCount {
				t.Errorf("%s: files_count %d, files %d", name, s.FilesCount, len(fl))
			}
		}
		check("skill-creator", "Apache-2.0", "https://github.com/anthropics/skills/tree/main/skills/", "LICENSE.txt")
		check("canvas-design", "Apache-2.0", "https://github.com/anthropics/skills/tree/main/skills/", "LICENSE.txt")
		check("github", "MIT", "https://github.com/openclaw/openclaw/tree/main/skills/", "LICENSE")
		check("debug-pro", "MIT-0", "https://clawhub.ai/cmanfre7/skills/", "LICENSE")
		// No front matter: the folder gives the name and the first paragraph the description.
		if d := byName["debug-pro"].Description; !strings.HasPrefix(d, "Systematic debugging methodology") {
			t.Errorf("debug-pro description = %q", d)
		}
		// Scripts keep their executable bit.
		sc, _ := store.ListSkillFileMeta(ctx, tx, byName["skill-creator"].ID)
		exec := 0
		for _, f := range sc {
			if f.Mode == 0o755 {
				exec++
			}
		}
		t.Logf("skill-creator: %d files, %d executable; library total %d skills", len(sc), exec, len(all))
	})
}

func TestImportLibraryMissingDir(t *testing.T) {
	inTx(t, func(ctx context.Context, tx pgx.Tx) {
		res, err := ImportLibrary(ctx, tx, filepath.Join(t.TempDir(), "nope"), quiet)
		if err != nil || res != (LibraryResult{}) {
			t.Fatalf("%+v %v", res, err)
		}
	})
}

// A skill that cannot be read is counted and skipped; the others still import.
func TestImportLibraryBadSkill(t *testing.T) {
	dir := t.TempDir()
	mk := func(rel, data string) {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("anthropic/good-one/SKILL.md", "---\nname: good-one\ndescription: ok\n---\nbody")
	mk("anthropic/no-skill-md/readme.md", "x")
	mk("anthropic/bad-yaml/SKILL.md", "---\nname: [oops\n---\nbody")
	mk("unknown-source/other/SKILL.md", "body")
	inTx(t, func(ctx context.Context, tx pgx.Tx) {
		res, err := ImportLibrary(ctx, tx, dir, quiet)
		if err != nil || res.Imported != 1 || res.Failed != 2 {
			t.Fatalf("%+v %v", res, err)
		}
	})
}

// Finding 7: a library skill is matched on its front-matter name. An unrelated skill that
// has the folder's name does not hide it; one with the front-matter name does.
func TestImportLibraryMatchesFrontMatterName(t *testing.T) {
	dir := t.TempDir()
	mk := func(rel, data string) {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("anthropic/folder-a/SKILL.md", "---\nname: real-a\ndescription: A\n---\nbody a")
	mk("anthropic/folder-b/SKILL.md", "---\nname: real-b\ndescription: B\n---\nbody b")
	inTx(t, func(ctx context.Context, tx pgx.Tx) {
		// Unrelated skills: one named like the folder, one named like the other's real name.
		for _, n := range []string{"folder-a", "real-b"} {
			if err := store.CreateSkill(ctx, tx, &store.Skill{Name: n, Description: "mine", Body: "own"}); err != nil {
				t.Skipf("a committed skill named %s exists: %v", n, err)
			}
		}
		res, err := ImportLibrary(ctx, tx, dir, quiet)
		if err != nil || res.Imported != 1 || res.Existing != 1 || res.Failed != 0 {
			t.Fatalf("%+v %v", res, err)
		}
		got, _ := store.ListSkillsByName(ctx, tx, []string{"real-a", "real-b"})
		if len(got) != 2 {
			t.Fatalf("skills = %+v", got)
		}
		for _, s := range got {
			switch s.Name {
			case "real-a":
				if s.Source != store.SkillSourceLibrary {
					t.Fatalf("real-a = %+v", s)
				}
			case "real-b":
				if s.Source != store.SkillSourceUser || s.Body != "own" {
					t.Fatalf("real-b was touched: %+v", s)
				}
			}
		}
	})
}

// Finding 8: the LICENSE added from the source counts against the caps.
func TestImportLibraryLicenseCountsAgainstCaps(t *testing.T) {
	dir := t.TempDir()
	mk := func(rel string, data []byte) {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("openclaw/lic-skill/SKILL.md", []byte("---\nname: lic-skill\ndescription: d\n---\nbody"))
	mk("openclaw/lic-skill/a.txt", []byte("a"))
	mk("openclaw/LICENSE", []byte(strings.Repeat("l", MaxFileBytes+10)))
	if _, err := ReadDirWith(filepath.Join(dir, "openclaw", "lic-skill"), "lic-skill", true, ExtraFile{Path: "LICENSE", Content: readLicense(filepath.Join(dir, "openclaw", "LICENSE")), Mode: 0o644}); !isCap(err) {
		t.Fatalf("an oversized LICENSE: %v", err)
	}
	inTx(t, func(ctx context.Context, tx pgx.Tx) {
		res, err := ImportLibrary(ctx, tx, dir, quiet)
		if err != nil || res.Failed != 1 || res.Imported != 0 {
			t.Fatalf("%+v %v", res, err)
		}
	})
	// A normal LICENSE is added once, and the skill's own LICENSE wins.
	mk("openclaw/LICENSE", []byte("MIT text"))
	b, err := ReadDirWith(filepath.Join(dir, "openclaw", "lic-skill"), "lic-skill", true, ExtraFile{Path: "LICENSE", Content: []byte("MIT text"), Mode: 0o644})
	if err != nil || len(b.Files) != 2 || b.Files[0].Path != "LICENSE" {
		t.Fatalf("%+v %v", b, err)
	}
	mk("openclaw/lic-skill/LICENSE", []byte("own"))
	b, err = ReadDirWith(filepath.Join(dir, "openclaw", "lic-skill"), "lic-skill", true, ExtraFile{Path: "LICENSE", Content: []byte("MIT text"), Mode: 0o644})
	if err != nil || len(b.Files) != 2 || string(b.Files[0].Content) != "own" {
		t.Fatalf("%+v %v", b, err)
	}
}
