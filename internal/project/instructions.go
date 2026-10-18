package project

import (
	"strings"
	"unicode/utf8"

	"yuno/internal/store"
)

// What a visit of a run on a project starts knowing (DESIGN 5.18 rule 4): the
// operator's standing instructions and the notes earlier runs saved, appended to the
// task protocol lines of 5.16 rule 9.
const (
	// InstructionsHeading and NotesHeading start the two blocks.
	InstructionsHeading = "Project instructions from the operator:"
	NotesHeading        = "Project notes saved by earlier runs, newest first:"
	// TaskNotes is how many of the newest live notes a task carries.
	TaskNotes = 10
	// MaxNoteValueBytes cuts a note's value in the block; a cut value ends with "…".
	MaxNoteValueBytes = 300
	// MaxExtrasBytes caps the two blocks together.
	MaxExtrasBytes = 12 << 10
)

// Extras are the per-project blocks of a task: the instructions (empty for none) and
// the live notes, newest first.
type Extras struct {
	Instructions string
	Notes        []store.ProjectFact
}

// InstructionsWith is Instructions plus the two blocks of DESIGN 5.18 rule 4, after the
// sandbox network line. A block with nothing to show is left out, headings included, so
// a project with instructions and no notes gets the first block only and the other way
// round. Together the blocks are at most MaxExtrasBytes: the instructions are kept
// whole, the notes are dropped oldest first until the cap holds.
func InstructionsWith(c Config, runID string, tc Toolchain, x Extras) string {
	return Instructions(c, runID, tc) + extrasBlocks(x)
}

func extrasBlocks(x Extras) string {
	var sb strings.Builder
	if instr := strings.TrimSpace(x.Instructions); instr != "" {
		sb.WriteString(InstructionsHeading + "\n" + instr + "\n")
	}
	room := MaxExtrasBytes - sb.Len() - len(NotesHeading) - 1
	var lines []string
	for _, n := range x.Notes {
		line := "- " + oneLine(n.Key) + ": " + cutValue(oneLine(n.Value)) + "\n"
		// Notes arrive newest first, so the first line that does not fit and every
		// one after it are the oldest.
		if len(line) > room {
			break
		}
		room -= len(line)
		lines = append(lines, line)
	}
	if len(lines) > 0 {
		sb.WriteString(NotesHeading + "\n")
		for _, l := range lines {
			sb.WriteString(l)
		}
	}
	return sb.String()
}

// oneLine keeps a note on its own line, so a value cannot add lines of its own to the
// protocol block.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// cutValue cuts a value at MaxNoteValueBytes on a character boundary and marks the cut.
func cutValue(s string) string {
	if len(s) <= MaxNoteValueBytes {
		return s
	}
	i := MaxNoteValueBytes
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + "…"
}
