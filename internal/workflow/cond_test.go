package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

const condState = `{
	"reviewer": {"verdict": "approved", "score": 7, "tags": ["a", "b"], "note": null, "ok": true},
	"coder": {"files": ["app.py"], "a/b": 1, "m~n": 2}
}`

func cond(t *testing.T, s string) *Condition {
	t.Helper()
	var c Condition
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// T5.6.2: every operator, including a missing path, which is exists:false and makes
// every other operator false.
func TestEvaluateOperators(t *testing.T) {
	tests := []struct {
		name, cond string
		want       bool
	}{
		{"eq string", `{"path":"/reviewer/verdict","op":"eq","value":"approved"}`, true},
		{"eq string other", `{"path":"/reviewer/verdict","op":"eq","value":"changes_requested"}`, false},
		{"eq number int vs float", `{"path":"/reviewer/score","op":"eq","value":7.0}`, true},
		{"eq bool", `{"path":"/reviewer/ok","op":"eq","value":true}`, true},
		{"eq null", `{"path":"/reviewer/note","op":"eq","value":null}`, true},
		{"eq array", `{"path":"/reviewer/tags","op":"eq","value":["a","b"]}`, true},
		{"eq object", `{"path":"/coder","op":"eq","value":{"files":["app.py"],"a/b":1,"m~n":2}}`, true},
		{"eq type mismatch", `{"path":"/reviewer/score","op":"eq","value":"7"}`, false},
		{"neq", `{"path":"/reviewer/verdict","op":"neq","value":"changes_requested"}`, true},
		{"neq equal", `{"path":"/reviewer/verdict","op":"neq","value":"approved"}`, false},
		{"in", `{"path":"/reviewer/verdict","op":"in","value":["rejected","approved"]}`, true},
		{"in absent", `{"path":"/reviewer/verdict","op":"in","value":["rejected"]}`, false},
		{"in not an array", `{"path":"/reviewer/verdict","op":"in","value":"approved"}`, false},
		{"exists", `{"path":"/reviewer/verdict","op":"exists","value":true}`, true},
		{"exists without value", `{"path":"/reviewer/verdict","op":"exists"}`, true},
		{"exists on null value", `{"path":"/reviewer/note","op":"exists","value":true}`, true},
		{"exists false on present", `{"path":"/reviewer/verdict","op":"exists","value":false}`, false},
		{"gt", `{"path":"/reviewer/score","op":"gt","value":6}`, true},
		{"gt equal", `{"path":"/reviewer/score","op":"gt","value":7}`, false},
		{"lt", `{"path":"/reviewer/score","op":"lt","value":7.5}`, true},
		{"lt equal", `{"path":"/reviewer/score","op":"lt","value":7}`, false},
		{"gt on a string", `{"path":"/reviewer/verdict","op":"gt","value":1}`, false},
		{"array index", `{"path":"/coder/files/0","op":"eq","value":"app.py"}`, true},
		{"array index out of range", `{"path":"/coder/files/1","op":"exists","value":false}`, true},
		{"escaped slash", `{"path":"/coder/a~1b","op":"eq","value":1}`, true},
		{"escaped tilde", `{"path":"/coder/m~0n","op":"eq","value":2}`, true},
		{"unknown op", `{"path":"/reviewer/verdict","op":"like","value":"approved"}`, false},
		{"pointer without slash", `{"path":"reviewer/verdict","op":"exists"}`, false},
	}
	for _, op := range []string{"eq", "neq", "in", "gt", "lt"} {
		value := `"x"`
		switch op {
		case "in":
			value = `["x", null]`
		case "gt", "lt":
			value = `0`
		}
		tests = append(tests, struct {
			name, cond string
			want       bool
		}{"missing path " + op, `{"path":"/critic/verdict","op":"` + op + `","value":` + value + `}`, false})
	}
	tests = append(tests,
		struct {
			name, cond string
			want       bool
		}{"missing path exists true", `{"path":"/critic/verdict","op":"exists","value":true}`, false},
		struct {
			name, cond string
			want       bool
		}{"missing path exists false", `{"path":"/critic/verdict","op":"exists","value":false}`, true},
	)
	var state any
	_ = json.Unmarshal([]byte(condState), &state)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Evaluate(cond(t, tc.cond), state); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	if !Evaluate(nil, state) {
		t.Fatal("a nil condition (an edge without when) must be true")
	}
	if EvaluateJSON(cond(t, `{"path":"/reviewer/verdict","op":"eq","value":"approved"}`), json.RawMessage(condState)) != true {
		t.Fatal("EvaluateJSON")
	}
}

// T5.6.3: all/any, nested.
func TestEvaluateAllAny(t *testing.T) {
	var state any
	_ = json.Unmarshal([]byte(condState), &state)
	yes := `{"path":"/reviewer/verdict","op":"eq","value":"approved"}`
	no := `{"path":"/reviewer/verdict","op":"eq","value":"nope"}`
	missing := `{"path":"/nope","op":"exists"}`
	tests := []struct {
		name, cond string
		want       bool
	}{
		{"all true", `{"all":[` + yes + `,` + yes + `]}`, true},
		{"all one false", `{"all":[` + yes + `,` + no + `]}`, false},
		{"all empty", `{"all":[]}`, false},
		{"any one true", `{"any":[` + no + `,` + yes + `]}`, true},
		{"any none", `{"any":[` + no + `,` + missing + `]}`, false},
		{"any empty", `{"any":[]}`, false},
		{"nested any in all", `{"all":[` + yes + `,{"any":[` + no + `,` + yes + `]}]}`, true},
		{"nested all in any", `{"any":[` + no + `,{"all":[` + yes + `,` + missing + `]}]}`, false},
		{"three levels", `{"all":[{"any":[{"all":[` + yes + `]}]}]}`, true},
		{"empty group nested", `{"any":[{"all":[]},` + no + `]}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := cond(t, tc.cond)
			if got := Evaluate(c, state); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			// A run evaluates the condition from its workflow snapshot, a JSON round trip
			// in which omitempty drops an empty group: the answer must not change.
			raw, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if got := Evaluate(cond(t, string(raw)), state); got != tc.want {
				t.Fatalf("after the snapshot round trip (%s): got %v, want %v", raw, got, tc.want)
			}
		})
	}
}

// T5.6.4: edges from a node are tried in definition order and the first true one wins;
// an edge without when is always true.
func TestNextEdgeOrder(t *testing.T) {
	d := &Definition{Edges: []Edge{
		{From: "reviewer", To: "coder", When: cond(t, `{"path":"/reviewer/verdict","op":"eq","value":"changes_requested"}`)},
		{From: "reviewer", To: "gate", When: cond(t, `{"path":"/reviewer/score","op":"gt","value":5}`)},
		{From: "reviewer", To: "fallback"},
		{From: "reviewer", To: "never"},
	}}
	tests := []struct {
		state string
		want  string
	}{
		{`{"reviewer":{"verdict":"changes_requested","score":9}}`, "coder"}, // both of the first two hold
		{`{"reviewer":{"verdict":"approved","score":9}}`, "gate"},
		{`{"reviewer":{"verdict":"approved","score":1}}`, "fallback"},
		{`{}`, "fallback"},
	}
	for _, tc := range tests {
		var state any
		_ = json.Unmarshal([]byte(tc.state), &state)
		if got, ok := d.Next("reviewer", state); !ok || got != tc.want {
			t.Errorf("%s: next = %q %v, want %q", tc.state, got, ok, tc.want)
		}
	}
	if _, ok := d.Next("nobody", nil); ok {
		t.Fatal("a node without edges has no next node")
	}
	strict := &Definition{Edges: []Edge{{From: "a", To: "b", When: cond(t, `{"path":"/x","op":"exists"}`)}}}
	if _, ok := strict.Next("a", map[string]any{}); ok {
		t.Fatal("no edge holds, yet one was taken")
	}
}

func TestConditionValidation(t *testing.T) {
	tests := []struct {
		cond string
		want string // substring; "" = valid
	}{
		{`{"path":"/a","op":"eq","value":1}`, ""},
		{`{"all":[{"path":"/a","op":"exists"},{"any":[{"path":"/b","op":"in","value":[1]}]}]}`, ""},
		{`{"path":"a","op":"eq","value":1}`, "edges[0].when.path: must be a JSON pointer"},
		{`{"path":"/a","op":"like","value":1}`, "edges[0].when.op: unknown op 'like'"},
		{`{"path":"/a","op":"in","value":1}`, "edges[0].when.value: must be an array for 'in'"},
		{`{"path":"/a","op":"gt","value":"x"}`, "edges[0].when.value: must be a number for 'gt'"},
		{`{"path":"/a","op":"exists","value":"yes"}`, "edges[0].when.value: must be true or false"},
		{`{"all":[{"path":"/a","op":"eq","value":1},{"path":"/b","op":"zz"}]}`, "edges[0].when.all[1].op: unknown op 'zz'"},
		{`{"all":[],"path":"/a","op":"eq"}`, "edges[0].when: use one of"},
		// Review finding 7 (wave 2): an empty group is refused, at any depth.
		{`{"all":[]}`, "edges[0].when: empty all/any group"},
		{`{"any":[]}`, "edges[0].when: empty all/any group"},
		{`{"all":[{"path":"/a","op":"exists"},{"any":[]}]}`, "edges[0].when.all[1]: empty all/any group"},
	}
	for _, tc := range tests {
		d := Definition{
			Nodes: []Node{{ID: "start", Kind: KindStart}, {ID: "end", Kind: KindEnd}},
			Edges: []Edge{{From: "start", To: "end", When: cond(t, tc.cond)}},
		}
		err := d.ValidateStructure()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.cond, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err = %v, want %q", tc.cond, err, tc.want)
		}
	}
}
