package config

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The sandbox images are held to the tool-version policy of DESIGN 5.16 rule 6: a tool an
// image installs is pinned to an exact version, and a base image names its release line,
// so a new release of a tool never reaches a run on its own. The project's own tools come
// from the project's lockfile, not from the image. The checks read the Dockerfiles;
// nothing is built.

var (
	pinnedPip   = regexp.MustCompile(`^[A-Za-z0-9_.-]+==\d+(\.\d+)+$`)
	pinnedNpm   = regexp.MustCompile(`^(@[a-z0-9._-]+/)?[a-z0-9._-]+@\d+\.\d+\.\d+$`)
	pinnedGo    = regexp.MustCompile(`^[A-Za-z0-9_./-]+@v?\d+(\.\d+)+$`)
	pinnedImage = regexp.MustCompile(`^[a-z0-9./_-]+:\d+(\.\d+)*[A-Za-z0-9._-]*$`)
)

// dockerInstalls reads a Dockerfile and returns its base images and, per package manager,
// the packages its RUN lines install. Comments are dropped and a continued line is joined.
func dockerInstalls(t *testing.T, path string) (from []string, installs map[string][]string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	installs = map[string][]string{}
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(string(raw), "\\\n", " "), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	install := regexp.MustCompile(`(?:^|\s)(pip|npm|go) install\s+(.*)$`)
	for _, l := range lines {
		if rest, ok := strings.CutPrefix(l, "FROM "); ok {
			from = append(from, strings.Fields(rest)[0])
			continue
		}
		if !strings.HasPrefix(l, "RUN ") {
			continue
		}
		for _, cmd := range regexp.MustCompile(`&&|;`).Split(l, -1) {
			m := install.FindStringSubmatch(cmd)
			if m == nil {
				continue
			}
			for _, arg := range strings.Fields(m[2]) {
				if !strings.HasPrefix(arg, "-") {
					installs[m[1]] = append(installs[m[1]], arg)
				}
			}
		}
	}
	return from, installs
}

func TestSandboxImagesPinWhatTheyInstall(t *testing.T) {
	// What each image installs today: the test fails when an image gains an install that
	// this list does not name, so a new tool is pinned (and listed) before it ships.
	want := map[string]map[string][]string{
		"sandbox.Dockerfile":        {"pip": {"pytest"}},
		"sandbox-python.Dockerfile": {"pip": {"pytest", "uv"}},
		"sandbox-node.Dockerfile":   {"npm": {"pnpm"}},
		"sandbox-go.Dockerfile":     {},
	}
	files, err := filepath.Glob("../../docker/sandbox*.Dockerfile")
	if err != nil || len(files) != len(want) {
		t.Fatalf("sandbox Dockerfiles = %v (%v), want %d", files, err, len(want))
	}
	for _, f := range files {
		name := filepath.Base(f)
		from, installs := dockerInstalls(t, f)
		if len(from) == 0 {
			t.Errorf("%s: no FROM line", name)
		}
		for _, img := range from {
			if !pinnedImage.MatchString(img) {
				t.Errorf("%s: base image %q does not name a release line", name, img)
			}
		}
		for manager, pkgs := range installs {
			re := map[string]*regexp.Regexp{"pip": pinnedPip, "npm": pinnedNpm, "go": pinnedGo}[manager]
			var names []string
			for _, p := range pkgs {
				if !re.MatchString(p) {
					t.Errorf("%s: %s installs %q without an exact version", name, manager, p)
				}
				names = append(names, strings.SplitN(strings.SplitN(p, "==", 2)[0], "@", 2)[0])
			}
			slices.Sort(names)
			if !slices.Equal(names, want[name][manager]) {
				t.Errorf("%s: %s installs %v, the list of this test has %v", name, manager, names, want[name][manager])
			}
		}
		for manager, names := range want[name] {
			if len(installs[manager]) == 0 {
				t.Errorf("%s: no %s install found, want %v", name, manager, names)
			}
		}
	}
}

// The pin patterns accept an exact version and refuse a range, a bare name and latest, so
// the check above cannot pass an unpinned install.
func TestPinPatterns(t *testing.T) {
	for _, tc := range []struct {
		re   *regexp.Regexp
		in   string
		want bool
	}{
		{pinnedPip, "pytest==9.1.1", true},
		{pinnedPip, "pytest>=9", false},
		{pinnedPip, "pytest", false},
		{pinnedNpm, "pnpm@10.34.7", true},
		{pinnedNpm, "@scope/tool@1.2.3", true},
		{pinnedNpm, "pnpm@latest", false},
		{pinnedNpm, "pnpm@10", false},
		{pinnedNpm, "pnpm", false},
		{pinnedGo, "honnef.co/go/tools/cmd/staticcheck@2026.2.2", true},
		{pinnedGo, "example.com/tool@latest", false},
		{pinnedImage, "python:3.12-slim", true},
		{pinnedImage, "node:24-slim", true},
		{pinnedImage, "golang:1.26-bookworm", true},
		{pinnedImage, "python:latest", false},
		{pinnedImage, "python", false},
	} {
		if got := tc.re.MatchString(tc.in); got != tc.want {
			t.Errorf("%s on %q = %v, want %v", tc.re, tc.in, got, tc.want)
		}
	}
}

// The reader finds installs across continued lines and joined commands, so a loose one in
// a real image is seen by the check above.
func TestDockerInstallsReadsContinuedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.Dockerfile")
	body := "# pip install ignored==1.0\nFROM python:3.12-slim AS base\nRUN apt-get install -y curl \\\n" +
		"    && pip install --no-cache-dir pytest>=9 uv==0.13.1 \\\n    && npm install -g --no-fund pnpm@latest && pnpm install\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	from, installs := dockerInstalls(t, path)
	if !slices.Equal(from, []string{"python:3.12-slim"}) {
		t.Errorf("from = %v", from)
	}
	if !slices.Equal(installs["pip"], []string{"pytest>=9", "uv==0.13.1"}) || !slices.Equal(installs["npm"], []string{"pnpm@latest"}) || len(installs["go"]) != 0 {
		t.Errorf("installs = %v", installs)
	}
}
