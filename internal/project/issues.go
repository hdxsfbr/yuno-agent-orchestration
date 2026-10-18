package project

import (
	"fmt"
	"strings"

	"yuno/internal/store"
)

// IssueURL is the page of issue n of a project's repository (DESIGN 5.23 rule 3): the
// repository URL without a trailing ".git" and slash, then "/-/issues/<n>" on GitLab and
// "/issues/<n>" on GitHub. n is the GitLab issue iid or the GitHub issue number, the
// "#<n>" of the intake's texts; the run's trigger_ref makes the URL derivable without a
// table, for the UI, the comments and run_status.
//
// The task text of rule 4 (tools.IssueTask) is in the tools package: it needs the
// untrusted-content escape that lives there, and tools imports this package.
func IssueURL(s store.ProjectSettings, n int64) string {
	base := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(s.RepoURL), "/"), ".git")
	base = strings.TrimRight(base, "/")
	if Provider(s.Provider) == ProviderGitHub {
		return fmt.Sprintf("%s/issues/%d", base, n)
	}
	return fmt.Sprintf("%s/-/issues/%d", base, n)
}
