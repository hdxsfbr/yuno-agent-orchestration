package engine

import (
	"context"

	"yuno/internal/tools"
)

// Wave 10 (DESIGN 5.24, 5.25): the engine's questions to the operator are in questions.go;
// this file holds the exported event helpers the artifact collector uses, and the call that
// hands a finished visit's workspace to the collector.

// The engine satisfies the tools' Asker, as the contract's Service says.
var _ tools.Asker = (*Engine)(nil)

// Emit records an SSE event of a run durably and fans it out (emit, exported for the
// artifact collector: run.artifacts, DESIGN 5.25 rule 2); failures are logged.
func (e *Engine) Emit(ctx context.Context, runID, event string, payload map[string]any) {
	e.emit(ctx, runID, event, payload)
}

// LogEvent writes a run log line to the structured log and to SSE, redacted for both
// (logEvent, exported for the artifact collector).
func (e *Engine) LogEvent(ctx context.Context, runID, level, msg string, attrs ...any) {
	e.logEvent(ctx, runID, level, msg, attrs...)
}

// collectArtifacts hands a finished agent visit to the artifact collector (DESIGN 5.25
// rule 2) with the visit's private workspace (5.30 rule 6), the run, the visit, the
// project configuration when the run has a checkout and the branch that checkout is on.
// It does nothing while Deps.Artifacts is nil, or for a visit that ended before it had a
// workspace (it left no files).
func (e *Engine) collectArtifacts(ctx context.Context, v *visit) {
	if e.deps.Artifacts == nil || v.workspace == "" {
		return
	}
	in := ArtifactInput{
		Workspace: v.workspace,
		Run:       v.run,
		Node:      v.rn,
	}
	if v.toolchain.Name != "" {
		in.Project = v.project
	}
	if v.branch != nil {
		in.SkipDiff, in.BranchHead = v.skipDiff, v.branch.Head
	}
	switch {
	case v.mergeRequest != nil:
		in.Branch = v.mergeRequest.SourceBranch
	case v.project != nil:
		in.Branch = v.project.DefaultBranch
	}
	e.deps.Artifacts.Collect(ctx, in)
}
