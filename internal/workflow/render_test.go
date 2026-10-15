package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	input := json.RawMessage(`{"task":"Write fizzbuzz.","n":3,"version":"2.0.0"}`)
	state := json.RawMessage(`{"reviewer":{"verdict":"changes_requested","feedback":"Add tests.","note":null},
		"coder":{"summary":"done","files":["a.py","b.py"]}}`)
	tests := []struct {
		name, tmpl, state, want string
	}{
		{name: "input", tmpl: "Do: {{.input.task}}", want: "Do: Write fizzbuzz."},
		{name: "with present", tmpl: "{{with .state.reviewer}}Fix: {{.feedback}}{{end}}", state: string(state), want: "Fix: Add tests."},
		{name: "with absent", tmpl: "x{{with .state.reviewer}}Fix: {{.feedback}}{{end}}y", want: "xy"},
		{name: "missing key renders empty", tmpl: "[{{.input.nope}}]", want: "[]"},
		{name: "missing intermediate renders empty", tmpl: "[{{.state.coder.summary}}]", want: "[]"},
		{name: "null renders empty", tmpl: "[{{.state.reviewer.note}}]", state: string(state), want: "[]"},
		{name: "range", tmpl: "{{range .state.coder.files}}{{.}} {{end}}", state: string(state), want: "a.py b.py "},
		{name: "range over missing", tmpl: "[{{range .state.coder.files}}{{.}}{{end}}]", want: "[]"},
		{name: "or default", tmpl: `{{or .input.release "1.0.0"}}`, want: "1.0.0"},
		{name: "or present", tmpl: `{{or .input.version "1.0.0"}}`, want: "2.0.0"},
		{name: "number", tmpl: "{{.input.n}}", want: "3"},
		{name: "variable declaration prints nothing", tmpl: "{{$t := .input.task}}<{{$t}}>", want: "<Write fizzbuzz.>"},
		{name: "if on missing", tmpl: "{{if .state.critic}}yes{{else}}no{{end}}", want: "no"},
		{name: "defined template", tmpl: `{{define "fb"}}[{{.feedback}}]{{end}}{{template "fb" .state.reviewer}}`, want: "[]"},
		{name: "wave 1 quick-run task", tmpl: "{{input.task}} now", want: "Write fizzbuzz. now"},
		{name: "input text is data, not template", tmpl: "{{.input.task}}", want: "Write fizzbuzz."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var st json.RawMessage
			if tc.state != "" {
				st = json.RawMessage(tc.state)
			}
			got, err := Render(tc.tmpl, input, st)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "<no value>") || strings.Contains(got, "<nil>") {
				t.Fatalf("placeholder printed: %q", got)
			}
		})
	}
	braces, err := Render("{{.input.task}}", json.RawMessage(`{"task":"print {{.state}} literally"}`), nil)
	if err != nil || braces != "print {{.state}} literally" {
		t.Fatalf("input with braces = %q, %v", braces, err)
	}
}

// T5.6.5 (render side): parse and execution errors come back as errors.
func TestRenderErrors(t *testing.T) {
	for _, tmpl := range []string{
		"{{.input.task",          // unclosed action
		"{{nofunc .input.task}}", // unknown function
		"{{index .input.items 3}}",
		"{{.state.reviewer.note.text}}", // field of a JSON null
	} {
		state := json.RawMessage(`{"reviewer":{"note":null}}`)
		if _, err := Render(tmpl, json.RawMessage(`{"items":[1]}`), state); err == nil {
			t.Errorf("%q rendered without error", tmpl)
		}
	}
	d := Definition{Nodes: []Node{
		{ID: "start", Kind: KindStart},
		{ID: "a", Kind: KindAgent, AgentID: "x", Task: "fine {{.input.task}}"},
		{ID: "b", Kind: KindAgent, AgentID: "x", Task: "{{.input.task"},
		{ID: "gate", Kind: KindApproval, Message: "{{bad}}"},
	}}
	msgs := d.ValidateTemplates(json.RawMessage(`{"task":"t"}`))
	if len(msgs) != 2 || !strings.HasPrefix(msgs[0], "nodes[2].task: ") || !strings.HasPrefix(msgs[1], "nodes[3].message: ") {
		t.Fatalf("messages = %q", msgs)
	}
}

// The shipped templates validate structurally and render against an empty state.
func TestShippedTemplates(t *testing.T) {
	files, err := filepath.Glob("../../templates/*.json")
	if err != nil || len(files) < 2 {
		t.Fatalf("templates: %v %v", files, err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var tpl struct {
			Definition json.RawMessage `json:"definition"`
		}
		if err := json.Unmarshal(raw, &tpl); err != nil {
			t.Fatal(err)
		}
		d, err := Parse(tpl.Definition)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.ValidateStructure(); err != nil {
			t.Errorf("%s: %v", f, err)
		}
		if msgs := d.ValidateTemplates(nil); len(msgs) > 0 {
			t.Errorf("%s: %v", f, msgs)
		}
	}
}
