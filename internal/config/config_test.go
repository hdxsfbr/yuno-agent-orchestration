package config

import (
	"bufio"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// envRead matches every setting FromEnv reads and the helper that parses it.
var envRead = regexp.MustCompile(`\benv(Int|Float|Duration|List|Int64List|Bool)?\("([A-Z][A-Z0-9_]*)"`)

// readNames returns the names config.go reads, each with its parser ("" is a string).
func readNames(t *testing.T) map[string]string {
	t.Helper()
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, m := range envRead.FindAllStringSubmatch(string(src), -1) {
		names[m[2]] = m[1]
	}
	if len(names) < 40 {
		t.Fatalf("found %d settings in config.go; the pattern no longer matches how it reads them", len(names))
	}
	return names
}

type exampleLine struct {
	value     string
	commented bool // a line that documents the default rather than setting a value
}

var exampleRe = regexp.MustCompile(`^(#?)([A-Z][A-Z0-9_]*)=(.*)$`)

func readExample(t *testing.T) map[string]exampleLine {
	t.Helper()
	f, err := os.Open("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]exampleLine{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := exampleRe.FindStringSubmatch(strings.TrimSpace(sc.Text())); m != nil {
			out[m[2]] = exampleLine{value: m[3], commented: m[1] == "#"}
		}
	}
	return out
}

// readCompose returns the x-api-env block of compose.yaml and the whole file.
func readCompose(t *testing.T) (map[string]string, string) {
	t.Helper()
	raw, err := os.ReadFile("../../compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		APIEnv map[string]any `yaml:"x-api-env"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for k, v := range doc.APIEnv {
		out[k] = fmt.Sprint(v)
	}
	return out, string(raw)
}

// composeDefault is the default of a "${NAME:-default}" value; ok is false for any
// other form (a literal the topology fixes, or a required "${NAME:?...}").
func composeDefault(name, v string) (string, bool) {
	prefix := "${" + name + ":-"
	if !strings.HasPrefix(v, prefix) || !strings.HasSuffix(v, "}") {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(v, prefix), "}"), true
}

// parses reports whether v is a valid value for the parser FromEnv uses: an invalid one
// silently falls back to the default, which is how "300*time.Second" went unnoticed.
func parses(kind, v string) bool {
	var err error
	switch kind {
	case "Int":
		_, err = strconv.Atoi(v)
	case "Float":
		_, err = strconv.ParseFloat(v, 64)
	case "Duration":
		_, err = time.ParseDuration(v)
	case "Bool":
		_, err = strconv.ParseBool(v)
	}
	return err == nil
}

// Every setting the config reads is documented in .env.example and passed to the api by
// compose.yaml's x-api-env; documented defaults and compose defaults are valid values
// equal to config.go's; and .env.example names nothing nobody reads (audit #15).
func TestEnvExampleAndComposeCoverConfig(t *testing.T) {
	names := readNames(t)
	example := readExample(t)
	apiEnv, composeFile := readCompose(t)

	// Defaults as config.go has them: every setting unset (env treats empty as unset).
	for name := range names {
		t.Setenv(name, "")
	}
	defaults := FromEnv()
	sameAsDefault := func(name, v string) bool {
		t.Setenv(name, v)
		defer t.Setenv(name, "")
		return reflect.DeepEqual(FromEnv(), defaults)
	}

	for name, kind := range names {
		line, ok := example[name]
		switch {
		case !ok:
			t.Errorf(".env.example does not mention %s", name)
		case line.value != "" && !parses(kind, line.value):
			t.Errorf(".env.example: %s=%s is not a valid value", name, line.value)
		case line.commented && line.value != "" && !sameAsDefault(name, line.value):
			t.Errorf(".env.example documents %s=%s, which is not config.go's default", name, line.value)
		}

		v, ok := apiEnv[name]
		if !ok {
			t.Errorf("compose.yaml x-api-env does not pass %s to the api", name)
			continue
		}
		if def, ok := composeDefault(name, v); ok && def != "" {
			if !parses(kind, def) {
				t.Errorf("compose.yaml: %s defaults to %s, not a valid value", name, def)
			} else if !sameAsDefault(name, def) {
				t.Errorf("compose.yaml: %s defaults to %s, which is not config.go's default", name, def)
			}
		}
	}
	// Variables docker compose itself reads from .env, without ${...} interpolation.
	composeNative := map[string]bool{"COMPOSE_PROFILES": true, "COMPOSE_PROJECT_NAME": true}
	for name := range example {
		if _, read := names[name]; !read && !composeNative[name] && !strings.Contains(composeFile, "${"+name) {
			t.Errorf(".env.example names %s, which neither the config nor compose.yaml reads", name)
		}
	}
}
