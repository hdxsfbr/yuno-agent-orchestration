package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// RunTokenField is the input field every platform tool carries (DESIGN 5.7 rule 3).
const RunTokenField = "run_token"

// ChatTokenField is the input field a chat-time call carries instead of run_token
// (DESIGN 5.15 rule 1).
const ChatTokenField = "chat_token"

// CanonicalJSON re-encodes a JSON document with object keys sorted, no insignificant
// whitespace, numbers as written and no HTML escaping (DESIGN 5.4 rule 1), so inputs
// that differ only in key order or spacing encode identically.
func CanonicalJSON(raw json.RawMessage) ([]byte, error) {
	v, err := decodeJSON(raw)
	if err != nil {
		return nil, err
	}
	return encodeCanonical(v)
}

// IdempotencyKey is sha256(run_id || node_visit_id || tool_name || canonical_json(input)),
// hex (DESIGN 5.4 rule 1). The run token is left out of the input: it binds the call to
// the visit, which the key already contains, and it is redacted where the row stores the
// input, so leaving it out keeps the key reproducible from the stored row.
func IdempotencyKey(runID, nodeVisitID, toolName string, input json.RawMessage) (string, error) {
	v, err := decodeJSON(input)
	if err != nil {
		return "", err
	}
	if m, ok := v.(map[string]any); ok {
		delete(m, RunTokenField)
	}
	canon, err := encodeCanonical(v)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(runID))
	h.Write([]byte(nodeVisitID))
	h.Write([]byte(toolName))
	h.Write(canon)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func decodeJSON(raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("canonical json: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("canonical json: trailing data")
	}
	return v, nil
}

// encodeCanonical relies on encoding/json sorting map keys; SetEscapeHTML(false) keeps
// <, > and & as themselves.
func encodeCanonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
