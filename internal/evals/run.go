// Package evals is the reproducible evaluation runner (DESIGN 5.13). It is an HTTP
// client of the api: it creates a temporary agent per case, registers the case's script
// with the scripted provider, triggers a run, waits for a terminal state, evaluates the
// trace and writes a report. Cases are YAML files under evals/cases/<version>/ and are
// documented in evals/README.md.
package evals

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"yuno/internal/config"
)

// Eval providers (EVAL_PROVIDER). The runs themselves are always replayed from the
// cases' scripts; the provider chooses who grades the rubric: the local checks for
// "scripted", claude-opus-5-5 through the api's model proxy for "anthropic".
const (
	ProviderScripted  = "scripted"
	ProviderAnthropic = "anthropic"
)

// Run executes the suite for cfg.EvalSplit against cfg.EvalAPIURL and writes the
// report under cfg.EvalReportsDir. It returns an error, so the process exits 1, when
// the public pass rate is below 0.9 (DESIGN 5.13).
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	return runSuite(ctx, cfg, log, os.Stdout, nil)
}

// normalizeProvider returns the eval provider, defaulting to scripted.
func normalizeProvider(p string) (string, error) {
	switch p = strings.ToLower(strings.TrimSpace(p)); p {
	case "", ProviderScripted:
		return ProviderScripted, nil
	case ProviderAnthropic:
		return ProviderAnthropic, nil
	}
	return "", fmt.Errorf("eval provider %q is not supported (want %s or %s)", p, ProviderScripted, ProviderAnthropic)
}

// runSuite is Run with the summary destination and runner settings injectable.
func runSuite(ctx context.Context, cfg config.Config, log *slog.Logger, out io.Writer, tune func(*Runner)) error {
	provider, err := normalizeProvider(cfg.EvalProvider)
	if err != nil {
		return err
	}
	cfg.EvalProvider = provider
	cases, err := LoadCases(cfg.EvalCasesDir, cfg.EvalSplit)
	if err != nil {
		return err
	}
	log.Info("eval start", "split", cfg.EvalSplit, "cases", len(cases), "api", cfg.EvalAPIURL, "provider", provider)

	runner := NewRunner(cfg, log)
	if tune != nil {
		tune(runner)
	}
	results, runErr := runner.Execute(ctx, cases)
	if len(results) == 0 {
		if runErr != nil {
			return fmt.Errorf("eval: %w", runErr)
		}
		return fmt.Errorf("eval: no case ran")
	}

	report := BuildReport(ReportMeta{
		GeneratedAt: runner.now(),
		Split:       strings.ToLower(strings.TrimSpace(cfg.EvalSplit)),
		Provider:    provider,
		APIURL:      cfg.EvalAPIURL,
		CasesDir:    cfg.EvalCasesDir,
	}, results)
	mdPath, jsonPath, err := WriteReport(cfg.EvalReportsDir, report)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, report.Summary.SummaryLine)
	log.Info("eval report written", "markdown", mdPath, "json", jsonPath)

	if runErr != nil {
		// Not wrapped with %w: an interrupted suite must not look like a clean cancel.
		return fmt.Errorf("%w after %d of %d cases: %v", errInterrupted, len(results), len(cases), runErr)
	}
	return report.PublicGate()
}
