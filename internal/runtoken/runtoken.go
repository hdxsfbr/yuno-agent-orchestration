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
