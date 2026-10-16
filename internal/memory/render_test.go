package memory

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"yuno/internal/store"
)

func fact(key, value string, updated time.Time) store.MemoryFact {
	return store.MemoryFact{ID: key, Key: key, Value: value, CreatedAt: updated, UpdatedAt: updated}
}

func TestRenderMemoryMDEmpty(t *testing.T) {
	if got := RenderMemoryMD(nil); got != "# Memory\n" {
		t.Fatalf("no facts = %q", got)
	}
}

// Rule 5: newest first, one line each, newlines inside a fact folded.
func TestRenderMemoryMDOrder(t *testing.T) {
	got := RenderMemoryMD([]store.MemoryFact{
		fact("old", "first thing", t0),
		fact("new", "line one\nline two", t0.Add(2*time.Minute)),
		fact("mid", "  padded   value ", t0.Add(time.Minute)),
	})
	want := "# Memory\n" + memoryMDIntro +
		"- new: line one line two\n" +
		"- mid: padded value\n" +
		"- old: first thing\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// At most 20 facts, the newest.
func TestRenderMemoryMDTwentyNewest(t *testing.T) {
	var facts []store.MemoryFact
	for i := range 30 {
		facts = append(facts, fact(fmt.Sprintf("k%02d", i), "v", t0.Add(time.Duration(i)*time.Minute)))
	}
	got := RenderMemoryMD(facts)
	if n := strings.Count(got, "\n- "); n != 20 {
		t.Fatalf("%d facts rendered, want 20", n)
	}
	if !strings.Contains(got, "- k29: v\n") || !strings.Contains(got, "- k10: v\n") || strings.Contains(got, "- k09:") {
		t.Fatalf("not the 20 newest:\n%s", got)
	}
}

// The whole file stays within 4000 characters; facts past the bound are left out,
// newest kept, and multi-byte text is counted in characters.
func TestRenderMemoryMDBound(t *testing.T) {
	var facts []store.MemoryFact
	for i := range 5 {
		facts = append(facts, fact(fmt.Sprintf("k%d", i), strings.Repeat("é", 1000), t0.Add(time.Duration(i)*time.Minute)))
	}
	got := RenderMemoryMD(facts)
	if n := utf8.RuneCountInString(got); n > MemoryMDMaxChars {
		t.Fatalf("%d characters", n)
	}
	if !strings.Contains(got, "- k4: ") || !strings.Contains(got, "- k2: ") || strings.Contains(got, "- k1: ") {
		t.Fatalf("expected the three newest facts:\n%.200s", got)
	}
	// A single fact larger than the bound leaves just the header.
	if got := RenderMemoryMD([]store.MemoryFact{fact("huge", strings.Repeat("x", 5000), t0)}); got != "# Memory\n" {
		t.Fatalf("oversized fact: %.80q", got)
	}
}
