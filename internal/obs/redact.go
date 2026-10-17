package obs

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// Redacted is the replacement text for any secret.
const Redacted = "[redacted]"

// Values that look like credentials wherever they appear in a string.
var secretValuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{10,}`),            // Anthropic API keys
	regexp.MustCompile(`sk-or-v1-[A-Za-z0-9]{10,}`),            // OpenRouter API keys
	regexp.MustCompile(`\b\d{6,}:[A-Za-z0-9_-]{30,}`),          // Telegram bot tokens
	regexp.MustCompile(`gl(?:pat|dt|oas|rt)-[A-Za-z0-9_-]{15,}`), // GitLab personal, deploy and access tokens
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),              // GitHub tokens (classic)
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),            // GitHub fine-grained tokens
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{16,}`),   // Authorization headers
	// Basic credentials (DESIGN 5.16 rule 4 sends the project token this way to git):
	// "Basic" and a base64 value with at least one digit or symbol, so prose such as
	// "Basic responsibilities" stays.
	regexp.MustCompile(`Basic\s+[A-Za-z0-9+/]*[0-9+/][A-Za-z0-9+/]{8,}={0,2}`),
	// Run tokens (DESIGN 5.14 rule 7) bind platform tool calls to a node visit; logs and
	// stored transcripts never keep one. Minted lowercase, matched in either case.
	regexp.MustCompile(`(?i)\br_[a-z2-7]{24}\b`),
	// Chat tokens (DESIGN 5.15 rule 1) identify an agent's chat-time tool calls; like run
	// tokens they never reach logs, traces, stored inputs or the UI.
	regexp.MustCompile(`(?i)\bc_[a-z2-7]{32}\b`),
}

// Keys whose whole value is a secret regardless of shape. Deliberately does not match
// a bare "key" so idempotency_key, api_key-like hashes and JSON pointer keys survive.
var secretKeyPattern = regexp.MustCompile(`(?i)(^|_|-)(api_?key|token|secret|password|passwd|authorization|credential|private_key)($|_|-)`)

// IsSecretKey reports whether a field name denotes a secret value.
func IsSecretKey(key string) bool {
	return secretKeyPattern.MatchString(key)
}

// Redact replaces credential-shaped substrings in s.
func Redact(s string) string {
	for _, re := range secretValuePatterns {
		s = re.ReplaceAllString(s, Redacted)
	}
	return s
}

// RedactJSON walks a JSON document and redacts secret keys and credential-shaped
// strings. Invalid JSON is treated as a string. The result is always valid JSON, with
// numbers kept as written.
func RedactJSON(raw []byte) []byte {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil || dec.More() {
		b, _ := json.Marshal(Redact(string(raw)))
		return b
	}
	out, _ := json.Marshal(redactValue(v))
	return out
}

func redactValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			if IsSecretKey(k) {
				m[k] = Redacted
				continue
			}
			m[k] = redactValue(val)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = redactValue(val)
		}
		return out
	case string:
		return Redact(x)
	default:
		return v
	}
}

// Truncate caps s at max bytes, appending a marker that says how much was dropped
// (DESIGN 5.7 rule 4). Cuts on a rune boundary.
func Truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	dropped := len(s) - cut
	var b strings.Builder
	b.Grow(cut + 32)
	b.WriteString(s[:cut])
	b.WriteString("\n[truncated ")
	b.WriteString(itoa(dropped))
	b.WriteString(" bytes]")
	return b.String()
}

func isRuneStart(c byte) bool { return c&0xC0 != 0x80 }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
