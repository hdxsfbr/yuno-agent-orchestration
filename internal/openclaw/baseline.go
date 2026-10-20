// Package openclaw is the control plane's view of the OpenClaw gateway: the baseline
// configuration it owns (this file), and the gateway client (wave 1).
package openclaw

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"yuno/internal/model"
)

// BaselineInputs are the only things that influence the managed part of openclaw.json
// (DESIGN 5.14 rule 3). Their hash is stored in meta.yunoInputsHash so an unchanged
// re-run writes nothing.
type BaselineInputs struct {
	GatewayToken           string
	MCPToken               string
	StateDir               string // gateway-side path, e.g. /home/node/.openclaw
	APIBase                string // how the gateway reaches the api, e.g. http://127.0.0.1:8080
	DefaultModel           string // e.g. claude-opus-5-5
	SandboxImage           string
	TelegramMode           string // off | polling | webhook
	TelegramWebhookSecret  string
	TelegramAllowedUserIDs []int64
	PublicBaseURL          string
}

// ProviderID is the custom provider name OpenClaw uses for Anthropic models through the
// api's proxy. Model refs are "<ProviderID>/<model id>".
const ProviderID = "yuno-anthropic"

// ScriptedProviderID is the deterministic offline provider (DESIGN 5.14 rule 9).
const ScriptedProviderID = "scripted"

// ModelRef returns the OpenClaw model reference for an Anthropic model id.
func ModelRef(modelID string) string { return ProviderID + "/" + modelID }

// ManagedModels are the Anthropic models exposed through the proxy, with the price
// table OpenClaw uses for its own cost accounting (USD per million tokens).
var ManagedModels = []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"}

// Baseline returns the managed keys as a nested map keyed by dotted path. Paths listed
// here are owned by the platform and overwritten on every inputs change; everything
// else in the file is preserved by Merge.
func Baseline(in BaselineInputs) map[string]any {
	models := make([]any, 0, len(ManagedModels))
	for _, id := range ManagedModels {
		p, _ := model.LookupPrice(id)
		models = append(models, map[string]any{
			"id":            id,
			"name":          id,
			"reasoning":     true,
			"input":         []any{"text", "image"},
			"cost":          map[string]any{"input": p.Input, "output": p.Output, "cacheRead": p.CacheRead, "cacheWrite": p.CacheWrite},
			"contextWindow": 1000000,
			"maxTokens":     32000,
		})
	}
	sp, _ := model.LookupPrice("scripted")

	ids := make([]any, 0, len(in.TelegramAllowedUserIDs))
	for _, id := range in.TelegramAllowedUserIDs {
		ids = append(ids, id)
	}
	// Direct messages start refused and groups stay refused: only the platform opens DMs,
	// once an agent is bound (DESIGN 5.8 rule 7, D-025), so a gateway that starts before
	// the api, or with no binding, never answers from its default agent.
	telegram := map[string]any{
		"enabled":     in.TelegramMode == "polling" || in.TelegramMode == "webhook",
		"dmPolicy":    TelegramDMClosed,
		"groupPolicy": "disabled",
		"allowFrom":   ids,
		"execApprovals": map[string]any{
			"enabled":   len(ids) > 0,
			"approvers": ids,
			"target":    "dm",
		},
	}
	if in.TelegramMode == "webhook" {
		telegram["webhookUrl"] = strings.TrimRight(in.PublicBaseURL, "/") + "/telegram-webhook"
		telegram["webhookSecret"] = in.TelegramWebhookSecret
	}

	return map[string]any{
		"gateway.mode": "local",
		"gateway.auth": map[string]any{"mode": "token", "token": in.GatewayToken},
		"gateway.bind": "lan",
		"gateway.port": 18789,
		"models.mode":  "merge",
		"models.providers." + ProviderID: map[string]any{
			"baseUrl":        in.APIBase + "/model-proxy/anthropic",
			"apiKey":         "proxy",
			"api":            "anthropic-messages",
			"timeoutSeconds": 300,
			"models":         models,
		},
		"models.providers." + ScriptedProviderID: map[string]any{
			"baseUrl":        in.APIBase + "/model-proxy/scripted/v1",
			"apiKey":         "scripted",
			"api":            "openai-completions",
			"timeoutSeconds": 60,
			"models": []any{map[string]any{
				"id": "scripted-1", "name": "Scripted (offline)", "reasoning": false,
				"input":         []any{"text"},
				"cost":          map[string]any{"input": sp.Input, "output": sp.Output, "cacheRead": sp.CacheRead, "cacheWrite": sp.CacheWrite},
				"contextWindow": 200000, "maxTokens": 8192,
			}},
		},
		"mcp.servers.yuno": map[string]any{
			"url":                 in.APIBase + "/mcp",
			"transport":           "streamable-http",
			"headers":             map[string]any{"Authorization": "Bearer " + in.MCPToken},
			"requestTimeoutMs":    600000,
			"connectionTimeoutMs": 5000,
		},
		"agents.ownership": "explicit",
		// Explicit ownership requires at least one configured agent [probed]; "main" is
		// the gateway's own default and is hidden from the platform UI.
		"agents.entries.main":              map[string]any{"name": "Gateway default"},
		"agents.defaults.workspace":        in.StateDir + "/workspace",
		"agents.defaults.model":            map[string]any{"primary": ModelRef(in.DefaultModel)},
		"agents.defaults.thinkingDefault":  "medium",
		"agents.defaults.reasoningDefault": "off",
		"agents.defaults.userTimezone":     "UTC",
		"agents.defaults.sandbox": map[string]any{
			"mode": "all",
			// One container per session key: every run visit (yuno-<run>-<node>-<n>) starts
			// a fresh one, so nothing installed in a container reaches another run. The
			// agent's workspace is not mounted: each session gets a private workspace under
			// <state>/sandboxes/, mounted at /workspace and seeded with the bootstrap files
			// and skills, so no run or step sees another's files (DESIGN 5.30 rule 1, D-083).
			"scope":           "session",
			"workspaceAccess": "none",
			// Session containers pile up: idle ones go after an hour, any after a day
			// (agents.defaults.sandbox.prune: idleHours, maxAgeDays; gateway/config-agents/
			// sandbox.md). agents.delete leaves a container behind [probed]; the gateway
			// prunes it here too, and `make prune-sandboxes` is the manual path.
			"prune": map[string]any{"idleHours": 1, "maxAgeDays": 1},
			"docker": map[string]any{
				"image":        in.SandboxImage,
				"network":      "none",
				"readOnlyRoot": true,
				"capDrop":      []any{"ALL"},
				"pidsLimit":    256,
				"memory":       "1g",
				"cpus":         1,
				"tmpfs":        []any{"/tmp"},
				"user":         "1000:1000",
			},
		},
		"tools.profile": "coding",
		// skill_workshop cannot run in a sandboxed session [probed: doctor warning].
		"tools.deny":                    []any{"group:web", "group:ui", "group:messaging", "group:nodes", "group:automation", "sessions_spawn", "subagents", "skill_workshop"},
		"tools.exec.host":               "sandbox",
		"tools.sandbox.tools.alsoAllow": []any{"bundle-mcp"},
		// Tool Search hides MCP tools behind tool_search/tool_call meta-tools; a direct
		// call to yuno__finish then fails with "Tool yuno__finish not found" [probed].
		// Platform agents have small allowlists, so every tool is exposed directly.
		"tools.toolSearch":        false,
		"channels.telegram":       telegram,
		"commands.ownerAllowFrom": ids,
	}
}

// BaselineVersion is folded into the inputs hash; bump it whenever the managed keys
// change so an existing install is rewritten at the next start.
const BaselineVersion = 4

// Telegram DM policies the platform switches between (DESIGN 5.8 rule 7): closed while
// no agent is bound, the allowlist once one is. The gateway reads dmPolicy per message
// (config.schema.lookup reloadKind "none") [probed].
const (
	TelegramDMClosed = "disabled"
	TelegramDMOpen   = "allowlist"
)

// MinWebhookSecretLen is the shortest TELEGRAM_WEBHOOK_SECRET webhook mode accepts.
const MinWebhookSecretLen = 32

// ValidateTelegram refuses a webhook mode the gateway could not verify (DESIGN 5.8 rule
// 2): Telegram only delivers to an https URL, and the secret token it echoes in every
// request is the only proof a request came from Telegram. The error names each missing
// piece.
func ValidateTelegram(in BaselineInputs) error {
	if in.TelegramMode != "webhook" {
		return nil
	}
	var errs []error
	if !strings.HasPrefix(in.PublicBaseURL, "https://") || len(in.PublicBaseURL) == len("https://") {
		errs = append(errs, errors.New("TELEGRAM_MODE=webhook needs PUBLIC_BASE_URL starting with https:// (make tunnel prints one)"))
	}
	if len(in.TelegramWebhookSecret) < MinWebhookSecretLen {
		errs = append(errs, fmt.Errorf("TELEGRAM_MODE=webhook needs TELEGRAM_WEBHOOK_SECRET of at least %d characters", MinWebhookSecretLen))
	}
	return errors.Join(errs...)
}

// InputsHash is a stable digest of the inputs, the baseline version and the managed
// keys the baseline writes, so a change to the baseline's content reaches an existing
// gateway even when nobody bumped BaselineVersion (encoding/json sorts map keys).
func InputsHash(in BaselineInputs) string {
	b, _ := json.Marshal(struct {
		Version  int
		Inputs   BaselineInputs
		Baseline map[string]any
	}{BaselineVersion, in, Baseline(in)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Merge sets every managed dotted path from base into existing (creating objects as
// needed) and returns the result. Nothing outside the managed paths is touched.
func Merge(existing map[string]any, base map[string]any) map[string]any {
	if existing == nil {
		existing = map[string]any{}
	}
	keys := make([]string, 0, len(base))
	for k := range base {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		setPath(existing, strings.Split(k, "."), base[k])
	}
	return existing
}

func setPath(m map[string]any, path []string, v any) {
	if len(path) == 1 {
		m[path[0]] = v
		return
	}
	child, ok := m[path[0]].(map[string]any)
	if !ok {
		child = map[string]any{}
		m[path[0]] = child
	}
	setPath(child, path[1:], v)
}

// HashFile is the sidecar next to openclaw.json that records the inputs hash of the
// last baseline written. OpenClaw's schema rejects unknown keys under meta [probed], so
// the marker cannot live inside the config.
const HashFile = "yuno-baseline.hash"

// WriteBaseline reads <stateDir>/openclaw.json if present, merges the managed keys and
// writes the file atomically with mode 0600. It returns changed=false when the sidecar
// hash already matches the inputs.
func WriteBaseline(stateDir string, in BaselineInputs) (changed bool, err error) {
	if in.GatewayToken == "" || in.MCPToken == "" {
		return false, errors.New("OPENCLAW_GATEWAY_TOKEN and YUNO_MCP_TOKEN are required")
	}
	if err := ValidateTelegram(in); err != nil {
		return false, err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return false, err
	}
	path := filepath.Join(stateDir, "openclaw.json")
	hashPath := filepath.Join(stateDir, HashFile)
	hash := InputsHash(in)
	if prev, err := os.ReadFile(hashPath); err == nil && strings.TrimSpace(string(prev)) == hash {
		if _, err := os.Stat(path); err == nil {
			return false, nil
		}
	}
	existing := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, &existing); err != nil {
			return false, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	merged := Merge(existing, Baseline(in))
	// Earlier platform builds wrote marker keys under meta, which the schema rejects.
	if meta, ok := merged["meta"].(map[string]any); ok {
		delete(meta, "managedBy")
		delete(meta, "yunoInputsHash")
	}
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, err
	}
	if err := os.WriteFile(hashPath, []byte(hash+"\n"), 0o600); err != nil {
		return false, err
	}
	return true, nil
}
