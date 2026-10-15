package workflow

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Condition operators (DESIGN 5.6 rule 3).
const (
	OpEq     = "eq"
	OpNeq    = "neq"
	OpIn     = "in"
	OpExists = "exists"
	OpGt     = "gt"
	OpLt     = "lt"
)

var conditionOps = map[string]bool{OpEq: true, OpNeq: true, OpIn: true, OpExists: true, OpGt: true, OpLt: true}

// Evaluate reports whether c holds over state, the decoded run state (DESIGN 5.6 rule
// 3). A nil condition is true. A path that does not resolve is exists:false and makes
// every other operator false, neq included. gt and lt compare numbers only.
func Evaluate(c *Condition, state any) bool {
	if c == nil {
		return true
	}
	// An empty group is invalid (validateCondition) and never holds: that is also how it
	// reads once omitempty has dropped it from a stored snapshot.
	if (c.All != nil && len(c.All) == 0) || (c.Any != nil && len(c.Any) == 0) {
		return false
	}
	if c.All != nil {
		for i := range c.All {
			if !Evaluate(&c.All[i], state) {
				return false
			}
		}
		return true
	}
	if c.Any != nil {
		for i := range c.Any {
			if Evaluate(&c.Any[i], state) {
				return true
			}
		}
		return false
	}
	got, found := Lookup(state, c.Path)
	if c.Op == OpExists {
		want := true
		if len(c.Value) > 0 {
			if err := json.Unmarshal(c.Value, &want); err != nil {
				return false
			}
		}
		return found == want
	}
	if !found {
		return false
	}
	var want any
	if len(c.Value) > 0 {
		if err := json.Unmarshal(c.Value, &want); err != nil {
			return false
		}
	}
	switch c.Op {
	case OpEq:
		return reflect.DeepEqual(got, want)
	case OpNeq:
		return !reflect.DeepEqual(got, want)
	case OpIn:
		list, ok := want.([]any)
		if !ok {
			return false
		}
		for _, w := range list {
			if reflect.DeepEqual(got, w) {
				return true
			}
		}
		return false
	case OpGt, OpLt:
		g, ok1 := got.(float64)
		w, ok2 := want.(float64)
		if !ok1 || !ok2 {
			return false
		}
		if c.Op == OpGt {
			return g > w
		}
		return g < w
	}
	return false
}

// EvaluateJSON is Evaluate over a raw JSON state.
func EvaluateJSON(c *Condition, state json.RawMessage) bool {
	var doc any
	if len(state) > 0 {
		if err := json.Unmarshal(state, &doc); err != nil {
			return false
		}
	}
	return Evaluate(c, doc)
}

// Lookup resolves an RFC 6901 JSON pointer in a decoded JSON document. The empty
// pointer is the whole document; a pointer not starting with "/" never resolves.
func Lookup(doc any, pointer string) (any, bool) {
	if pointer == "" {
		return doc, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	cur := doc
	for _, tok := range strings.Split(pointer[1:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[tok]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(v) || (len(tok) > 1 && tok[0] == '0') {
				return nil, false
			}
			cur = v[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// Next returns the target of the first edge from `from` whose condition holds over
// state, in definition order (DESIGN 5.6 rule 4).
func (d *Definition) Next(from string, state any) (string, bool) {
	for _, e := range d.OutgoingEdges(from) {
		if Evaluate(e.When, state) {
			return e.To, true
		}
	}
	return "", false
}

// validateCondition returns path-qualified messages for a malformed condition.
func validateCondition(path string, c *Condition) []string {
	if c == nil {
		return nil
	}
	var msgs []string
	leaf := c.Path != "" || c.Op != "" || len(c.Value) > 0
	switch {
	case c.All != nil && c.Any != nil, (c.All != nil || c.Any != nil) && leaf:
		return []string{path + ": use one of {path, op, value}, all or any"}
	case (c.All != nil && len(c.All) == 0) || (c.Any != nil && len(c.Any) == 0):
		return []string{path + ": empty all/any group"}
	case c.All != nil:
		for i := range c.All {
			msgs = append(msgs, validateCondition(fmt.Sprintf("%s.all[%d]", path, i), &c.All[i])...)
		}
		return msgs
	case c.Any != nil:
		for i := range c.Any {
			msgs = append(msgs, validateCondition(fmt.Sprintf("%s.any[%d]", path, i), &c.Any[i])...)
		}
		return msgs
	}
	if !strings.HasPrefix(c.Path, "/") {
		msgs = append(msgs, path+".path: must be a JSON pointer starting with '/'")
	}
	if !conditionOps[c.Op] {
		msgs = append(msgs, fmt.Sprintf("%s.op: unknown op '%s' (use eq, neq, in, exists, gt or lt)", path, c.Op))
		return msgs
	}
	var v any
	if len(c.Value) > 0 && json.Unmarshal(c.Value, &v) != nil {
		return append(msgs, path+".value: must be JSON")
	}
	switch c.Op {
	case OpIn:
		if _, ok := v.([]any); !ok {
			msgs = append(msgs, path+".value: must be an array for 'in'")
		}
	case OpGt, OpLt:
		if _, ok := v.(float64); !ok {
			msgs = append(msgs, fmt.Sprintf("%s.value: must be a number for '%s'", path, c.Op))
		}
	case OpExists:
		if _, ok := v.(bool); len(c.Value) > 0 && !ok {
			msgs = append(msgs, path+".value: must be true or false for 'exists'")
		}
	}
	return msgs
}
