package runtoken

import (
	"crypto/rand"
	"regexp"
	"strings"
)

// ChatPrefix identifies a chat token in free text. A chat token is an agent's identity
// for platform tool calls made while it chats with a person (DESIGN 5.15 rule 1): the
// gateway's MCP client carries nothing that names the calling agent, so the call carries
// it, as a run's calls carry the run token. It is written into the agent's AGENTS.md and
// is never returned by the API.
const ChatPrefix = "c_"

// ChatPattern matches a chat token anywhere in text.
var ChatPattern = regexp.MustCompile(`\bc_[a-z2-7]{32}\b`)

// NewChat returns a fresh chat token: "c_" plus 32 lowercase base32 characters (160 bits).
func NewChat() string {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("runtoken: crypto/rand unavailable: " + err.Error())
	}
	return ChatPrefix + strings.ToLower(enc.EncodeToString(b[:]))
}
