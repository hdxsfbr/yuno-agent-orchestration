// Package config reads every setting from the environment with documented defaults.
// Names match .env.example one to one.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Version is set at build time with -ldflags "-X yuno/internal/config.Version=...".
var Version = "dev"

type Config struct {
	// Persistence and services.
	DatabaseURL         string
	ListenAddr          string
	DeployTargetURL     string
	DeployTargetListen  string
	DeployTargetDataDir string
	DeployTargetMaxMB   int
	PublicBaseURL       string
	WebPort             int

	// OpenClaw gateway (DESIGN 5.14).
	OpenClawGatewayURL   string // ws://127.0.0.1:18789, loopback with the gateway
	OpenClawGatewayToken string
	OpenClawStateDir     string // /home/node/.openclaw, bind-mounted in api and openclaw
	OpenClawSandboxImage string
	APIBaseForOpenClaw   string // how the gateway reaches the api: http://127.0.0.1:8080
	MCPToken             string
	MCPToolWait          time.Duration

	// Model provider and proxy. ModelUpstream selects where the proxy forwards: "anthropic"
	// (x-api-key auth, Anthropic model ids) or "openrouter" (bearer auth, model ids
	// rewritten to OpenRouter's "anthropic/claude-x.y" form; OpenRouter serves the
	// Anthropic messages format at <OpenRouterBaseURL>/v1/messages).
	ModelUpstream           string
	AnthropicAPIKey         string
	AnthropicBaseURL        string
	OpenRouterAPIKey        string
	OpenRouterBaseURL       string
	DefaultModel            string
	ModelProxyRatePerMinute int

	// Telegram through OpenClaw's native channel (DESIGN 5.8).
	TelegramMode           string // off | polling | webhook
	TelegramWebhookSecret  string
	TelegramAllowedUserIDs []int64

	// Optional GitLab merge-request tool.
	GitLabBaseURL   string
	GitLabProjectID string
	GitLabToken     string

	// Project repository bootstrap (DESIGN 5.16 rule 1, 5.17 rule 9): at the first start
	// these become the first project record; projects are then edited in the UI. The
	// token is write-only: never returned by the API, redacted wherever it could appear.
	ProjectRepoURL        string
	ProjectGitToken       string
	ProjectDefaultBranch  string
	ProjectProvider       string // gitlab | github; "" means by host (github.com → github)
	ProjectSandboxNetwork bool
	// ProjectTokenKey encrypts every project's git token at rest (DESIGN 5.17 rule 2);
	// generated into .env by scripts/ensure-env.sh. Empty: no token can be stored.
	ProjectTokenKey string

	// API and engine.
	APIToken           string
	EngineWorkers      int
	EngineStaleAfter   time.Duration
	SchedulerTick      time.Duration
	HTTPFetchAllowlist []string
	ToolOutputMaxBytes int
	LogLevel           string

	// Evaluation runner (DESIGN 5.13). The runner is an HTTP client of the api.
	EvalAPIURL     string
	EvalProvider   string // scripted | anthropic
	EvalSplit      string // public | private | all
	EvalCasesDir   string
	EvalReportsDir string

	// Platform maximums that clamp per-workflow limits (DESIGN 5.2).
	MaxSteps              int
	MaxDurationSeconds    int
	MaxNodeIterations     int
	MaxLoopVisits         int
	MaxCostUSD            float64
	MaxToolCalls          int
	ApprovalTimeoutMaxSec int
}

func FromEnv() Config {
	return Config{
		DatabaseURL:         env("DATABASE_URL", "postgres://yuno:yuno@localhost:5432/yuno?sslmode=disable"),
		ListenAddr:          env("LISTEN_ADDR", ":8080"),
		DeployTargetURL:     env("DEPLOYTARGET_URL", "http://localhost:8091"),
		DeployTargetListen:  env("DEPLOYTARGET_LISTEN", ":8091"),
		DeployTargetDataDir: env("DEPLOYTARGET_DATA_DIR", "/tmp/yuno-releases"),
		DeployTargetMaxMB:   envInt("DEPLOYTARGET_MAX_MB", 100),
		PublicBaseURL:       strings.TrimRight(env("PUBLIC_BASE_URL", ""), "/"),
		WebPort:             envInt("WEB_PORT", 8080),

		OpenClawGatewayURL:   env("OPENCLAW_GATEWAY_URL", "ws://127.0.0.1:18789"),
		OpenClawGatewayToken: env("OPENCLAW_GATEWAY_TOKEN", ""),
		OpenClawStateDir:     env("OPENCLAW_STATE_DIR", "/home/node/.openclaw"),
		OpenClawSandboxImage: env("OPENCLAW_SANDBOX_IMAGE", "yuno-sandbox:latest"),
		APIBaseForOpenClaw:   env("API_BASE_FOR_OPENCLAW", "http://127.0.0.1:8080"),
		MCPToken:             env("YUNO_MCP_TOKEN", ""),
		MCPToolWait:          time.Duration(envInt("MCP_TOOL_WAIT_SECONDS", 540)) * time.Second,

		ModelUpstream:           strings.ToLower(env("MODEL_UPSTREAM", "anthropic")),
		AnthropicAPIKey:         env("ANTHROPIC_API_KEY", ""),
		AnthropicBaseURL:        strings.TrimRight(env("ANTHROPIC_BASE_URL", "https://api.anthropic.com"), "/"),
		OpenRouterAPIKey:        env("OPENROUTER_API_KEY", ""),
		OpenRouterBaseURL:       strings.TrimRight(env("OPENROUTER_BASE_URL", "https://openrouter.ai/api"), "/"),
		DefaultModel:            env("DEFAULT_MODEL", "claude-opus-5-5"),
		ModelProxyRatePerMinute: envInt("MODEL_PROXY_RATE_PER_MINUTE", 60),

		TelegramMode:           env("TELEGRAM_MODE", "off"),
		TelegramWebhookSecret:  env("TELEGRAM_WEBHOOK_SECRET", ""),
		TelegramAllowedUserIDs: envInt64List("TELEGRAM_ALLOWED_USER_IDS"),

		GitLabBaseURL:   strings.TrimRight(env("GITLAB_BASE_URL", ""), "/"),
		GitLabProjectID: env("GITLAB_PROJECT_ID", ""),
		GitLabToken:     env("GITLAB_TOKEN", ""),

		ProjectRepoURL:        env("PROJECT_REPO_URL", ""),
		ProjectGitToken:       env("PROJECT_GIT_TOKEN", ""),
		ProjectDefaultBranch:  env("PROJECT_DEFAULT_BRANCH", "main"),
		ProjectProvider:       strings.ToLower(env("PROJECT_PROVIDER", "")),
		ProjectSandboxNetwork: envBool("PROJECT_SANDBOX_NETWORK", false),
		ProjectTokenKey:       env("PROJECT_TOKEN_KEY", ""),

		APIToken:           env("API_TOKEN", ""),
		EngineWorkers:      envInt("ENGINE_WORKERS", 4),
		EngineStaleAfter:   envDuration("ENGINE_STALE_AFTER", 300*time.Second),
		SchedulerTick:      envDuration("SCHEDULER_TICK", 10*time.Second),
		HTTPFetchAllowlist: envList("HTTP_FETCH_ALLOWLIST", "docs.python.org,pypi.org,raw.githubusercontent.com"),
		ToolOutputMaxBytes: envInt("TOOL_OUTPUT_MAX_BYTES", 32768),
		LogLevel:           env("LOG_LEVEL", "info"),

		EvalAPIURL:     strings.TrimRight(env("EVAL_API_URL", "http://127.0.0.1:8080"), "/"),
		EvalProvider:   env("EVAL_PROVIDER", "scripted"),
		EvalSplit:      env("EVAL_SPLIT", "public"),
		EvalCasesDir:   env("EVAL_CASES_DIR", "evals/cases"),
		EvalReportsDir: env("EVAL_REPORTS_DIR", "evals/reports"),

		MaxSteps:              envInt("MAX_STEPS", 200),
		MaxDurationSeconds:    envInt("MAX_DURATION_SECONDS", 3600),
		MaxNodeIterations:     envInt("MAX_NODE_ITERATIONS", 50),
		MaxLoopVisits:         envInt("MAX_LOOP_VISITS", 10),
		MaxCostUSD:            envFloat("MAX_COST_USD", 20),
		MaxToolCalls:          envInt("MAX_TOOL_CALLS", 300),
		ApprovalTimeoutMaxSec: envInt("APPROVAL_TIMEOUT_MAX_SECONDS", 604800),
	}
}

// GitLabEnabled reports whether the optional merge-request tool should be registered.
func (c Config) GitLabEnabled() bool {
	return c.GitLabBaseURL != "" && c.GitLabProjectID != "" && c.GitLabToken != ""
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(env(key, "")); err == nil {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, err := strconv.ParseBool(env(key, "")); err == nil {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(env(key, ""), 64); err == nil {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(env(key, "")); err == nil {
		return v
	}
	return def
}

func envList(key, def string) []string {
	raw := env(key, def)
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envInt64List(key string) []int64 {
	var out []int64
	for _, p := range envList(key, "") {
		if v, err := strconv.ParseInt(p, 10, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}
