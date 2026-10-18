package project

import (
	"testing"

	"yuno/internal/store"
)

// T5.23.10: IssueURL for a GitLab and a GitHub repository URL, with and without ".git"
// (DESIGN 5.23 rule 3): "/-/issues/<n>" on GitLab, "/issues/<n>" on GitHub, built from
// the run's snapshot of the project settings, so no table is needed.
func TestIssueURL(t *testing.T) {
	for _, c := range []struct {
		name, provider, repo string
		n                    int64
		want                 string
	}{
		{"gitlab", "gitlab", "https://gitlab.test/group/demo", 12, "https://gitlab.test/group/demo/-/issues/12"},
		{"gitlab with .git", "gitlab", "https://gitlab.test/group/demo.git", 12, "https://gitlab.test/group/demo/-/issues/12"},
		{"gitlab subgroup", "gitlab", "https://gitlab.test/group/sub/demo.git", 3, "https://gitlab.test/group/sub/demo/-/issues/3"},
		{"gitlab trailing slash", "gitlab", "https://gitlab.test/group/demo/", 1, "https://gitlab.test/group/demo/-/issues/1"},
		{"gitlab .git and slash", "gitlab", "https://gitlab.test/group/demo.git/", 1, "https://gitlab.test/group/demo/-/issues/1"},
		{"github", "github", "https://github.com/owner/demo", 7, "https://github.com/owner/demo/issues/7"},
		{"github with .git", "github", "https://github.com/owner/demo.git", 7, "https://github.com/owner/demo/issues/7"},
		{"github enterprise", "github", "https://ghe.test/owner/demo.git", 40, "https://ghe.test/owner/demo/issues/40"},
		// A snapshot with no provider (a run from before providers were recorded) is
		// GitLab's shape, as the rest of the platform reads it.
		{"no provider", "", "https://gitlab.test/group/demo.git", 5, "https://gitlab.test/group/demo/-/issues/5"},
	} {
		s := store.ProjectSettings{RepoURL: c.repo, Provider: c.provider}
		if got := IssueURL(s, c.n); got != c.want {
			t.Errorf("%s: IssueURL = %q, want %q", c.name, got, c.want)
		}
	}
}
