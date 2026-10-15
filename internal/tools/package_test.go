package tools

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Review finding 1 (wave 2): links the agent planted must not let deploy_release or
// gitlab_create_mr package files from outside the workspace. A linked run directory is
// refused, a named link is refused, a link inside a named directory is skipped.
func TestPackageRunFilesRefusesLinks(t *testing.T) {
	const run = "run-1"
	tests := []struct {
		name  string
		setup func(t *testing.T, ws, runDir, outside string)
		paths []string
		want  string   // ModelError text; empty when packaging succeeds
		files []string // packaged paths on success
	}{
		{name: "run directory linked outside the workspace", paths: []string{"a.txt"}, want: "is a link",
			setup: func(t *testing.T, ws, runDir, outside string) {
				write(t, filepath.Join(outside, "a.txt"), "outside")
				must(t, os.MkdirAll(filepath.Join(ws, "runs"), 0o755))
				must(t, os.Symlink(outside, runDir))
			}},
		{name: "runs linked outside the workspace", paths: []string{"a.txt"}, want: "is a link",
			setup: func(t *testing.T, ws, runDir, outside string) {
				write(t, filepath.Join(outside, run, "a.txt"), "outside")
				must(t, os.Symlink(outside, filepath.Join(ws, "runs")))
			}},
		{name: "run directory linked inside the workspace", paths: []string{"a.txt"}, want: "is a link",
			setup: func(t *testing.T, ws, runDir, outside string) {
				write(t, filepath.Join(ws, "other", "a.txt"), "elsewhere")
				must(t, os.MkdirAll(filepath.Join(ws, "runs"), 0o755))
				must(t, os.Symlink(filepath.Join(ws, "other"), runDir))
			}},
		{name: "named link out of the workspace", paths: []string{"escape.txt"}, want: "links are not packaged",
			setup: func(t *testing.T, ws, runDir, outside string) {
				must(t, os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(runDir, "escape.txt")))
			}},
		{name: "named link inside the run directory", paths: []string{"alias.txt"}, want: "links are not packaged",
			setup: func(t *testing.T, ws, runDir, outside string) {
				must(t, os.Symlink("ok.txt", filepath.Join(runDir, "alias.txt")))
			}},
		{name: "linked directory on the way", paths: []string{"sub/secret.txt"}, want: "links are not packaged",
			setup: func(t *testing.T, ws, runDir, outside string) {
				must(t, os.Symlink(outside, filepath.Join(runDir, "sub")))
			}},
		{name: "links inside a named directory are skipped", paths: []string{"site"}, files: []string{"site/index.html"},
			setup: func(t *testing.T, ws, runDir, outside string) {
				write(t, filepath.Join(runDir, "site", "index.html"), "<h1>hi</h1>")
				must(t, os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(runDir, "site", "leak.txt")))
				must(t, os.Symlink("index.html", filepath.Join(runDir, "site", "alias.html")))
				must(t, os.Symlink(outside, filepath.Join(runDir, "site", "dir")))
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, outside := t.TempDir(), t.TempDir()
			runDir := filepath.Join(ws, "runs", run)
			write(t, filepath.Join(outside, "secret.txt"), "outside")
			if !strings.HasPrefix(tc.name, "run") {
				write(t, filepath.Join(runDir, "ok.txt"), "ok")
			}
			tc.setup(t, ws, runDir, outside)
			got, err := packageRunFiles(ws, run, tc.paths, 1<<20)
			for _, f := range got {
				if strings.Contains(string(f.Content), "outside") || strings.Contains(string(f.Content), "elsewhere") {
					t.Fatalf("packaged %s from outside the run directory", f.Path)
				}
			}
			if tc.want != "" {
				var me *ModelError
				if !errors.As(err, &me) || !strings.Contains(me.Message, tc.want) {
					t.Fatalf("err = %v; want a model error containing %q", err, tc.want)
				}
				return
			}
			var paths []string
			for _, f := range got {
				paths = append(paths, f.Path)
			}
			if err != nil || !slices.Equal(paths, tc.files) {
				t.Fatalf("packaged %v, %v; want %v", paths, err, tc.files)
			}
		})
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte(content), 0o644))
}
