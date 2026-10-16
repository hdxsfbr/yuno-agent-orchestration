package config

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The checks here hold the deployment files (compose.yaml, docker/api.Dockerfile,
// Makefile) to the fixes of the wave-4 audit; they read the files, nothing runs.

type composeService struct {
	Image string `yaml:"image"`
	Build *struct {
		Args map[string]string `yaml:"args"`
	} `yaml:"build"`
	Ports []string `yaml:"ports"`
}

func composeServices(t *testing.T) map[string]composeService {
	t.Helper()
	raw, err := os.ReadFile("../../compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]composeService `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Services
}

// makeRule is one Makefile rule: its prerequisites and recipe lines.
type makeRule struct {
	prereqs []string
	recipe  []string
}

func makeRules(t *testing.T) map[string]makeRule {
	t.Helper()
	raw, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	rules := map[string]makeRule{}
	var cur string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "\t") {
			if cur != "" {
				r := rules[cur]
				r.recipe = append(r.recipe, strings.TrimSpace(line))
				rules[cur] = r
			}
			continue
		}
		cur = ""
		head, _, _ := strings.Cut(line, "##")
		target, deps, ok := strings.Cut(head, ":")
		if !ok || strings.ContainsAny(target, " \t$#=") || strings.HasPrefix(strings.TrimSpace(deps), "=") ||
			strings.Contains(deps, "export") {
			continue
		}
		cur = strings.TrimSpace(target)
		rules[cur] = makeRule{prereqs: strings.Fields(deps)}
	}
	return rules
}

func indexOf(lines []string, sub string) int {
	return slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, sub) })
}

// Postgres is published on the loopback interface only (audit #29).
func TestComposePostgresOnLoopback(t *testing.T) {
	pg := composeServices(t)["postgres"]
	if len(pg.Ports) == 0 {
		t.Fatal("postgres publishes no port; make test and the host's psql need one")
	}
	for _, p := range pg.Ports {
		if !strings.HasPrefix(p, "127.0.0.1:") {
			t.Errorf("postgres port %q is published on every interface", p)
		}
	}
}

// Every image compose pulls, rather than builds, names a fixed tag (audit #34), and the
// static checker runs at a fixed version.
func TestToolsArePinned(t *testing.T) {
	for name, s := range composeServices(t) {
		if s.Build != nil || s.Image == "" {
			continue
		}
		ref := s.Image[strings.LastIndex(s.Image, "/")+1:]
		_, tag, ok := strings.Cut(ref, ":")
		if !ok || tag == "" || tag == "latest" {
			t.Errorf("service %s pulls %s without a fixed tag", name, s.Image)
		}
	}
	lint := makeRules(t)["lint"]
	i := indexOf(lint.recipe, "staticcheck@")
	if i < 0 {
		t.Fatal("make lint does not run staticcheck")
	}
	if strings.Contains(lint.recipe[i], "staticcheck@latest") {
		t.Errorf("make lint runs staticcheck@latest: %s", lint.recipe[i])
	}
}

// Images carry the commit they were built from (demo pre-flight): the Dockerfile stamps
// VERSION into config.Version, compose passes it to every image built from it that runs
// the api's binary, and make run and make build set it from git.
func TestVersionFromCommit(t *testing.T) {
	df, err := os.ReadFile("../../docker/api.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ARG VERSION=dev", "-X yuno/internal/config.Version=${VERSION}"} {
		if !strings.Contains(string(df), want) {
			t.Errorf("docker/api.Dockerfile lacks %q", want)
		}
	}
	services := composeServices(t)
	for _, name := range []string{"api", "deploytarget", "eval", "openclaw-init"} {
		s := services[name]
		if s.Build == nil || s.Build.Args["VERSION"] != "${VERSION:-dev}" {
			t.Errorf("compose service %s does not pass VERSION as a build arg", name)
		}
	}
	mk, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mk), "run build: export VERSION ?= $(shell git rev-parse --short HEAD") {
		t.Error("make run and make build do not export VERSION from git")
	}
}

// From a fresh checkout: make run seeds once the stack is ready, the test and eval
// targets create .env first, and make eval says to start the stack when it is down
// (audit #12).
func TestMakeTargetsFromFreshCheckout(t *testing.T) {
	rules := makeRules(t)
	for _, target := range []string{"test", "test-go", "test-web", "eval"} {
		r, ok := rules[target]
		if !ok || !slices.Contains(r.prereqs, "env") {
			t.Errorf("make %s does not depend on env (prerequisites %v)", target, r.prereqs)
		}
	}
	run := rules["run"].recipe
	ready, seed := indexOf(run, "/readyz"), indexOf(run, "$(COMPOSE) run --rm api seed")
	if ready < 0 || seed < ready {
		t.Errorf("make run does not seed after the readiness wait:\n%s", strings.Join(run, "\n"))
	}
	eval := rules["eval"].recipe
	check, build := indexOf(eval, "/readyz"), indexOf(eval, "build eval")
	if check < 0 || check > build || !strings.Contains(eval[check], "run `make run` first") {
		t.Errorf("make eval does not check the stack first:\n%s", strings.Join(eval, "\n"))
	}
}
