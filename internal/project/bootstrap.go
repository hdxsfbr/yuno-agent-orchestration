package project

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"yuno/internal/config"
	"yuno/internal/store"
)

// maxNameLen is the projects name rule (DESIGN 5.17 rule 1).
const maxNameLen = 64

// Bootstrap creates the project PROJECT_* describe when no project, live or deleted,
// has that clone URL (DESIGN 5.17 rule 9), once: it is named after the repository path's
// last segment, or the whole path when a live project already has that name. It returns
// the project (existing or created) and whether it created it; (nil, false, nil) when
// PROJECT_* is not set or the operator deleted the project. Nothing is updated on a
// later start: the Projects page is the truth.
func Bootstrap(ctx context.Context, q store.Querier, c *Cipher, cfg config.Config) (*store.Project, bool, error) {
	pc, err := FromConfig(cfg)
	if errors.Is(err, ErrNotConfigured) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if c == nil {
		return nil, false, ErrNoKey
	}
	existing, err := store.FindProjectByRepoURL(ctx, q, pc.RepoURL)
	if err == nil {
		if existing.DeletedAt != nil {
			return nil, false, nil // deleted on the page: not brought back
		}
		return existing, false, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, false, err
	}
	name, err := freeName(ctx, q, pc.Path())
	if err != nil {
		return nil, false, err
	}
	p := &store.Project{ID: store.NewID(), Name: name, RepoURL: pc.RepoURL, Provider: string(pc.Provider),
		DefaultBranch: pc.DefaultBranch, SandboxNetwork: pc.SandboxNetwork}
	blob, err := c.Seal(p.ID, pc.Token)
	if err != nil {
		return nil, false, err
	}
	if err := store.CreateProject(ctx, q, p, blob); err != nil {
		return nil, false, fmt.Errorf("create project %q: %w", name, err)
	}
	return p, true, nil
}

// freeName is the last segment of the repository path, or the whole path when a live
// project has that name already.
func freeName(ctx context.Context, q store.Querier, path string) (string, error) {
	path = strings.Trim(path, "/")
	last := path[strings.LastIndex(path, "/")+1:]
	for _, candidate := range []string{last, path} {
		candidate = cut(candidate, maxNameLen)
		if candidate == "" {
			continue
		}
		_, err := store.GetProjectByName(ctx, q, candidate)
		if errors.Is(err, store.ErrNotFound) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("a project named %q exists with another repository; rename it on the Projects page", cut(path, maxNameLen))
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
