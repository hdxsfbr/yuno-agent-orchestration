package httpapi

import (
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"yuno/internal/store"
)

// Run artifacts (DESIGN 5.25 rule 5): the list of the files a run delivered and the
// download of one. The files live under ARTIFACT_DIR as <run id>/<artifact id>, written
// by the collector (internal/artifacts); nothing here ever renders one inline.

// listArtifacts is GET /api/runs/{id}/artifacts: the run's artifacts by path, 404 for an
// unknown run, [] for a run that delivered none. With no artifact directory configured
// (the store is disabled) the list is empty.
func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if _, err := store.GetRun(r.Context(), s.pool, runID); err != nil {
		s.storeError(w, r, "run", err)
		return
	}
	if s.cfg.ArtifactDir == "" {
		writeJSON(w, http.StatusOK, []store.RunArtifact{})
		return
	}
	list, err := store.ListRunArtifacts(r.Context(), s.pool, runID)
	if err != nil {
		s.internalError(w, r, "list the run's artifacts", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// downloadArtifact is GET /api/runs/{id}/artifacts/{artifactId}: the file as an
// attachment. The content type is the stored one, except that what a browser would render
// as a page (HTML, XHTML, SVG) is sent as plain text; nosniff keeps it that way. 404 when
// the artifact is not this run's, and 404 with its own message when the file is gone.
func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	a, err := store.GetRunArtifact(r.Context(), s.pool, r.PathValue("id"), r.PathValue("artifactId"))
	if err != nil {
		s.storeError(w, r, "artifact", err)
		return
	}
	f, err := openArtifactFile(s.cfg.ArtifactDir, a)
	if err != nil {
		if fileIsMissing(err) {
			writeError(w, http.StatusNotFound, "not_found", "artifact file is missing", nil)
			return
		}
		s.internalError(w, r, "open an artifact file", err)
		return
	}
	defer f.Close()

	h := w.Header()
	h.Set("Content-Type", servedContentType(a.ContentType))
	h.Set("Content-Disposition", `attachment; filename="`+downloadName(a.Path)+`"`)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, no-store")
	// The type is set, so ServeContent sniffs nothing; it adds Content-Length and answers
	// a Range request.
	http.ServeContent(w, r, "", time.Time{}, f)
}

var errNotAFile = errors.New("not a regular file")

// fileIsMissing reports whether err means there is no file to send where the row says: it
// does not exist, it is not a regular file, or the way to it is a link (the root refuses a
// link that leads out of the store). Anything else is a failure of the store.
func fileIsMissing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotAFile) || errors.Is(err, syscall.ELOOP) ||
		errors.Is(err, syscall.ENOTDIR) || strings.Contains(err.Error(), "escapes from parent")
}

// openArtifactFile opens <dir>/<run id>/<artifact id> through a root, so nothing in the
// store can lead out of it, and refuses anything but a regular file.
func openArtifactFile(dir string, a *store.RunArtifact) (*os.File, error) {
	if dir == "" {
		return nil, fs.ErrNotExist
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(filepath.Join(a.RunID, a.ID))
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err == nil {
			err = errNotAFile
		}
		return nil, err
	}
	return f, nil
}

// servedContentType is the Content-Type a download is sent with: the stored one, except
// that HTML, XHTML and SVG, which a browser renders, are sent as plain text. A stored type
// that cannot be read is sent as an opaque download.
func servedContentType(stored string) string {
	mediaType, _, err := mime.ParseMediaType(stored)
	if err != nil {
		return "application/octet-stream"
	}
	switch mediaType {
	case "text/html", "application/xhtml+xml", "image/svg+xml":
		return "text/plain; charset=utf-8"
	}
	return stored
}

// downloadName is the file name offered to the browser: the base name of the artifact's
// path with the characters that could end the quoted string or the header removed (quotes,
// a backslash that would escape the closing one, and control characters).
func downloadName(artifactPath string) string {
	name := strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || unicode.IsControl(r) {
			return -1
		}
		return r
	}, path.Base(artifactPath))
	if name == "" || name == "." || name == "/" {
		return "artifact"
	}
	return name
}
