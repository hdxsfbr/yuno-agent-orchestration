package tools

import (
	"bytes"
	"encoding/json"

	"yuno/internal/obs"
)

// Render turns a result into the text the model sees and the JSON stored on the
// tool_calls row (DESIGN 5.7 rule 4): the text is redacted and capped at maxBytes with
// the truncation marker (0 means no cap); the stored value is the JSON output when it
// survived the cap intact, else {"error": text} for errors and {"text": text} for plain
// results, redacted again by key.
func Render(res Result, maxBytes int) (text string, stored json.RawMessage) {
	full := obs.Redact(res.ModelText())
	text = obs.Truncate(full, maxBytes)
	if len(res.Output) > 0 && text == full && json.Valid([]byte(text)) {
		return text, obs.RedactJSON([]byte(text))
	}
	field := "text"
	if res.IsError {
		field = "error"
	}
	b, _ := json.Marshal(map[string]string{field: text})
	return text, b
}

// WithFields returns a JSON object with the extra fields set (output.replayed, DESIGN
// 5.4 rule 2; "policy": "auto", T5.3.6). A non-object output is wrapped as
// {"result": output} first, so the fields always have somewhere to go.
func WithFields(out json.RawMessage, fields map[string]any) json.RawMessage {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil || m == nil {
		m = map[string]any{}
		if len(bytes.TrimSpace(out)) > 0 {
			m["result"] = json.RawMessage(out)
		}
	}
	for k, v := range fields {
		m[k] = v
	}
	b, err := encodeCanonical(m)
	if err != nil {
		return out
	}
	return b
}

// JSONResult is a successful result with a JSON output.
func JSONResult(v any) Result {
	b, err := json.Marshal(v)
	if err != nil {
		return Result{IsError: true, Text: "could not encode the result"}
	}
	return Result{Output: b}
}
