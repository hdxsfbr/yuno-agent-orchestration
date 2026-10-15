package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// tokenEntry is one live run token: the context it resolves to, the visit it belongs
// to and the compiled output schema (DESIGN 5.14 rule 7).
type tokenEntry struct {
	rc        RunContext
	v         *visit
	schema    *jsonschema.Schema
	expiresAt time.Time
}

// tokenRegistry holds the tokens of node visits in progress. A token is removed when
// its visit ends and is refused after expiresAt even if the visit leaked.
type tokenRegistry struct {
	clock Clock
	mu    sync.RWMutex
	m     map[string]*tokenEntry
}

func newTokenRegistry(c Clock) *tokenRegistry {
	return &tokenRegistry{clock: c, m: map[string]*tokenEntry{}}
}

func (r *tokenRegistry) add(token string, e *tokenEntry) {
	r.mu.Lock()
	r.m[token] = e
	r.mu.Unlock()
}

func (r *tokenRegistry) remove(token string) {
	r.mu.Lock()
	delete(r.m, token)
	r.mu.Unlock()
}

func (r *tokenRegistry) get(token string) (*tokenEntry, bool) {
	r.mu.RLock()
	e, ok := r.m[token]
	r.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if !e.expiresAt.IsZero() && !r.clock.Now().Before(e.expiresAt) {
		r.remove(token)
		return nil, false
	}
	return e, true
}

// compileSchema compiles a node output schema. An empty schema accepts any object.
func compileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = json.RawMessage(`{"type":"object"}`)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("output schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("output-schema.json", doc); err != nil {
		return nil, fmt.Errorf("output schema: %w", err)
	}
	s, err := c.Compile("output-schema.json")
	if err != nil {
		return nil, fmt.Errorf("output schema: %w", err)
	}
	return s, nil
}

var schemaPrinter = message.NewPrinter(language.English)

// validateOutput checks a result against the schema and returns the first violation
// as a path-qualified message the model can act on, or "" when valid.
func validateOutput(s *jsonschema.Schema, result json.RawMessage) string {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(result))
	if err != nil {
		return "result is not valid JSON"
	}
	if _, ok := inst.(map[string]any); !ok {
		return "result must be a JSON object"
	}
	err = s.Validate(inst)
	if err == nil {
		return ""
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err.Error()
	}
	leaf := ve
	for len(leaf.Causes) > 0 {
		leaf = leaf.Causes[0]
	}
	loc := "/" + strings.Join(leaf.InstanceLocation, "/")
	return fmt.Sprintf("result at %s: %s", loc, leaf.ErrorKind.LocalizedString(schemaPrinter))
}

// findJSONResult returns the first JSON object in text that validates against s.
func findJSONResult(s *jsonschema.Schema, text string) (json.RawMessage, bool) {
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(text[i:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			continue
		}
		if validateOutput(s, raw) == "" {
			return raw, true
		}
	}
	return nil, false
}
