//go:build unix

package httpapi

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Finding 1: a named pipe where a workspace file belongs is absent in the list, and the
// request does not block on it.
func TestAgentFilesListIgnoresNamedPipe(t *testing.T) {
	f := newFilesFixture(t, true)
	if err := syscall.Mkfifo(filepath.Join(f.ws, "IDENTITY.md"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan map[string]fileView, 1)
	go func() { done <- f.list(t) }()
	select {
	case got := <-done:
		if v := got["IDENTITY.md"]; v.Exists || v.Content != "" {
			t.Fatalf("IDENTITY.md = %+v", v)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the files list blocked on a named pipe")
	}
}
