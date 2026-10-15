package pathcheck

import (
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	ok := []string{"main.py", "src/app/x.py", "tests/test_a.py", "a-b_c.1"}
	bad := []string{"/etc/passwd", "../x", "a/../b", "a b", "", "x\n", strings.Repeat("a", 201)}
	for _, p := range ok {
		if err := Valid(p); err != nil {
			t.Errorf("%q should be valid: %v", p, err)
		}
	}
	for _, p := range bad {
		if err := Valid(p); err == nil {
			t.Errorf("%q should be invalid", p)
		}
	}
}
