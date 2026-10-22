package mergereq

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"yuno/internal/obs"
	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// The merge request reads of Chief platform control (DESIGN 5.34 rule 14): what the
// provider says about a request's diff, the checks of its head and a job's log, redacted
// and capped before they reach the operator or the Chief.

const (
	// MaxDiffBytes caps a diff: the paths and patches, after redaction (rule 14).
	MaxDiffBytes = 256 << 10
	// DefaultLogTail and MaxLogTail bound a job log's tail (rule 14).
	DefaultLogTail = 64 << 10
	MaxLogTail     = 1 << 20
	// ReadTimeout bounds one read of the provider.
	ReadTimeout = 30 * time.Second
)

// ErrHeadUnknown is the checks read of a record whose head commit is not known yet: it
// was never synced.
var ErrHeadUnknown = errors.New("the head commit of the request is not known yet; sync the request first")

// DiffRead is a request's diff as the read answers it: the files in the provider's order,
// Truncated when the cap or the provider's page limit left files or part of a patch out.
type DiffRead struct {
	Files     []tools.DiffFile `json:"files"`
	Truncated bool             `json:"truncated"`
}

// LogRead is a job log's tail as the read answers it: Size is the whole log's, in bytes;
// Truncated says Log is not all of it. A tail that starts mid-log starts at a line.
type LogRead struct {
	Job       string `json:"job"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	Log       string `json:"log"`
}

// Diff reads the record's files and patches from its provider (rule 14): redacted with
// obs.Redact and the project's token, at most MaxDiffBytes of paths and patches, then
// truncated. Errors as Checks.
func (s *Sync) Diff(ctx context.Context, rec *store.MergeRequest) (DiffRead, error) {
	pc, rp, err := s.reader(ctx, rec)
	if err != nil {
		return DiffRead{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	d, err := rp.Diff(ctx, rec.Number, MaxDiffBytes)
	if err != nil {
		return DiffRead{}, providerErr(err, pc.Token)
	}
	return capDiff(d, pc.Token, MaxDiffBytes), nil
}

// capDiff redacts each file and keeps files while their paths and patches fit in limit;
// the file that does not fit keeps the whole lines of its patch that do, and the read is
// truncated there.
func capDiff(d tools.Diff, token string, limit int) DiffRead {
	out := DiffRead{Files: []tools.DiffFile{}, Truncated: d.More}
	budget := limit
	for _, f := range d.Files {
		f.Path, f.OldPath, f.Patch = redact(f.Path, token), redact(f.OldPath, token), redact(f.Patch, token)
		cost := len(f.Path) + len(f.OldPath) + len(f.Patch)
		if cost > budget {
			if keep := budget - len(f.Path) - len(f.OldPath); keep > 0 {
				f.Patch = cutLines(f.Patch, keep)
				out.Files = append(out.Files, f)
			}
			out.Truncated = true
			break
		}
		budget -= cost
		out.Files = append(out.Files, f)
	}
	return out
}

// Checks reads the CI of exactly the record's head_sha with its jobs (rule 14). Errors:
// ErrHeadUnknown; the project resolver's; tools.ErrRequestNotFound; *ProviderError.
func (s *Sync) Checks(ctx context.Context, rec *store.MergeRequest) (tools.Checks, error) {
	if rec.HeadSHA == nil || *rec.HeadSHA == "" {
		return tools.Checks{}, ErrHeadUnknown
	}
	pc, rp, err := s.reader(ctx, rec)
	if err != nil {
		return tools.Checks{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	c, err := rp.Checks(ctx, rec.Number, *rec.HeadSHA)
	if err != nil {
		return tools.Checks{}, providerErr(err, pc.Token)
	}
	c.URL = redact(c.URL, pc.Token)
	for i := range c.Jobs {
		c.Jobs[i].Name, c.Jobs[i].URL = redact(c.Jobs[i].Name, pc.Token), redact(c.Jobs[i].URL, pc.Token)
	}
	return c, nil
}

// JobLog reads the last tail bytes of a job's log (rule 14): a tail that starts mid-log
// starts after its first line break, so no line, and no credential, is cut in two; then
// it is redacted and kept within tail bytes. Errors: tools.ErrJobNotFound; the project
// resolver's; *ProviderError.
func (s *Sync) JobLog(ctx context.Context, rec *store.MergeRequest, job string, tail int) (LogRead, error) {
	pc, rp, err := s.reader(ctx, rec)
	if err != nil {
		return LogRead{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	lg, err := rp.JobLog(ctx, job, tail)
	if err != nil {
		return LogRead{}, providerErr(err, pc.Token)
	}
	text := string(lg.Text)
	if lg.Truncated {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	text = redact(text, pc.Token)
	out := LogRead{Job: job, Size: lg.Size, Truncated: lg.Truncated}
	if len(text) > tail {
		cut := len(text) - tail
		for cut < len(text) && !utf8.RuneStart(text[cut]) {
			cut++
		}
		text, out.Truncated = text[cut:], true
	}
	out.Log = text
	return out, nil
}

// reader resolves the record's project and its provider client; the caller reads under
// ReadTimeout.
func (s *Sync) reader(ctx context.Context, rec *store.MergeRequest) (*project.Config, tools.RequestProvider, error) {
	pc, err := s.Projects.ForRunID(ctx, rec.RunID)
	if err != nil {
		return nil, nil, err
	}
	return pc, tools.ProviderFor(pc, s.Client), nil
}

// providerErr passes the errors a read answers as they are (a request or a job the
// provider does not have) and turns any other provider failure, a timeout included, into
// a *ProviderError.
func providerErr(err error, token string) error {
	if errors.Is(err, tools.ErrRequestNotFound) || errors.Is(err, tools.ErrJobNotFound) {
		return err
	}
	return &ProviderError{Message: cleanError(err.Error(), token), Transient: tools.TransientError(err)}
}

// redact is a provider's text as a read answers it: valid UTF-8, without the project's
// token or anything credential-shaped, and without NUL characters.
func redact(text, token string) string {
	text = strings.ToValidUTF8(text, "")
	if token != "" {
		text = strings.ReplaceAll(text, token, obs.Redacted)
	}
	return obs.Redact(text)
}

// cutLines cuts text to at most max bytes, at the end of its last whole line within them,
// or at a rune boundary when the first line alone is longer.
func cutLines(text string, max int) string {
	if len(text) <= max {
		return text
	}
	if i := strings.LastIndexByte(text[:max], '\n'); i >= 0 {
		return text[:i+1]
	}
	for max > 0 && !utf8.RuneStart(text[max]) {
		max--
	}
	return text[:max]
}
