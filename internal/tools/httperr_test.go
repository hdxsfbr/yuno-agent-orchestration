package tools

import (
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// A provider's error text is cut at 300 bytes on a rune boundary: a character that
// straddles the cut is dropped whole, so the text stays valid UTF-8 and can be stored.
func TestStatusFailureCutsOnARuneBoundary(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusInternalServerError}
	for name, ch := range map[string]string{"two bytes": "é", "three bytes": "€", "four bytes": "😀"} {
		for pad := 300 - len(ch) - 1; pad <= 300; pad++ { // the character starts before, at and after the cut
			body := strings.Repeat("a", pad) + ch + strings.Repeat("b", 50)
			got := statusFailure("gitlab", resp, []byte(body)).Text
			prefix := "gitlab: HTTP 500: "
			msg := strings.TrimPrefix(got, prefix)
			if !utf8.ValidString(got) || len(msg) > 300 || !strings.HasPrefix(body, msg) {
				t.Fatalf("%s at %d: %d bytes, valid %v: %q", name, pad, len(msg), utf8.ValidString(got), msg[max(0, len(msg)-8):])
			}
			if len(body) > 300 && len(msg) < 300-(len(ch)-1) {
				t.Fatalf("%s at %d: cut too early, %d bytes", name, pad, len(msg))
			}
		}
	}
	// Bytes that are not UTF-8 anywhere in the body are dropped too.
	got := statusFailure("github", resp, []byte("bad \xff\xfe gateway")).Text
	if got != "github: HTTP 500: bad  gateway" {
		t.Fatalf("invalid bytes kept: %q", got)
	}
	// A body of at most 300 bytes is kept whole, and the provider's message is cut alike.
	if got := providerMessage([]byte(strings.Repeat("é", 200))); len(got) != 300 || !utf8.ValidString(got) {
		t.Fatalf("providerMessage: %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	if got := providerMessage([]byte(strings.Repeat("a", 299) + "é")); len(got) != 299 || !utf8.ValidString(got) {
		t.Fatalf("providerMessage across the cut: %d bytes", len(got))
	}
}
