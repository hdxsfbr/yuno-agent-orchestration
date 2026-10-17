package obs

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// T5.12.4: redactor table test.
func TestRedact(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"anthropic key", "key sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789 here", "key [redacted] here"},
		{"telegram token", "token 1234567890:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw_X", "token [redacted]"},
		{"gitlab pat", "glpat-abcdefghijklmnopqrstuv", "[redacted]"},
		{"gitlab deploy token", "pushed with gldt-abcdefghijklmnopqrstuv", "pushed with [redacted]"},
		{"github classic token", "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", "[redacted]"},
		{"github fine-grained token", "github_pat_11ABCDEFG0123456789_abcdefghijklmnop", "[redacted]"},
		{"basic credentials", "Authorization: Basic b2F1dGgyOmdscGF0LWFiY2RlZmdoaWprbG1ub3A=", "Authorization: [redacted]"},
		{"basic in prose untouched", "Basic responsibilities of the reviewer", "Basic responsibilities of the reviewer"},
		{"bearer", "Authorization: Bearer abcdefghijklmnopqrstuvwxyz", "Authorization: [redacted]"},
		{"idempotency key untouched", "idempotency_key=9f86d081884c7d659a2feaa0c55ad015", "idempotency_key=9f86d081884c7d659a2feaa0c55ad015"},
		{"short digits untouched", "chat 12345:abc", "chat 12345:abc"},
		{"openrouter key", "key sk-or-v1-0123456789abcdef0123456789abcdef here", "key [redacted] here"},
		{"run token", "run_token: r_abcdefghijklmnopqrstuvwx\n", "run_token: [redacted]\n"},
		{"run token, upper case", "R_ABCDEFGHIJKLMNOPQRSTUVWX.", "[redacted]."},
		{"run token in json", `{"t":"r_abcdefghijklmnopqrstuv23"}`, `{"t":"[redacted]"}`},
		{"not a run token: too short", "r_abcdefghijklmnopqrstuvw", "r_abcdefghijklmnopqrstuvw"},
		{"not a run token: too long", "r_abcdefghijklmnopqrstuvwxy", "r_abcdefghijklmnopqrstuvwxy"},
		{"not a run token: inside a word", "attr_abcdefghijklmnopqrstuvwx", "attr_abcdefghijklmnopqrstuvwx"},
		{"not a run token: base32 has no 0, 1, 8, 9", "r_abcdefghijklmnopqrstuv01", "r_abcdefghijklmnopqrstuv01"},
		{"chat token", "chat_token: c_abcdefghijklmnopqrstuvwxyz234567\n", "chat_token: [redacted]\n"},
		{"chat token, upper case", "C_ABCDEFGHIJKLMNOPQRSTUVWXYZ234567.", "[redacted]."},
		{"chat token in json", `{"t":"c_abcdefghijklmnopqrstuvwxyz234567"}`, `{"t":"[redacted]"}`},
		{"not a chat token: too short", "c_abcdefghijklmnopqrstuvwxyz23456", "c_abcdefghijklmnopqrstuvwxyz23456"},
		{"not a chat token: inside a word", "abc_abcdefghijklmnopqrstuvwxyz234567", "abc_abcdefghijklmnopqrstuvwxyz234567"},
	}
	for _, c := range cases {
		if got := Redact(c.in); got != c.want {
			t.Errorf("%s: Redact(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestIsSecretKey(t *testing.T) {
	yes := []string{"token", "api_key", "apiKey", "TELEGRAM_BOT_TOKEN", "webhook_secret", "password", "authorization", "x-token",
		"run_token", "chat_token"}
	no := []string{"idempotency_key", "key", "run_id", "tokens", "input_tokens", "keyboard", "monkey"}
	for _, k := range yes {
		if !IsSecretKey(k) {
			t.Errorf("IsSecretKey(%q) = false, want true", k)
		}
	}
	for _, k := range no {
		if IsSecretKey(k) {
			t.Errorf("IsSecretKey(%q) = true, want false", k)
		}
	}
}

func TestRedactJSON(t *testing.T) {
	in := []byte(`{"api_key":"sk-ant-api03-xxxxxxxxxxxxxxxx","nested":{"token":"abc","ok":"sk-ant-api03-yyyyyyyyyyyyyyyy inside"},"list":[{"secret":1},"plain"],"input_tokens":12}`)
	var got map[string]any
	if err := json.Unmarshal(RedactJSON(in), &got); err != nil {
		t.Fatal(err)
	}
	if got["api_key"] != Redacted {
		t.Errorf("api_key not redacted: %v", got["api_key"])
	}
	nested := got["nested"].(map[string]any)
	if nested["token"] != Redacted || nested["ok"] != "[redacted] inside" {
		t.Errorf("nested not redacted: %v", nested)
	}
	if got["list"].([]any)[0].(map[string]any)["secret"] != Redacted {
		t.Errorf("list secret not redacted")
	}
	if got["input_tokens"] != float64(12) {
		t.Errorf("input_tokens changed: %v", got["input_tokens"])
	}
}

func TestLoggerRedacts(t *testing.T) {
	var buf bytes.Buffer
	h := &redactingHandler{inner: slog.NewJSONHandler(&buf, nil)}
	log := slog.New(h)
	log.Info("sending sk-ant-api03-abcdefghijklmnopqrstuvwxyz", "bot_token", "1234567890:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw_X", "run_id", "r1", slog.Group("g", "secret", "s", "name", "n"))
	out := buf.String()
	if strings.Contains(out, "sk-ant") || strings.Contains(out, "AAHdqTcvCH1") || strings.Contains(out, `"secret":"s"`) {
		t.Fatalf("secret leaked: %s", out)
	}
	if !strings.Contains(out, `"run_id":"r1"`) || !strings.Contains(out, `"name":"n"`) {
		t.Fatalf("non-secret attrs lost: %s", out)
	}
}

func TestTruncate(t *testing.T) {
	s := strings.Repeat("a", 100)
	got := Truncate(s, 40)
	if !strings.HasPrefix(got, strings.Repeat("a", 40)) || !strings.HasSuffix(got, "[truncated 60 bytes]") {
		t.Fatalf("got %q", got)
	}
	if Truncate("short", 40) != "short" {
		t.Fatal("short string changed")
	}
	// rune boundary: "é" is 2 bytes; cutting at 1 must step back to 0
	if got := Truncate("é", 1); !strings.HasPrefix(got, "\n[truncated 2 bytes]") {
		t.Fatalf("rune boundary: %q", got)
	}
}
