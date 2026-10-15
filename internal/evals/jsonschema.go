package evals

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// The evaluators validate with the library the engine validates with, so a document
// the engine accepted is never rejected here for a difference of dialect.

var schemaPrinter = message.NewPrinter(language.English)

// compileSchema compiles a JSON Schema document.
func compileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}
	c := jsonschema.NewCompiler()
	const loc = "https://yuno.local/evals/schema.json"
	if err := c.AddResource(loc, doc); err != nil {
		return nil, err
	}
	s, err := c.Compile(loc)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// validateDoc checks a JSON document against a compiled schema and returns the first
// violation as "at /path: message", or "" when it is valid.
func validateDoc(s *jsonschema.Schema, doc json.RawMessage) string {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return "not valid JSON"
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
	return fmt.Sprintf("at /%s: %s", strings.Join(leaf.InstanceLocation, "/"), leaf.ErrorKind.LocalizedString(schemaPrinter))
}

// hasSchema reports whether raw holds a schema (not empty and not JSON null).
func hasSchema(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && string(t) != "null"
}
