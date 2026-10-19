package httpapi

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// The Chief's add_project (D-081) creates a project through createProjectFrom, the code
// POST /api/projects runs, so the URL, name, budget and intake rules are the same. The
// token is chosen here, on the server, and is not part of the tool's input: that of a
// live project in the same repository namespace (host, provider and owner or group path),
// else PROJECT_GIT_TOKEN. A token is never lent across namespaces (M4).

const (
	chatDefaultBudgetUSD = 10
	// chatIntakeTemplate names the copy intake runs by default (the Wave pipeline).
	chatIntakeTemplate = "wave_pipeline"
)

func (s *Server) chatAddProject(ctx context.Context, req tools.AddProjectRequest) (*tools.AddProjectResult, error) {
	in := projectInput{Name: req.Name, RepoURL: req.RepoURL, Provider: req.Provider, DefaultBranch: req.DefaultBranch,
		MaxCostUSDPerDay: req.BudgetUSDPerDay}
	if strings.TrimSpace(in.Name) == "" {
		in.Name = repoNameOf(req.RepoURL)
	}
	// Validate the URL and provider first, so the token search names the host it uses.
	if _, _, fe := in.validate(false); hasFieldError(fe, "repo_url", "provider") {
		return nil, modelFieldErrors(fe)
	}
	if s.cipher == nil {
		return nil, &tools.ModelError{Message: "project tokens are not available on this server (PROJECT_TOKEN_KEY is not set)"}
	}
	token, from, err := s.tokenForRepo(ctx, &in)
	if err != nil {
		return nil, err
	}
	in.Token = token
	res := &tools.AddProjectResult{TokenFrom: from, TokenSrc: "config"}
	if from != "" {
		res.TokenSrc = "project"
	}
	if req.Intake != nil {
		if in.MaxCostUSDPerDay == nil {
			b := float64(chatDefaultBudgetUSD)
			in.MaxCostUSDPerDay = &b
		}
		label := strings.TrimSpace(req.Intake.Label)
		if label == "" {
			label = store.DefaultIntakeLabel
		}
		wf, err := s.intakeWorkflowFor(ctx, req.Intake.Workflow)
		if err != nil {
			return nil, err
		}
		in.Intake = &intakeInput{Enabled: true, Label: &label, WorkflowID: wf.ID}
		res.IntakeWorkflow = wf.Name
	}
	created, fe, err := s.createProjectFrom(ctx, &in)
	switch {
	case errors.Is(err, errTokensUnavailable):
		return nil, &tools.ModelError{Message: "project tokens are not available on this server (PROJECT_TOKEN_KEY is not set)"}
	case err != nil:
		return nil, err
	case len(fe) > 0:
		return nil, modelFieldErrors(fe)
	}
	res.ProjectID, res.Name = created.ID, created.Name
	res.IntakeEnabled, res.IntakeLabel = created.IntakeEnabled, ""
	if created.IntakeEnabled {
		res.IntakeLabel = created.IntakeLabel
	}
	res.BudgetUSDDay = in.MaxCostUSDPerDay
	return res, nil
}

// tokenForRepo picks the token for a new project: the one of a live project on the same
// host and provider whose repository namespace (owner, or group path with subgroups)
// equals the new repository's (its name is returned), else PROJECT_GIT_TOKEN (name ""), else the
// model's error that sends the person to the Projects page.
func (s *Server) tokenForRepo(ctx context.Context, in *projectInput) (token, from string, err error) {
	provider := in.Provider
	if p, perr := project.ParseProvider(provider); perr == nil && p == "" {
		provider = string(project.InferProvider(in.RepoURL))
	} else {
		provider = string(p)
	}
	host := hostOf(in.RepoURL)
	ns := namespaceOf(in.RepoURL)
	projects, err := store.ListProjects(ctx, s.pool)
	if err != nil {
		return "", "", err
	}
	for _, p := range projects {
		if p.Provider != provider || hostOf(p.RepoURL) != host || ns == "" || namespaceOf(p.RepoURL) != ns {
			continue
		}
		tok, terr := s.projects.Token(ctx, p.ID)
		if terr != nil || strings.TrimSpace(tok) == "" {
			continue // no token stored, or one that does not open: try the next
		}
		return tok, p.Name, nil
	}
	if t := strings.TrimSpace(s.cfg.ProjectGitToken); t != "" {
		return t, "", nil
	}
	return "", "", &tools.ModelError{Message: "no token is known for " + host + " (" + provider + "): no project in the same group or owner has one " +
		"and none is configured. Tell the person to add this project once on the Projects page, where the token is entered; " +
		"never ask for a token in chat"}
}

// intakeWorkflowFor finds the workflow intake runs: the live one called name (ignoring
// case), or the copy of the Wave pipeline template when name is empty.
func (s *Server) intakeWorkflowFor(ctx context.Context, name string) (*store.Workflow, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		wf, err := store.GetWorkflowByFromTemplate(ctx, s.pool, chatIntakeTemplate)
		if errors.Is(err, store.ErrNotFound) {
			return nil, &tools.ModelError{Message: "the Wave pipeline workflow does not exist; name another workflow for intake"}
		}
		return wf, err
	}
	all, err := store.ListWorkflows(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if strings.EqualFold(strings.TrimSpace(all[i].Name), name) {
			return &all[i], nil
		}
	}
	return nil, &tools.ModelError{Message: "no workflow named " + name + "; list_workflows shows the names"}
}

// namespaceOf is the lowercased owner (GitHub) or group path (GitLab, subgroups included)
// of a repository URL: its path without the last segment. "" when it has none.
func namespaceOf(repoURL string) string {
	u, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil {
		return ""
	}
	path := strings.Trim(strings.TrimSuffix(strings.TrimRight(u.Path, "/"), ".git"), "/")
	i := strings.LastIndex(path, "/")
	if i <= 0 {
		return ""
	}
	return strings.ToLower(path[:i])
}

// repoNameOf is the repository's name from its URL, "" when it has none.
func repoNameOf(repoURL string) string {
	u, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.TrimRight(u.Path[strings.LastIndex(u.Path, "/")+1:], "/"), ".git")
}

func hasFieldError(fe fieldErrors, paths ...string) bool {
	for _, e := range fe {
		for _, p := range paths {
			if e.Path == p {
				return true
			}
		}
	}
	return false
}

// modelFieldErrors is a project's validation failures as the sentence the model reads.
func modelFieldErrors(fe fieldErrors) error {
	parts := make([]string, 0, len(fe))
	for _, e := range fe {
		parts = append(parts, e.Path+": "+e.Message)
	}
	return &tools.ModelError{Message: "project not added; " + strings.Join(parts, "; ")}
}
