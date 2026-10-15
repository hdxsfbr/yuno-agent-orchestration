// Package pathcheck validates user- and model-supplied relative paths before they touch
// a filesystem or a release.
package pathcheck

import (
	"fmt"
	"regexp"
	"strings"
)

var valid = regexp.MustCompile(`^[a-zA-Z0-9_./-]{1,200}$`)

// Valid rejects absolute paths, parent traversal and odd characters.
func Valid(p string) error {
	if !valid.MatchString(p) {
		return fmt.Errorf("path %q: must match %s", p, valid.String())
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("path %q: must be relative", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("path %q: parent traversal not allowed", p)
		}
	}
	return nil
}
