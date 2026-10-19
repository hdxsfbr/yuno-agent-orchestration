package openclaw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"yuno/internal/store"
)

// Workspace file names the operator may add to or override (DESIGN 5.27 units).
const (
	FileKindGenerated = "generated"
	FileKindRuntime   = "runtime"

	// AdditionsHeading separates a generated file's platform part from the operator's
	// addition (DESIGN 5.27 rule 2).
	AdditionsHeading = "\n\n## Operator additions\n\n"
)

// GeneratedFiles are written by the platform; the operator adds to them.
var GeneratedFiles = []string{"AGENTS.md", "SOUL.md", "MEMORY.md"}

// RuntimeFiles are seeded and written by OpenClaw; the operator may override them.
var RuntimeFiles = []string{"IDENTITY.md", "USER.md", "BOOTSTRAP.md", "HEARTBEAT.md", "TOOLS.md", "DREAMS.md"}

// AgentFileNames lists every named workspace file, generated first.
func AgentFileNames() []string {
	return append(slices.Clone(GeneratedFiles), RuntimeFiles...)
}

// FileKind returns "generated", "runtime", or "" for a name outside the list.
func FileKind(name string) string {
	switch {
	case slices.Contains(GeneratedFiles, name):
		return FileKindGenerated
	case slices.Contains(RuntimeFiles, name):
		return FileKindRuntime
	}
	return ""
}

// WithAddition appends the operator's addition to a generated file's content; an empty
// (or blank) addition leaves the content unchanged.
func WithAddition(content, addition string) string {
	if strings.TrimSpace(addition) == "" {
		return content
	}
	return content + AdditionsHeading + addition
}

// MemoryAddition is the addition stored for MEMORY.md in edits, "" when none.
func MemoryAddition(edits map[string]store.AgentFileEdit) string {
	if e, ok := edits["MEMORY.md"]; ok && e.Addition != nil {
		return *e.Addition
	}
	return ""
}

// ComposeMemory is the MEMORY.md content for rendered facts: the rendered text followed by
// the operator's addition. Every path that writes MEMORY.md goes through it, so the file
// the hash describes (header plus addition) and the file with facts differ only in facts.
func ComposeMemory(rendered string, edits map[string]store.AgentFileEdit) string {
	return WithAddition(rendered, MemoryAddition(edits))
}

// Extras is what Materialize needs besides the agent row and its skill rows: the
// operator's file edits and each assigned skill's bundle files by skill name. The files
// carry path, mode, size and digest; their bytes are either in Content (tests) or loaded
// through Load, and only when the workspace needs a file written.
type Extras struct {
	Edits      map[string]store.AgentFileEdit
	SkillFiles map[string][]store.SkillFile
	// Load returns the bytes of all the skills' bundle files, by skill name and path, in
	// one query. Nil when every file carries its Content.
	Load func() (map[string]map[string][]byte, error)
}

// LoadExtras reads the agent's file edits and its skills' bundle file digests (one query
// for all the skills, no content). The bytes are read by Extras.Load, at most once.
func LoadExtras(ctx context.Context, q store.Querier, agentID string, skills []store.Skill) (Extras, error) {
	ex := Extras{SkillFiles: map[string][]store.SkillFile{}}
	var err error
	if ex.Edits, err = store.GetAgentFileEdits(ctx, q, agentID); err != nil {
		return Extras{}, err
	}
	ids := make([]string, 0, len(skills))
	for _, s := range skills {
		ids = append(ids, s.ID)
	}
	digests, err := store.ListSkillFileDigests(ctx, q, ids)
	if err != nil {
		return Extras{}, err
	}
	byID := map[string]string{}
	for _, s := range skills {
		byID[s.ID] = s.Name
		if files := digests[s.ID]; len(files) > 0 {
			ex.SkillFiles[s.Name] = files
		}
	}
	if len(ex.SkillFiles) > 0 {
		var once sync.Once
		var loaded map[string]map[string][]byte
		var lerr error
		ex.Load = func() (map[string]map[string][]byte, error) {
			once.Do(func() {
				var all map[string][]store.SkillFile
				if all, lerr = store.ListSkillFilesContent(ctx, q, ids); lerr != nil {
					return
				}
				loaded = map[string]map[string][]byte{}
				for id, files := range all {
					m := map[string][]byte{}
					for _, f := range files {
						m[f.Path] = f.Content
					}
					loaded[byID[id]] = m
				}
			})
			return loaded, lerr
		}
	}
	return ex, nil
}

// bundleFile is a skill bundle file as the hash sees it: path (the map key), size,
// digest and mode, never the bytes. data is nil until the write path needs it.
type bundleFile struct {
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   int    `json:"mode"`
	data   []byte
	skill  string
	sub    string
}

// bundlePath is the workspace-relative path of a bundle file, or "" when it is not a safe
// relative path under skills/<name>/ (the database validated it already; this refuses
// anything that would leave the skill directory regardless).
func bundlePath(skill, p string) string {
	if p == "" || strings.Contains(p, `\`) || path.IsAbs(p) || !filepath.IsLocal(p) || path.Clean(p) != p || p == "SKILL.md" || p == store.SkillManifestFile {
		return ""
	}
	rel := path.Join("skills", skill, p)
	if !strings.HasPrefix(rel, "skills/"+skill+"/") || !filepath.IsLocal(rel) {
		return ""
	}
	return filepath.FromSlash(rel)
}

// newBundleFile describes f. A file that carries its content is hashed from it; one read
// without content uses the stored digest and size.
func newBundleFile(skill string, f store.SkillFile) bundleFile {
	mode := 0o644
	if f.Mode&0o111 != 0 {
		mode = 0o755
	}
	b := bundleFile{Mode: mode, skill: skill, sub: f.Path}
	if f.Content != nil || len(f.SHA256) == 0 {
		sum := sha256.Sum256(f.Content)
		b.Size, b.SHA256, b.data = len(f.Content), hex.EncodeToString(sum[:]), f.Content
		if b.data == nil {
			b.data = []byte{}
		}
		return b
	}
	b.Size, b.SHA256 = f.Size, hex.EncodeToString(f.SHA256)
	return b
}
