package model

import (
	"testing"
	"time"
)

// Bindings are queues per project and trigger, taken oldest first and only once.
func TestScriptBindingsAreTakenInOrder(t *testing.T) {
	s := NewMemoryScriptStore()
	s.Bind("p1", "issue", "a")
	s.Bind("p1", "issue", "b")
	s.Bind("p1", "plan", "c")
	s.Bind("p2", "issue", "d")
	for _, want := range []struct{ project, trigger, id string }{
		{"p1", "issue", "a"}, {"p1", "plan", "c"}, {"p1", "issue", "b"}, {"p2", "issue", "d"},
	} {
		if id, ok := s.TakeBound(want.project, want.trigger); !ok || id != want.id {
			t.Fatalf("TakeBound(%s, %s) = %q %v, want %q", want.project, want.trigger, id, ok, want.id)
		}
	}
	for _, key := range [][2]string{{"p1", "issue"}, {"p1", "plan"}, {"p3", "issue"}} {
		if id, ok := s.TakeBound(key[0], key[1]); ok {
			t.Fatalf("TakeBound(%v) = %q, want none", key, id)
		}
	}
}

func TestScriptBindingsExpire(t *testing.T) {
	s := NewMemoryScriptStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	s.Bind("p1", "issue", "old")
	now = now.Add(ScriptTTL + time.Second)
	s.Bind("p1", "issue", "new")
	if id, ok := s.TakeBound("p1", "issue"); !ok || id != "new" {
		t.Fatalf("TakeBound = %q %v, want the live binding", id, ok)
	}
}
