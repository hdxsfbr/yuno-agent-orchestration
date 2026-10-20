package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Every shipped template's state schema accepts what its steps may output: for each
// agent node, every property the node's output schema allows is allowed in the node's
// entry of the state schema, with every enum value. (The doc audit's "rework" verdict was
// added to the output schema and not to the state schema, and the first real rework
// failed the run with "state schema: value must be 'ok'".)
func TestTemplateStateSchemasAcceptNodeOutputs(t *testing.T) {
	files, err := filepath.Glob("../../templates/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("templates: %v %v", files, err)
	}
	type schema struct {
		Properties map[string]struct {
			Enum []any `json:"enum"`
		} `json:"properties"`
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var tpl struct {
			Definition *Definition `json:"definition"`
		}
		if err := json.Unmarshal(raw, &tpl); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		def := tpl.Definition
		if def == nil {
			def = &Definition{}
			if err := json.Unmarshal(raw, def); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
		}
		if len(def.StateSchema) == 0 {
			continue
		}
		var state struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(def.StateSchema, &state); err != nil {
			t.Fatalf("%s state schema: %v", f, err)
		}
		for _, n := range def.Nodes {
			if n.Kind != KindAgent || len(n.OutputSchema) == 0 {
				continue
			}
			entry, ok := state.Properties[n.ID]
			if !ok {
				continue // a node the state schema does not constrain
			}
			var out, st schema
			if err := json.Unmarshal(n.OutputSchema, &out); err != nil {
				t.Fatalf("%s %s output schema: %v", f, n.ID, err)
			}
			if err := json.Unmarshal(entry, &st); err != nil {
				t.Fatalf("%s %s state entry: %v", f, n.ID, err)
			}
			for prop, o := range out.Properties {
				s, ok := st.Properties[prop]
				if !ok {
					// Allowed when the state entry leaves extra properties open; the enum
					// check below is what a narrower state entry gets wrong.
					continue
				}
				if len(s.Enum) == 0 {
					continue
				}
				for _, v := range o.Enum {
					if !slices.Contains(s.Enum, v) {
						t.Errorf("%s: node %s may output %s=%v, which its state schema refuses (allows %v)",
							filepath.Base(f), n.ID, prop, v, s.Enum)
					}
				}
			}
		}
	}
}
