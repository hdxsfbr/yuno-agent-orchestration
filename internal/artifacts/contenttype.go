package artifacts

import (
	"net/http"
	"path"
	"strings"
)

// extensionTypes are the content types an extension decides (DESIGN 5.25 rule 4): the
// text kinds http.DetectContentType misreads (a Markdown, JSON or source file all sniff as
// text/plain) and SVG, which it reads as XML or text.
var extensionTypes = map[string]string{
	".md":    "text/markdown",
	".json":  "application/json",
	".csv":   "text/csv",
	".patch": "text/x-patch",
	".diff":  "text/x-patch",
	".yaml":  "text/yaml",
	".yml":   "text/yaml",
	".svg":   "image/svg+xml",
	".py":    "text/plain",
	".go":    "text/plain",
	".js":    "text/plain",
	".ts":    "text/plain",
	".tsx":   "text/plain",
	".sh":    "text/plain",
	".sql":   "text/plain",
	".toml":  "text/plain",
	".ini":   "text/plain",
	".log":   "text/plain",
}

// contentType is the content type stored for a file (DESIGN 5.25 rule 4):
// http.DetectContentType on the first 512 bytes (head), overridden by the extension of p
// for the kinds above; as detected otherwise, which is application/octet-stream for
// content it cannot place (and text/plain for an empty file).
func contentType(p string, head []byte) string {
	if t, ok := extensionTypes[strings.ToLower(path.Ext(p))]; ok {
		return t
	}
	return http.DetectContentType(head)
}
