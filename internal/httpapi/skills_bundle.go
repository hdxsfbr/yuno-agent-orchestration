package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"yuno/internal/obs"
	"yuno/internal/skillbundle"
	"yuno/internal/store"
)

// Skill bundles (DESIGN 5.27 rules 5 to 11): zip and Git import, the file API and export.

const (
	maxSkillImportRequest = 25 << 20
	maxSkillFileText      = 256 << 10
	maxSkillFileRequest   = 2 << 20
)

// newGitImporter builds the importer for one request; tests replace it with one that has a
// fake git and transport.
var newGitImporter = func(hosts []string) *skillbundle.GitImporter {
	return &skillbundle.GitImporter{Hosts: hosts}
}

// ImportSkillLibrary imports the skills library at start (rule 10): skills not yet present
// are added, none is overwritten, and a failure is logged, never fatal.
func (s *Server) ImportSkillLibrary(ctx context.Context) {
	if s.pool == nil || s.cfg.SkillsLibraryDir == "" {
		return
	}
	if _, err := skillbundle.ImportLibrary(ctx, s.pool, s.cfg.SkillsLibraryDir, s.log); err != nil {
		s.log.Warn("skills library not imported", "error", err)
	}
}

// writeReasons answers 422 with the reasons in the message ("invalid skill: url: host "x"
// is not allowed (...)"), several joined with "; ", so a client that shows only the message
// still tells the user what to change.
func writeReasons(w http.ResponseWriter, what string, fe fieldErrors) {
	parts := make([]string, 0, len(fe))
	for _, f := range fe {
		parts = append(parts, f.Path+": "+f.Message)
	}
	writeValidation(w, what+": "+strings.Join(parts, "; "), fe)
}

// bundleError answers an import error: 400 malformed, 413 over a cap, 422 a bad field.
func bundleError(w http.ResponseWriter, err error) bool {
	var fe *skillbundle.FormatError
	var ce *skillbundle.CapError
	var ie *skillbundle.InputError
	switch {
	case errors.As(err, &fe):
		writeError(w, http.StatusBadRequest, "invalid_bundle", fe.Msg, nil)
	case errors.As(err, &ce):
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", ce.Msg, nil)
	case errors.As(err, &ie):
		writeReasons(w, "invalid skill", fieldErrors{{Path: ie.Path, Message: ie.Msg}})
	default:
		return false
	}
	return true
}

func replaceParam(w http.ResponseWriter, r *http.Request) (replace, ok bool) {
	v := r.URL.Query().Get("replace")
	if v == "" {
		return false, true
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		writeValidation(w, "invalid request", fieldErrors{{Path: "replace", Message: "must be true or false"}})
		return false, false
	}
	return b, true
}

// importSkillZip is POST /api/skills/import (rule 6).
func (s *Server) importSkillZip(w http.ResponseWriter, r *http.Request) {
	replace, ok := replaceParam(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSkillImportRequest)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_bundle", "expected a multipart/form-data upload with a zip in the field \"file\"", nil)
		return
	}
	var data []byte
	var zipName string
	found := false
	for !found {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.uploadReadError(w, err)
			return
		}
		if part.FormName() != "file" {
			continue
		}
		zipName = part.FileName()
		data, err = io.ReadAll(io.LimitReader(part, skillbundle.MaxZipBytes+1))
		if err != nil {
			s.uploadReadError(w, err)
			return
		}
		found = true
	}
	if !found {
		writeValidation(w, "invalid request", fieldErrors{{Path: "file", Message: "is required"}})
		return
	}
	if len(data) > skillbundle.MaxZipBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the upload is over the 25 MiB cap", nil)
		return
	}
	b, err := skillbundle.ExtractZip(bytes.NewReader(data), int64(len(data)), zipName)
	if err != nil {
		if !bundleError(w, err) {
			s.internalError(w, r, "extract skill zip", err)
		}
		return
	}
	s.saveImported(w, r, b, store.SkillSourceUpload, "", "", replace)
}

func (s *Server) uploadReadError(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the upload is over the 25 MiB cap", nil)
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_bundle", "the upload could not be read as multipart/form-data", nil)
}

type importGitInput struct {
	URL  string `json:"url"`
	Ref  string `json:"ref"`
	Path string `json:"path"`
}

// importSkillGit is POST /api/skills/import-git (rule 7).
func (s *Server) importSkillGit(w http.ResponseWriter, r *http.Request) {
	replace, ok := replaceParam(w, r)
	if !ok {
		return
	}
	var in importGitInput
	if !decodeBody(w, r, &in, 16<<10) {
		return
	}
	res, err := newGitImporter(s.cfg.SkillImportHosts).Import(r.Context(), skillbundle.GitRequest{URL: in.URL, Ref: in.Ref, Path: in.Path})
	if err != nil {
		if bundleError(w, err) {
			return
		}
		s.log.Warn("skill git import failed", "url", in.URL, "error", err)
		writeError(w, http.StatusBadGateway, "import_failed", obs.Redact(err.Error()), nil)
		return
	}
	s.saveImported(w, r, res.Bundle, store.SkillSourceGit, res.URL, res.Commit, replace)
}

// saveImported stores a parsed bundle: a new skill (201), or with replace the existing one
// by that name, its body, source and files in one transaction (200). Without replace an
// existing name is 409.
func (s *Server) saveImported(w http.ResponseWriter, r *http.Request, b *skillbundle.Bundle, source, sourceURL, sourceRef string, replace bool) {
	ctx := r.Context()
	same, err := store.ListSkillsByName(ctx, s.pool, []string{b.Name})
	if err != nil {
		s.internalError(w, r, "skill name", err)
		return
	}
	exists := len(same) > 0
	if exists && !replace {
		writeError(w, http.StatusConflict, "skill_exists",
			"a skill named \""+b.Name+"\" already exists; import with replace=true to replace it", map[string]string{"id": same[0].ID})
		return
	}
	var id string
	var users []string
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if exists {
			id = same[0].ID
			next := &store.Skill{ID: id, Name: b.Name, Description: b.Description, Body: b.Body}
			if err := store.UpdateSkill(ctx, tx, next); err != nil {
				return err
			}
			if err := store.SetSkillSource(ctx, tx, id, source, sourceURL, sourceRef, b.License); err != nil {
				return err
			}
			var err error
			if users, err = renameAgentSkill(ctx, tx, b.Name, b.Name); err != nil {
				return err
			}
		} else {
			sk := &store.Skill{Name: b.Name, Description: b.Description, Body: b.Body,
				Source: source, SourceURL: sourceURL, SourceRef: sourceRef, License: b.License}
			if err := store.CreateSkill(ctx, tx, sk); err != nil {
				return err
			}
			id = sk.ID
		}
		return store.ReplaceSkillFiles(ctx, tx, id, b.Files)
	})
	if isUniqueViolation(err) {
		writeError(w, http.StatusConflict, "skill_exists", "a skill named \""+b.Name+"\" already exists", nil)
		return
	}
	if err != nil {
		s.internalError(w, r, "import skill", err)
		return
	}
	saved, err := store.GetSkill(ctx, s.pool, id)
	if err != nil {
		s.storeError(w, r, "skill", err)
		return
	}
	if !s.rematerialize(w, r, id, users) {
		return
	}
	status := http.StatusCreated
	if exists {
		status = http.StatusOK
	}
	writeJSON(w, status, saved)
}

// skillUsers returns the ids of the agents that list the skill.
func (s *Server) skillUsers(ctx context.Context, name string) ([]string, error) {
	agents, err := store.ListAgents(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, a := range agents {
		if slices.Contains(a.Skills, name) {
			ids = append(ids, a.ID)
		}
	}
	return ids, nil
}

// skillFileBody is one file as the API shows it.
type skillFileBody struct {
	Path    string `json:"path"`
	Size    int    `json:"size"`
	Mode    int    `json:"mode"`
	Content string `json:"content"`
}

// skillFilePath is the {path...} of the request, checked as in an import. A bad path is
// reported as the error text.
func skillFilePath(r *http.Request) (string, error) {
	p := r.PathValue("path")
	if err := skillbundle.CheckPath(p); err != nil {
		return "", err
	}
	return p, nil
}

func (s *Server) listSkillFiles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, err := store.GetSkill(ctx, s.pool, r.PathValue("id")); err != nil {
		s.storeError(w, r, "skill", err)
		return
	}
	files, err := store.ListSkillFileMeta(ctx, s.pool, r.PathValue("id"))
	if err != nil {
		s.internalError(w, r, "list skill files", err)
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (s *Server) getSkillFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, err := skillFilePath(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "file not found", nil)
		return
	}
	f, err := store.GetSkillFile(r.Context(), s.pool, id, p)
	if err != nil {
		s.storeError(w, r, "file", err)
		return
	}
	if len(f.Content) > maxSkillFileText || !utf8.Valid(f.Content) || bytes.IndexByte(f.Content, 0) >= 0 {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"this file is binary or larger than 256 KiB; it is not shown here, use Export to get the bundle", nil)
		return
	}
	writeJSON(w, http.StatusOK, skillFileBody{Path: f.Path, Size: f.Size, Mode: f.Mode, Content: string(f.Content)})
}

type skillFileInput struct {
	Content *string `json:"content"`
	Mode    *int    `json:"mode"`
	// Read-only fields, accepted so a client can PUT back what it fetched.
	Path any `json:"path"`
	Size any `json:"size"`
}

// putSkillFile creates or replaces one text file (rules 5 and 11) and re-materialises the
// agents that hold the skill.
func (s *Server) putSkillFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sk, err := store.GetSkill(ctx, s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "skill", err)
		return
	}
	p, perr := skillFilePath(r)
	if perr != nil {
		writeReasons(w, "invalid file", fieldErrors{{Path: "path", Message: perr.Error()}})
		return
	}
	var in skillFileInput
	if !decodeBody(w, r, &in, maxSkillFileRequest) {
		return
	}
	var fe fieldErrors
	if in.Content == nil {
		fe.add("content", "is required")
	} else if bytes.IndexByte([]byte(*in.Content), 0) >= 0 {
		fe.add("content", "must be text")
	}
	if in.Mode != nil && *in.Mode != 0o644 && *in.Mode != 0o755 {
		fe.add("mode", "must be 420 (0644) or 493 (0755)")
	}
	if len(fe) > 0 {
		writeReasons(w, "invalid file", fe)
		return
	}
	content := []byte(*in.Content)
	if len(content) > maxSkillFileText {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "a file edited here is at most 256 KiB", nil)
		return
	}
	meta, err := store.ListSkillFileMeta(ctx, s.pool, sk.ID)
	if err != nil {
		s.internalError(w, r, "list skill files", err)
		return
	}
	total, cur := len(content), (*store.SkillFile)(nil)
	for i := range meta {
		if meta[i].Path == p {
			cur = &meta[i]
		} else {
			total += meta[i].Size
		}
	}
	switch {
	case cur == nil && len(meta) >= skillbundle.MaxFiles:
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "a skill has at most 200 files", nil)
		return
	case total > skillbundle.MaxTotalBytes:
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "a skill's files are at most 20 MiB in total", nil)
		return
	}
	mode := 0o644
	if in.Mode != nil {
		mode = *in.Mode
	} else if cur != nil {
		mode = cur.Mode
	}
	f := &store.SkillFile{Path: p, Content: content, Mode: mode}
	if err := store.PutSkillFile(ctx, s.pool, sk.ID, f); err != nil {
		s.storeError(w, r, "skill", err)
		return
	}
	users, err := s.skillUsers(ctx, sk.Name)
	if err != nil {
		s.internalError(w, r, "skill users", err)
		return
	}
	if !s.rematerialize(w, r, sk.ID, users) {
		return
	}
	writeJSON(w, http.StatusOK, skillFileBody{Path: f.Path, Size: f.Size, Mode: f.Mode, Content: string(content)})
}

func (s *Server) deleteSkillFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sk, err := store.GetSkill(ctx, s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "skill", err)
		return
	}
	p, perr := skillFilePath(r)
	if perr != nil {
		writeError(w, http.StatusNotFound, "not_found", "file not found", nil)
		return
	}
	if err := store.DeleteSkillFile(ctx, s.pool, sk.ID, p); err != nil {
		s.storeError(w, r, "file", err)
		return
	}
	users, err := s.skillUsers(ctx, sk.Name)
	if err != nil {
		s.internalError(w, r, "skill users", err)
		return
	}
	if !s.rematerialize(w, r, sk.ID, users) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// exportSkill returns the bundle as a zip (rule 11).
func (s *Server) exportSkill(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sk, err := store.GetSkill(ctx, s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "skill", err)
		return
	}
	files, err := store.ListSkillFiles(ctx, s.pool, sk.ID)
	if err != nil {
		s.internalError(w, r, "list skill files", err)
		return
	}
	var buf bytes.Buffer
	if err := skillbundle.WriteZip(&buf, *sk, files); err != nil {
		s.internalError(w, r, "export skill", err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sk.Name+`.zip"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	_, _ = w.Write(buf.Bytes())
}
