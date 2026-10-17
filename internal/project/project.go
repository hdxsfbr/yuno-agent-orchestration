// Package project is the platform's one project repository (DESIGN 5.16): its
// configuration from the environment, the toolchain read from a checkout, the text that
// tells an agent where the checkout is, and the git operations the api runs on the
// platform side with the token no agent ever sees.
package project

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"yuno/internal/config"
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

// Parse validates the PROJECT_* settings (rule 1): an https clone URL without user
// information, a token, and a provider that is one of the two known, inferred from the
// host when not set. The platform runs without a project when it returns an error; the
// error says what to fix.
func Parse(repoURL, token, defaultBranch, provider string, sandboxNetwork bool) (Config, error) {
	repoURL = strings.TrimSpace(repoURL)
	if repoURL == "" {
		return Config{}, ErrNotConfigured
	}
	u, err := url.Parse(repoURL)
	if err != nil {
		return Config{}, fmt.Errorf("PROJECT_REPO_URL: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return Config{}, errors.New("PROJECT_REPO_URL must be an https URL")
	}
	if u.User != nil {
		return Config{}, errors.New("PROJECT_REPO_URL must not carry credentials; put the token in PROJECT_GIT_TOKEN")
	}
	if strings.Trim(u.Path, "/") == "" {
		return Config{}, errors.New("PROJECT_REPO_URL names no repository path")
	}
	if strings.TrimSpace(token) == "" {
		return Config{}, errors.New("PROJECT_GIT_TOKEN is required with PROJECT_REPO_URL")
	}
	p := Provider(strings.ToLower(strings.TrimSpace(provider)))
	switch p {
	case "":
		p = ProviderGitLab
		if strings.EqualFold(u.Hostname(), "github.com") {
			p = ProviderGitHub
		}
	case ProviderGitLab, ProviderGitHub:
	default:
		return Config{}, fmt.Errorf("PROJECT_PROVIDER must be gitlab or github, not %q", provider)
	}
	branch := strings.TrimSpace(defaultBranch)
	if branch == "" {
		branch = "main"
	}
	if strings.ContainsAny(branch, " \t\n~^:?*[\\") || strings.HasPrefix(branch, "-") {
		return Config{}, fmt.Errorf("PROJECT_DEFAULT_BRANCH %q is not a branch name", branch)
	}
	return Config{RepoURL: repoURL, Token: strings.TrimSpace(token), DefaultBranch: branch, Provider: p, SandboxNetwork: sandboxNetwork}, nil
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
