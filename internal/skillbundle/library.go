package skillbundle

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"yuno/internal/store"
)

// DB is a pool or a transaction: the library importer reads and writes through it, and
// begins a (nested) transaction per skill.
type DB interface {
	store.Querier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// librarySource describes one directory of skills-library/ (DESIGN 5.27 rule 10): where its
// skills live upstream, their licence, and whether the source-level LICENSE file is copied
// into each skill as LICENSE.
type librarySource struct {
	urlPrefix   string
	license     string
	licenseFile bool
}

var librarySources = map[string]librarySource{
	"anthropic": {"https://github.com/anthropics/skills/tree/main/skills/", "Apache-2.0", false},
	"openclaw":  {"https://github.com/openclaw/openclaw/tree/main/skills/", "MIT", true},
	"clawhub":   {"https://clawhub.ai/cmanfre7/skills/", "MIT-0", true},
}

// LibraryResult counts what an import did.
type LibraryResult struct {
	Imported int
	Existing int
	Failed   int
}

// ImportLibrary imports every skill under dir/<source>/<name>/ that is not yet in skills,
// with source "library", its upstream URL and licence. An existing skill of the same name
// is never touched, so a second run imports nothing. A missing directory is not an error;
// a skill that cannot be read or stored is logged and counted as failed.
func ImportLibrary(ctx context.Context, db DB, dir string, log *slog.Logger) (LibraryResult, error) {
	var res LibraryResult
	sources, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Warn("skills library not found; nothing imported", "dir", dir)
			return res, nil
		}
		return res, err
	}
	for _, src := range sources {
		spec, ok := librarySources[src.Name()]
		if !src.IsDir() || !ok {
			continue
		}
		skills, err := os.ReadDir(filepath.Join(dir, src.Name()))
		if err != nil {
			return res, err
		}
		for _, sk := range skills {
			if !sk.IsDir() {
				continue
			}
			if err := ctx.Err(); err != nil {
				return res, err
			}
			outcome, err := importLibrarySkill(ctx, db, filepath.Join(dir, src.Name()), src.Name(), sk.Name(), spec)
			switch {
			case err != nil:
				res.Failed++
				log.Warn("library skill not imported", "source", src.Name(), "skill", sk.Name(), "error", err)
			case outcome:
				res.Imported++
				log.Info("library skill imported", "source", src.Name(), "skill", sk.Name())
			default:
				res.Existing++
			}
		}
	}
	log.Info("skills library", "imported", res.Imported, "existing", res.Existing, "failed", res.Failed)
	return res, nil
}

// importLibrarySkill imports one skill; created is false when a skill of that name exists.
func importLibrarySkill(ctx context.Context, db DB, sourceDir, source, dirName string, spec librarySource) (created bool, err error) {
	// The skill is matched on the name its SKILL.md gives, not on the folder: an unrelated
	// skill that happens to be called like the folder must not hide it.
	var extra []ExtraFile
	if spec.licenseFile {
		if data := readLicense(filepath.Join(sourceDir, "LICENSE")); data != nil {
			extra = append(extra, ExtraFile{Path: "LICENSE", Content: data, Mode: 0o644})
		}
	}
	b, err := ReadDirWith(filepath.Join(sourceDir, dirName), dirName, true, extra...)
	if err != nil {
		return false, err
	}
	if exists, err := skillExists(ctx, db, b.Name); err != nil || exists {
		return false, err
	}
	sk := &store.Skill{
		Name: b.Name, Description: b.Description, Body: b.Body,
		Source: store.SkillSourceLibrary, SourceURL: spec.urlPrefix + dirName, License: spec.license,
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.CreateSkill(ctx, tx, sk); err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return false, nil
		}
		return false, err
	}
	if err := store.ReplaceSkillFiles(ctx, tx, sk.ID, b.Files); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// readLicense reads a source-level LICENSE file, at most one byte over the per-file cap
// (the cap check refuses the bundle); nil when it is missing.
func readLicense(p string) []byte {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil
	}
	return data
}

func skillExists(ctx context.Context, q store.Querier, name string) (bool, error) {
	same, err := store.ListSkillsByName(ctx, q, []string{name})
	return len(same) > 0, err
}
