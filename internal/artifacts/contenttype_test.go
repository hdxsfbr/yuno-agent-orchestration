package artifacts

import (
	"bytes"
	"testing"
)

// DESIGN 5.25 rule 4 (T5.25.4): the content type is what http.DetectContentType says of
// the first 512 bytes, overridden by the extension for the kinds it misreads; unknown
// content is application/octet-stream.
func TestContentType(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	pdf := []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")
	gif := []byte("GIF89a\x01\x00\x01\x00")
	html := []byte("<!DOCTYPE html><html><body>hi</body></html>")
	zip := []byte("PK\x03\x04\x14\x00\x00\x00")
	random := bytes.Repeat([]byte{0x00, 0xff, 0x13, 0x37}, 64)
	text := []byte("plain words, nothing more\n")

	for _, c := range []struct {
		name string
		path string
		head []byte
		want string
	}{
		// By extension, whatever the content says.
		{"markdown", "report.md", text, "text/markdown"},
		{"markdown upper case", "NOTES.MD", text, "text/markdown"},
		{"json", "data/out.json", []byte(`{"a":1}`), "application/json"},
		{"csv", "table.csv", []byte("a,b\n1,2\n"), "text/csv"},
		{"patch", "fix.patch", []byte("--- a\n+++ b\n"), "text/x-patch"},
		{"diff", "fix.diff", text, "text/x-patch"},
		{"generated patch", "diff.patch", []byte("diff --git a/x b/x\n"), "text/x-patch"},
		{"yaml", "c.yaml", text, "text/yaml"},
		{"yml", "c.yml", text, "text/yaml"},
		{"svg", "logo.svg", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`), "image/svg+xml"},
		{"python", "a/b/tool.py", text, "text/plain"},
		{"go", "main.go", text, "text/plain"},
		{"javascript", "app.js", text, "text/plain"},
		{"typescript", "app.ts", text, "text/plain"},
		{"tsx", "view.tsx", text, "text/plain"},
		{"shell", "run.sh", text, "text/plain"},
		{"sql", "q.sql", text, "text/plain"},
		{"toml", "c.toml", text, "text/plain"},
		{"ini", "c.ini", text, "text/plain"},
		{"log", "run.log", text, "text/plain"},
		{"an extension wins over a binary body", "weird.md", png, "text/markdown"},
		{"an empty markdown file", "empty.md", nil, "text/markdown"},
		// By sniffing.
		{"png", "chart.png", png, "image/png"},
		{"png without an extension", "chart", png, "image/png"},
		{"gif", "anim.gif", gif, "image/gif"},
		{"pdf", "paper.pdf", pdf, "application/pdf"},
		{"html is detected, not trusted by name", "page.html", html, "text/html; charset=utf-8"},
		{"html under a plain name", "page", html, "text/html; charset=utf-8"},
		{"zip", "bundle.zip", zip, "application/zip"},
		{"plain text", "readme", text, "text/plain; charset=utf-8"},
		{"plain text under an unknown extension", "notes.txt", text, "text/plain; charset=utf-8"},
		{"unknown binary", "blob.bin", random, "application/octet-stream"},
		{"empty", "empty", nil, "text/plain; charset=utf-8"},
		{"a dot file is not an extension", ".md", text, "text/markdown"},
		{"an extension in a directory name is not the file's", "v1.json/file", text, "text/plain; charset=utf-8"},
	} {
		if got := contentType(c.path, c.head); got != c.want {
			t.Errorf("%s: contentType(%q) = %q, want %q", c.name, c.path, got, c.want)
		}
	}
}
