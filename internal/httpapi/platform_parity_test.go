package httpapi

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"yuno/internal/tools"
)

// handlerName is the Go name of a route's handler: "listAgents" for s.listAgents.
func handlerName(h any) string {
	full := runtime.FuncForPC(reflect.ValueOf(h).Pointer()).Name() // yuno/internal/httpapi.(*Server).listAgents-fm
	return strings.TrimSuffix(full[strings.LastIndexByte(full, '.')+1:], "-fm")
}

// isMachineMount is a route rule A1 leaves undocumented: health, readiness, the MCP server
// and the model proxy.
func isMachineMount(pattern string) bool {
	return pattern == "GET /healthz" || pattern == "GET /readyz" || pattern == "/mcp" || strings.HasPrefix(pattern, "/model-proxy/")
}

// Platform control A1: every route registered in server.go but the machine mounts has
// exactly one documented operation, whose operationId is the route's handler and whose
// x-yuno-risk is a valid class; no documented operation lacks a route.
func TestPlatformParity(t *testing.T) {
	s, err := New(Deps{Cfg: testConfig()})
	if err != nil {
		t.Fatal(err)
	}
	c, err := tools.Contract()
	if err != nil {
		t.Fatal(err)
	}
	routed := map[string]string{} // operationId -> pattern
	checked := 0
	for _, rt := range s.routeTable {
		op, ok := c.ForPattern(rt.Pattern)
		if !ok {
			if !isMachineMount(rt.Pattern) {
				t.Errorf("%s: no documented operation", rt.Pattern)
			}
			continue
		}
		if prev, dup := routed[op.ID]; dup {
			t.Errorf("%s and %s share the operation %s", prev, rt.Pattern, op.ID)
		}
		routed[op.ID] = rt.Pattern
		if rt.Handler == nil {
			t.Errorf("%s: documented, but mounted without a handler of its own", rt.Pattern)
			continue
		}
		if name := handlerName(rt.Handler); op.ID != name {
			t.Errorf("%s: operationId %s, handler %s", rt.Pattern, op.ID, name)
		}
		if !slices.Contains(tools.PlatformRisks, op.Risk) {
			t.Errorf("%s: x-yuno-risk %q", rt.Pattern, op.Risk)
		}
		checked++
	}
	for _, op := range c.Operations() {
		if _, ok := routed[op.ID]; !ok {
			t.Errorf("%s %s (%s) is documented but has no route", op.Method, op.Path, op.ID)
		}
	}
	if checked < 96 {
		t.Fatalf("%d routes checked", checked)
	}
	// The routes waves 1, 3 and 4 added are among them (DESIGN W4 rule 8).
	for _, id := range []string{"listRunEventsPage", "getRunDiagnosis", "listRunDiagnoses", "listAudit", "revertAudit",
		"listWorkflowVersions", "getWorkflowVersion", "restoreWorkflowVersion", "restoreAgent", "getMergeRequestDiff",
		"getMergeRequestChecks", "getMergeRequestJobLog", "updateMergeRequestFromDefault"} {
		if _, ok := routed[id]; !ok {
			t.Errorf("%s has no route", id)
		}
	}
	// The mounts the rule leaves out are registered, so the gate can refuse them by name.
	for _, p := range []string{"/mcp", anthropicProxyPrefix + "/", scriptedProxyPrefix + "/"} {
		if !slices.ContainsFunc(s.routeTable, func(rt registeredRoute) bool { return rt.Pattern == p }) {
			t.Errorf("%s is not in the route table", p)
		}
	}
}

// parityLaterWave marks an acceptance test of PARITY.md that a later wave builds.
var parityLaterWave = []string{"(wave 2)", "(wave 3 gate)"}

// DESIGN W4 rule 8: PARITY.md has a row for every operation of the contract, naming its
// handler in the Backend column, and every acceptance test it names is a test of the
// repository; a test marked for a later wave must not exist yet, so the mark goes when it
// lands.
func TestParityMatrix(t *testing.T) {
	root := filepath.Join("..", "..")
	doc, err := os.ReadFile(filepath.Join(root, "docs", "briefs", "chief-platform-control", "PARITY.md"))
	if err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	testFunc := regexp.MustCompile(`(?m)^func (Test\w+)\(`)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range testFunc.FindAllSubmatch(src, -1) {
			defined[string(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	testName := regexp.MustCompile(`\bTest[A-Z]\w*`)
	word := regexp.MustCompile(`[A-Za-z]\w*`)
	backends := map[string]bool{}
	named := 0
	for _, line := range strings.Split(string(doc), "\n") {
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if !strings.HasPrefix(line, "|") || len(cells) != 7 || strings.TrimSpace(cells[0]) == "#" || strings.HasPrefix(strings.TrimSpace(cells[0]), "---") {
			continue
		}
		for _, w := range word.FindAllString(cells[2], -1) {
			backends[w] = true
		}
		// The acceptance cell lists tests separated by commas or semicolons; a mark applies to
		// the names of its own item.
		for _, item := range strings.FieldsFunc(cells[6], func(r rune) bool { return r == ',' || r == ';' }) {
			later := slices.ContainsFunc(parityLaterWave, func(m string) bool { return strings.Contains(item, m) })
			for _, name := range testName.FindAllString(item, -1) {
				named++
				switch {
				case later && defined[name]:
					t.Errorf("PARITY.md marks %s for a later wave, and it exists: drop the mark", name)
				case !later && !defined[name]:
					t.Errorf("PARITY.md names %s, and no func %s is in the repository", name, name)
				}
			}
		}
	}
	if named < 100 {
		t.Fatalf("%d acceptance tests read from PARITY.md", named)
	}
	c, err := tools.Contract()
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range c.Operations() {
		if !backends[op.ID] {
			t.Errorf("PARITY.md has no row whose Backend names %s (%s %s)", op.ID, op.Method, op.Path)
		}
	}
}
