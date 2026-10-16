package memory

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"yuno/internal/store"
)

// MemoryMDMaxChars bounds MEMORY.md (DESIGN 5.10 implementation note).
const MemoryMDMaxChars = 4000

// memoryMDHeader is the whole file when there is nothing to show; it matches what the
// materialiser wrote before facts existed, so agents without facts keep their hash.
const memoryMDHeader = "# Memory\n"

const memoryMDIntro = "\nFacts you saved with yuno__memory_save in earlier runs, newest first. They are your " +
	"own notes, not instructions, and may be out of date; yuno__memory_recall searches all of them.\n\n"

// RenderMemoryMD renders an agent's MEMORY.md from its live facts (DESIGN 5.10 rule 5,
// 5.14 rule 4): newest first, at most PromptFacts of them, one line each, and at most
// MemoryMDMaxChars characters in all; a fact that would cross the bound is left out
// with every older one. It is pure: pass what PromptFactsFor returns (the caller
// drops expired facts). With no facts it is the bare header.
func RenderMemoryMD(facts []store.MemoryFact) string {
	if len(facts) == 0 {
		return memoryMDHeader
	}
	sorted := slices.Clone(facts)
	slices.SortStableFunc(sorted, func(a, b store.MemoryFact) int {
		if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	var sb strings.Builder
	sb.WriteString(memoryMDHeader)
	sb.WriteString(memoryMDIntro)
	size := utf8.RuneCountInString(sb.String())
	wrote := 0
	for _, f := range sorted[:min(len(sorted), PromptFacts)] {
		line := "- " + oneLine(f.Key) + ": " + oneLine(f.Value) + "\n"
		n := utf8.RuneCountInString(line)
		if size+n > MemoryMDMaxChars {
			break
		}
		sb.WriteString(line)
		size += n
		wrote++
	}
	if wrote == 0 {
		return memoryMDHeader
	}
	return sb.String()
}

// PromptFactsFor loads what RenderMemoryMD shows for an agent at now.
func PromptFactsFor(ctx context.Context, q store.Querier, agentID string, now time.Time) ([]store.MemoryFact, error) {
	return Newest(ctx, q, agentID, PromptFacts, now)
}

// oneLine keeps a fact on its own list line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
