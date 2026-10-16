package openclaw

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func inputs() BaselineInputs {
	return BaselineInputs{
		GatewayToken: "gw-token", MCPToken: "mcp-token", StateDir: "/home/node/.openclaw",
		APIBase: "http://127.0.0.1:8080", DefaultModel: "claude-opus-5-5", SandboxImage: "yuno-sandbox:latest",
		TelegramMode: "off",
	}
}

// T5.14.2: idempotent, preserves foreign keys, overwrites managed ones.
func TestWriteBaseline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")
	// Pre-existing config with keys the platform does not own and one it does.
	pre := map[string]any{
		"agents": map[string]any{
			"entries":  map[string]any{"coder": map[string]any{"name": "Coder"}},
			"defaults": map[string]any{"model": map[string]any{"primary": "stale/model"}, "heartbeat": map[string]any{"every": "30m"}},
		},
		"skills":   map[string]any{"entries": map[string]any{"github": map[string]any{"enabled": true}}},
		"meta":     map[string]any{"migrations": map[string]any{"x": true}},
		"bindings": []any{map[string]any{"agentId": "coder"}},
	}
	raw, _ := json.Marshal(pre)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := WriteBaseline(dir, inputs())
	if err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v", changed, err)
	}
	changed, err = WriteBaseline(dir, inputs())
	if err != nil || changed {
		t.Fatalf("second write should be a no-op: changed=%v err=%v", changed, err)
	}

	var got map[string]any
	out, _ := os.ReadFile(path)
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	agents := got["agents"].(map[string]any)
	if agents["entries"].(map[string]any)["coder"] == nil {
		t.Error("agents.entries.coder was lost")
	}
	defaults := agents["defaults"].(map[string]any)
	if defaults["heartbeat"] == nil {
		t.Error("agents.defaults.heartbeat was lost")
	}
	if defaults["model"].(map[string]any)["primary"] != "yuno-anthropic/claude-opus-5-5" {
		t.Errorf("model not overwritten: %v", defaults["model"])
	}
	if got["skills"] == nil || got["bindings"] == nil {
		t.Error("foreign top-level keys lost")
	}
	meta := got["meta"].(map[string]any)
	if meta["migrations"] == nil || meta["managedBy"] != nil {
		t.Errorf("meta must be preserved and carry no platform keys: %v", meta)
	}
	if _, err := os.Stat(filepath.Join(dir, HashFile)); err != nil {
		t.Errorf("hash sidecar missing: %v", err)
	}
	if agents["entries"].(map[string]any)["main"] == nil {
		t.Error("agents.entries.main must be seeded for explicit ownership")
	}
	if got["gateway"].(map[string]any)["auth"].(map[string]any)["token"] != "gw-token" {
		t.Error("gateway token not set")
	}
	mcp := got["mcp"].(map[string]any)["servers"].(map[string]any)["yuno"].(map[string]any)
	if mcp["transport"] != "streamable-http" || mcp["headers"].(map[string]any)["Authorization"] != "Bearer mcp-token" {
		t.Errorf("mcp server wrong: %v", mcp)
	}
	sb := defaults["sandbox"].(map[string]any)
	if sb["mode"] != "all" || sb["docker"].(map[string]any)["network"] != "none" {
		t.Errorf("sandbox wrong: %v", sb)
	}
	if got["channels"].(map[string]any)["telegram"].(map[string]any)["enabled"] != false {
		t.Error("telegram should be disabled in off mode")
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", st.Mode().Perm())
	}

	// Changing an input rewrites.
	in := inputs()
	in.TelegramMode = "webhook"
	in.PublicBaseURL = "https://example.trycloudflare.com/"
	in.TelegramWebhookSecret = "s3cret-s3cret-s3cret-s3cret-s3cret"
	in.TelegramAllowedUserIDs = []int64{12345}
	changed, err = WriteBaseline(dir, in)
	if err != nil || !changed {
		t.Fatalf("input change should write: %v %v", changed, err)
	}
	out, _ = os.ReadFile(path)
	_ = json.Unmarshal(out, &got)
	tg := got["channels"].(map[string]any)["telegram"].(map[string]any)
	if tg["enabled"] != true || tg["webhookUrl"] != "https://example.trycloudflare.com/telegram-webhook" {
		t.Errorf("telegram webhook config wrong: %v", tg)
	}
	if tg["execApprovals"].(map[string]any)["enabled"] != true {
		t.Error("exec approvals should be enabled with approvers")
	}
}

func TestWriteBaselineRequiresTokens(t *testing.T) {
	in := inputs()
	in.MCPToken = ""
	if _, err := WriteBaseline(t.TempDir(), in); err == nil {
		t.Fatal("expected error without MCP token")
	}
}

// T5.8.2: config renders per mode.
func TestBaselineTelegramModes(t *testing.T) {
	for _, mode := range []string{"off", "polling", "webhook"} {
		in := inputs()
		in.TelegramMode = mode
		in.PublicBaseURL = "https://x.example"
		tg := Baseline(in)["channels.telegram"].(map[string]any)
		if (tg["enabled"] == true) != (mode != "off") {
			t.Errorf("%s: enabled=%v", mode, tg["enabled"])
		}
		if _, has := tg["webhookUrl"]; has != (mode == "webhook") {
			t.Errorf("%s: webhookUrl presence %v", mode, has)
		}
		// D-025: the gateway may start before the api re-asserts the binding, so the
		// baseline itself refuses DMs and groups.
		if tg["dmPolicy"] != TelegramDMClosed || tg["groupPolicy"] != "disabled" {
			t.Errorf("%s: dmPolicy=%v groupPolicy=%v, want both refused", mode, tg["dmPolicy"], tg["groupPolicy"])
		}
	}
	in := inputs()
	in.TelegramMode = "polling"
	in.TelegramAllowedUserIDs = []int64{1184040613}
	tg := Baseline(in)["channels.telegram"].(map[string]any)
	ea := tg["execApprovals"].(map[string]any)
	if ea["target"] != "dm" || len(ea["approvers"].([]any)) != 1 || len(tg["allowFrom"].([]any)) != 1 {
		t.Errorf("approvers and allowlist come from TELEGRAM_ALLOWED_USER_IDS: %v", tg)
	}
	if owners := Baseline(in)["commands.ownerAllowFrom"].([]any); len(owners) != 1 || owners[0] != int64(1184040613) {
		t.Errorf("commands.ownerAllowFrom = %v", owners)
	}
	if _, has := tg["botToken"]; has {
		t.Error("the bot token stays in the gateway's environment, never in the config")
	}
}
