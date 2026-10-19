package skillbundle

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"strings"
	"time"

	"yuno/internal/store"
)

// SkillMD renders a skill's SKILL.md: front matter (name, description, and license when
// recorded) and the body. A JSON string is a valid YAML scalar.
func SkillMD(s store.Skill) string {
	q := func(v string) string {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(v)
		return strings.TrimSpace(b.String())
	}
	var sb strings.Builder
	sb.WriteString("---\nname: " + s.Name + "\ndescription: " + q(s.Description) + "\n")
	if s.License != "" {
		sb.WriteString("license: " + q(s.License) + "\n")
	}
	sb.WriteString("---\n\n" + strings.TrimRight(s.Body, "\n") + "\n")
	return sb.String()
}

// WriteZip writes the bundle as a zip under a top folder named after the skill (import
// strips it): SKILL.md and every file with its mode.
func WriteZip(w io.Writer, s store.Skill, files []store.SkillFile) error {
	zw := zip.NewWriter(w)
	add := func(name string, mode int, data []byte) error {
		h := &zip.FileHeader{Name: s.Name + "/" + name, Method: zip.Deflate, Modified: s.UpdatedAt.UTC().Truncate(time.Second)}
		h.SetMode(modeBits(mode))
		fw, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = fw.Write(data)
		return err
	}
	if err := add(SkillFile, 0o644, []byte(SkillMD(s))); err != nil {
		return err
	}
	for _, f := range files {
		if err := add(f.Path, f.Mode, f.Content); err != nil {
			return err
		}
	}
	return zw.Close()
}

func modeBits(m int) (b fs.FileMode) {
	if m&0o111 != 0 {
		return 0o755
	}
	return 0o644
}
