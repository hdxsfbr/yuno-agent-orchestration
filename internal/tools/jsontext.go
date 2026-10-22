package tools

import (
	"bytes"
	"encoding/json"
)

// jsonText reads a tool argument that may come as JSON text (D-104). On OpenAI's native
// route the gateway makes every tool schema strict, which closes an open object parameter
// so that only {} gets through; such a parameter is declared as text or object, the text
// form is the one the tools ask for, and an object still passes for a Claude or scripted
// caller. It returns the value's JSON: the text's content (which must be an object when
// object is true, any JSON value otherwise), or raw as it came. An absent or null argument
// comes back empty.
func jsonText(raw json.RawMessage, field string, object bool) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return raw, nil // not text: the schema allows only the object
	}
	value := json.RawMessage(bytes.TrimSpace([]byte(text)))
	if object {
		var m map[string]json.RawMessage
		if json.Unmarshal(value, &m) != nil || m == nil {
			return nil, &ModelError{Message: field + ": a string must be the JSON text of an object"}
		}
		return value, nil
	}
	if !json.Valid(value) {
		return nil, &ModelError{Message: field + ": a string must be JSON text"}
	}
	return value, nil
}
