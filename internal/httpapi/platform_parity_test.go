package httpapi

import (
	"reflect"
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
	if checked < 80 {
		t.Fatalf("%d routes checked", checked)
	}
	// The mounts the rule leaves out are registered, so the gate can refuse them by name.
	for _, p := range []string{"/mcp", anthropicProxyPrefix + "/", scriptedProxyPrefix + "/"} {
		if !slices.ContainsFunc(s.routeTable, func(rt registeredRoute) bool { return rt.Pattern == p }) {
			t.Errorf("%s is not in the route table", p)
		}
	}
}
