package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"yuno/internal/obs"
	"yuno/internal/project"
)

// ProjectCheckTimeout bounds the whole check, both requests (DESIGN 5.17 rule 5).
const ProjectCheckTimeout = 10 * time.Second

// CheckResult is the project check's answer (the ProjectCheck schema, DESIGN 5.17 rule
// 5). OK means the token was accepted and the repository found; Message is one
// sentence for the operator. BranchExists and CanPush are nil when the provider was
// not asked or did not say.
type CheckResult struct {
	OK            bool      `json:"ok"`
	Message       string    `json:"message"`
	DefaultBranch string    `json:"default_branch,omitempty"`
	BranchExists  *bool     `json:"branch_exists,omitempty"`
	CanPush       *bool     `json:"can_push,omitempty"`
	CheckedAt     time.Time `json:"checked_at"`
}

// CheckProject verifies a project's token against its provider without a clone
// (DESIGN 5.17 rule 5): GitLab GET <base> (the repository's default_branch) then GET
// <base>/repository/branches/<branch> (can_push); GitHub GET <base> (default_branch,
// permissions.push) then GET <base>/branches/<branch> (existence). <branch> is the
// project's default branch. Provider refusals, errors and the timeout are results,
// never errors, and the token never appears in the message. apiBase "" is
// pc.APIBase() (tests point it at a fake); a nil client is the default one.
func CheckProject(ctx context.Context, client *http.Client, pc project.Config, apiBase string) CheckResult {
	if client == nil {
		client = ProviderClient(nil)
	}
	if apiBase == "" {
		apiBase = pc.APIBase()
	}
	ctx, cancel := context.WithTimeout(ctx, ProjectCheckTimeout)
	defer cancel()
	c := &projectCheck{pc: pc, base: strings.TrimRight(apiBase, "/")}
	var res CheckResult
	switch pc.Provider {
	case project.ProviderGitHub:
		c.name = "GitHub"
		c.api = &apiClient{what: "github", client: client, authorize: bearerAuth(pc.Token)}
		res = c.github(ctx)
	default:
		c.name = "GitLab"
		c.api = &apiClient{what: "gitlab", client: client, authorize: privateTokenAuth(pc.Token)}
		res = c.gitlab(ctx)
	}
	res.Message = c.scrub(res.Message)
	res.CheckedAt = time.Now().UTC()
	return res
}

type projectCheck struct {
	pc   project.Config
	base string
	name string // the provider as the operator knows it
	api  *apiClient
}

// gitlabDeveloper is the access level from which GitLab lets a token push branches
// (Developer); the platform pushes its own branch and never the default one, so the
// default branch's protection does not decide.
const gitlabDeveloper = 30

func (c *projectCheck) gitlab(ctx context.Context) CheckResult {
	type access struct {
		AccessLevel int `json:"access_level"`
	}
	var repo struct {
		DefaultBranch string `json:"default_branch"`
		Permissions   struct {
			ProjectAccess *access `json:"project_access"`
			GroupAccess   *access `json:"group_access"`
		} `json:"permissions"`
	}
	if status, err := c.api.do(ctx, http.MethodGet, c.base, nil, &repo); err != nil {
		return c.repoFailure(ctx, status, err)
	}
	res := CheckResult{OK: true, DefaultBranch: repo.DefaultBranch}
	// The token's access level on the project, or the group's, says whether it can push
	// a branch; a project access token reports it under project_access.
	level := -1
	for _, a := range []*access{repo.Permissions.ProjectAccess, repo.Permissions.GroupAccess} {
		if a != nil {
			level = max(level, a.AccessLevel)
		}
	}
	if level >= 0 {
		res.CanPush = ptr(level >= gitlabDeveloper)
	}
	var branch struct {
		CanPush bool `json:"can_push"`
	}
	status, err := c.api.do(ctx, http.MethodGet, c.base+"/repository/branches/"+url.PathEscape(c.pc.DefaultBranch), nil, &branch)
	if err != nil {
		return c.branchFailure(ctx, res, status, err)
	}
	res.BranchExists = ptr(true)
	if res.CanPush == nil {
		// No access level reported (an older GitLab): the branch's own flag stands in.
		res.CanPush = ptr(branch.CanPush)
	}
	res.Message = c.accepted(*res.CanPush)
	return res
}

func (c *projectCheck) github(ctx context.Context) CheckResult {
	var repo struct {
		DefaultBranch string `json:"default_branch"`
		Permissions   *struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if status, err := c.api.do(ctx, http.MethodGet, c.base, nil, &repo); err != nil {
		return c.repoFailure(ctx, status, err)
	}
	res := CheckResult{OK: true, DefaultBranch: repo.DefaultBranch}
	if repo.Permissions != nil {
		res.CanPush = ptr(repo.Permissions.Push)
	}
	status, err := c.api.do(ctx, http.MethodGet, c.base+"/branches/"+url.PathEscape(c.pc.DefaultBranch), nil, nil)
	if err != nil {
		return c.branchFailure(ctx, res, status, err)
	}
	res.BranchExists = ptr(true)
	if res.CanPush == nil {
		res.Message = fmt.Sprintf("Token accepted: default branch %s; %s does not say whether the token can push.", c.pc.DefaultBranch, c.name)
		return res
	}
	res.Message = c.accepted(*res.CanPush)
	return res
}

// accepted is the sentence for a repository found and a branch that exists.
func (c *projectCheck) accepted(canPush bool) string {
	if canPush {
		return fmt.Sprintf("Token accepted: default branch %s, push allowed.", c.pc.DefaultBranch)
	}
	return "Token accepted, but it cannot push to the repository; it needs write access."
}

// repoFailure is the result when the repository request failed: nothing was accepted.
func (c *projectCheck) repoFailure(ctx context.Context, status int, err error) CheckResult {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return CheckResult{Message: fmt.Sprintf("The token was refused (%d).", status)}
	case http.StatusNotFound:
		return CheckResult{Message: "Repository not found (404): check the URL and the token's scope."}
	}
	return CheckResult{Message: c.failure(ctx, status, err) + "."}
}

// branchFailure is the result when the repository was found but the branch request
// failed: a 404 is a missing branch (still ok: the token was accepted and the
// repository found), a refusal is a refused token. GitLab's can_push comes with the
// branch, so it stays unknown; GitHub's came with the repository.
func (c *projectCheck) branchFailure(ctx context.Context, res CheckResult, status int, err error) CheckResult {
	branch := c.pc.DefaultBranch
	switch status {
	case http.StatusNotFound:
		res.BranchExists = ptr(false)
		if res.DefaultBranch == "" || res.DefaultBranch == branch {
			// GitHub names a default branch for an empty repository too.
			res.Message = fmt.Sprintf("Token accepted, but branch %s does not exist; the repository has no commits yet.", branch)
		} else {
			res.Message = fmt.Sprintf("Token accepted, but branch %s does not exist; the repository's default branch is %s.", branch, res.DefaultBranch)
		}
		return res
	case http.StatusUnauthorized, http.StatusForbidden:
		return CheckResult{DefaultBranch: res.DefaultBranch, Message: fmt.Sprintf("The token was refused (%d) when reading branch %s.", status, branch)}
	}
	res.Message = fmt.Sprintf("Token accepted, but branch %s could not be checked: %s.", branch, c.failure(ctx, status, err))
	return res
}

// failure describes a failed request other than a refusal or a 404, without a final
// period: the timeout, a transport error, an answer that is not JSON, another status.
func (c *projectCheck) failure(ctx context.Context, status int, err error) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		// Whether the headers or the body stalled.
		return fmt.Sprintf("%s did not answer within %d s", c.name, int(ProjectCheckTimeout/time.Second))
	case status == 0:
		return fmt.Sprintf("could not reach %s: %s", c.name, c.netError(err))
	case status >= 200 && status <= 299:
		return fmt.Sprintf("%s's answer is not JSON; check the repository URL and the provider", c.name)
	}
	return fmt.Sprintf("%s answered %d", c.name, status)
}

// netError is the short reason of a transport failure: the apiClient's text without
// its "gitlab: " prefix and without the request line Go puts in front.
func (c *projectCheck) netError(err error) string {
	msg := strings.TrimPrefix(err.Error(), c.api.what+": ")
	if strings.HasPrefix(msg, `Get "`) {
		if i := strings.Index(msg, `": `); i > 0 {
			msg = msg[i+3:]
		}
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return strings.TrimRight(msg, ". ")
}

// scrub keeps the token out of the message (DESIGN 5.17 rule 5) whatever went into it.
func (c *projectCheck) scrub(msg string) string {
	if msg != "" {
		msg = strings.ToUpper(msg[:1]) + msg[1:]
	}
	if c.pc.Token != "" {
		msg = strings.ReplaceAll(msg, c.pc.Token, "[redacted]")
	}
	return obs.Redact(msg)
}
