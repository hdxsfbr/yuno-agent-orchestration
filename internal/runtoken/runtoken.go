// Package runtoken mints and recognises the per-node-visit token that binds platform
// tool calls and scripted model requests to a run (DESIGN 5.14 rule 7).
package runtoken

import (
	"crypto/rand"
	"encoding/base32"
	"regexp"
	"strings"
)

// Prefix identifies a run token in free text.
const Prefix = "r_"

// ProtocolHeader opens the platform protocol block the engine appends to every node task
// message (DESIGN 5.6): run_id, run_token and script_id follow it, one per line.
const ProtocolHeader = "--- platform protocol ---"

// Pattern matches a token anywhere in text, e.g. inside a task message.
var Pattern = regexp.MustCompile(`\br_[a-z2-7]{24}\b`)

var enc = base32.StdEncoding.WithPadding(base32.NoPadding)

// New returns a fresh token: "r_" plus 24 lowercase base32 characters (120 bits).
func New() string {
	var b [15]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("runtoken: crypto/rand unavailable: " + err.Error())
	}
	return Prefix + strings.ToLower(enc.EncodeToString(b[:]))
}

// Find returns the first token in text, or "".
func Find(text string) string {
	return Pattern.FindString(text)
}

// ScriptIDPattern matches the "script_id: <id>" marker the engine writes into a task
// message for scripted (offline) runs; the scripted provider keys its script by it.
var ScriptIDPattern = regexp.MustCompile(`\bscript_id: ([a-zA-Z0-9._-]{1,80})\b`)

// FindScriptID returns the script id in text, or "".
func FindScriptID(text string) string {
	m := ScriptIDPattern.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return m[1]
}
