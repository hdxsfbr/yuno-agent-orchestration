// Package project is a project repository (DESIGN 5.16, 5.17): the settings of a
// record (bootstrapped once from the environment), the token sealed at rest, the
// toolchain read from a checkout, the text that tells an agent where the checkout is,
// and the git operations the api runs on the platform side with the token no agent
// ever sees.
package project

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"unicode"

	"yuno/internal/config"
	"yuno/internal/store"
)

// Provider is where the repository lives and who opens the merge request (rule 11).
type Provider string

const (
	ProviderGitLab Provider = "gitlab"
	ProviderGitHub Provider = "github"
)

// Config is a configured project (DESIGN 5.16 rule 1). Token is write-only: it goes
// into git's environment and the provider's request header, never into a file, a task,
// a transcript or a log line.
type Config struct {
	RepoURL        string
	Token          string
	DefaultBranch  string
	Provider       Provider
	SandboxNetwork bool
}

// ErrNotConfigured is Parse's answer when no repository URL is set.
var ErrNotConfigured = errors.New("no project repository configured")

// EvalForgeEnv names the setting that lets an eval project use the fake forge (DESIGN 5.29
// rule 17): the forge's base URL, set only for the eval profile.
const EvalForgeEnv = "EVAL_FORGE_URL"

// evalForgeHost is the host (with port) of EVAL_FORGE_URL when it is a plain http URL, ""
// otherwise. It is the one host a clone URL may reach over http; nothing else is allowed
// to, and an unset variable allows none.
func evalForgeHost() string {
	u, err := url.Parse(strings.TrimSpace(os.Getenv(EvalForgeEnv)))
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Host)
}

// ValidateRepoURL checks a clone URL (rule 1): https, no user information, a repository
// path. It returns the trimmed URL; the error says what to fix, without a field name. The
// one exception is http to exactly the host EVAL_FORGE_URL names (the eval profile's fake
// forge, DESIGN 5.29 rule 17).
func ValidateRepoURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	evalForge := u.Scheme == "http" && u.Host != "" && strings.ToLower(u.Host) == evalForgeHost()
	if (u.Scheme != "https" && !evalForge) || u.Host == "" {
		return "", errors.New("must be an https URL")
	}
	if u.User != nil {
		return "", errors.New("must not carry credentials")
	}
	if strings.Trim(u.Path, "/") == "" {
		return "", errors.New("names no repository path")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		// A query could carry a token, and it would be served and shown everywhere.
		return "", errors.New("must not carry a query or a fragment")
	}
	return raw, nil
}

// InferProvider is the provider a host implies (rule 1): github for github.com, gitlab
// for any other host.
func InferProvider(repoURL string) Provider {
	u, err := url.Parse(strings.TrimSpace(repoURL))
	if err == nil && strings.EqualFold(u.Hostname(), "github.com") {
		return ProviderGitHub
	}
	return ProviderGitLab
}

// ParseProvider accepts gitlab or github in any case, and "" (meaning: infer).
func ParseProvider(s string) (Provider, error) {
	p := Provider(strings.ToLower(strings.TrimSpace(s)))
	switch p {
	case "", ProviderGitLab, ProviderGitHub:
		return p, nil
	}
	return "", fmt.Errorf("must be gitlab or github, not %q", strings.TrimSpace(s))
}

// ValidateBranch checks a branch name (rule 1); "" is main.
func ValidateBranch(branch string) (string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "main", nil
	}
	if strings.ContainsAny(branch, " \t\n~^:?*[\\") || strings.HasPrefix(branch, "-") || strings.Contains(branch, "..") ||
		strings.ContainsFunc(branch, unicode.IsControl) {
		return "", fmt.Errorf("%q is not a branch name", branch)
	}
	return branch, nil
}

// Parse validates the PROJECT_* settings (rule 1): an https clone URL without user
// information, a token, and a provider that is one of the two known, inferred from the
// host when not set. The platform runs without a project when it returns an error; the
// error says what to fix.
func Parse(repoURL, token, defaultBranch, provider string, sandboxNetwork bool) (Config, error) {
	if strings.TrimSpace(repoURL) == "" {
		return Config{}, ErrNotConfigured
	}
	repoURL, err := ValidateRepoURL(repoURL)
	if err != nil {
		var ue *url.Error
		switch {
		case errors.As(err, &ue):
			// url.Error prints the raw URL, which may carry credentials: never logged.
			return Config{}, errors.New("PROJECT_REPO_URL is not a valid URL")
		case strings.Contains(err.Error(), "credentials"):
			return Config{}, errors.New("PROJECT_REPO_URL must not carry credentials; put the token in PROJECT_GIT_TOKEN")
		}
		return Config{}, fmt.Errorf("PROJECT_REPO_URL %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return Config{}, errors.New("PROJECT_GIT_TOKEN is required with PROJECT_REPO_URL")
	}
	p, err := ParseProvider(provider)
	if err != nil {
		return Config{}, fmt.Errorf("PROJECT_PROVIDER %w", err)
	}
	if p == "" {
		p = InferProvider(repoURL)
	}
	branch, err := ValidateBranch(defaultBranch)
	if err != nil {
		return Config{}, fmt.Errorf("PROJECT_DEFAULT_BRANCH %w", err)
	}
	return Config{RepoURL: repoURL, Token: strings.TrimSpace(token), DefaultBranch: branch, Provider: p, SandboxNetwork: sandboxNetwork}, nil
}

// ConfigOf is the configuration of a project record (or a run's snapshot of one) with
// its opened token (DESIGN 5.17 rule 6).
func ConfigOf(s store.ProjectSettings, token string) Config {
	return Config{RepoURL: s.RepoURL, Token: token, DefaultBranch: s.DefaultBranch, Provider: Provider(s.Provider), SandboxNetwork: s.SandboxNetwork}
}

// FromConfig is Parse over the platform settings.
func FromConfig(cfg config.Config) (Config, error) {
	return Parse(cfg.ProjectRepoURL, cfg.ProjectGitToken, cfg.ProjectDefaultBranch, cfg.ProjectProvider, cfg.ProjectSandboxNetwork)
}

// Enabled reports whether a repository is configured.
func (c Config) Enabled() bool { return c.RepoURL != "" }

// Path is the repository path on the provider: the URL path without the leading slash
// and a trailing ".git" ("group/sub/project", "owner/repo").
func (c Config) Path() string {
	u, err := url.Parse(c.RepoURL)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
}

// Host is the repository's host, for the UI and log lines.
func (c Config) Host() string {
	u, err := url.Parse(c.RepoURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// APIBase is the provider's REST base for this repository (rule 11): GitLab's
// /api/v4/projects/<encoded path> on the repository's host; GitHub's
// api.github.com/repos/<owner>/<repo>, or <host>/api/v3/repos/... for another host.
func (c Config) APIBase() string {
	u, err := url.Parse(c.RepoURL)
	if err != nil {
		return ""
	}
	switch c.Provider {
	case ProviderGitHub:
		if strings.EqualFold(u.Hostname(), "github.com") {
			return "https://api.github.com/repos/" + c.Path()
		}
		return u.Scheme + "://" + u.Host + "/api/v3/repos/" + c.Path()
	default:
		return u.Scheme + "://" + u.Host + "/api/v4/projects/" + url.PathEscape(c.Path())
	}
}

// authUser is the user name sent with the token over HTTP basic authentication: the
// providers accept a token as the password of a fixed user.
func (c Config) authUser() string {
	if c.Provider == ProviderGitHub {
		return "x-access-token"
	}
	return "oauth2"
}

// RequestName is what the provider calls the thing the tool opens, for messages.
func (c Config) RequestName() string {
	if c.Provider == ProviderGitHub {
		return "pull request"
	}
	return "merge request"
}
