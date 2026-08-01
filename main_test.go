package main

import (
	"strings"
	"testing"
)

// --json used to silently swallow --context, so a caller asking for context got
// JSON without it. That is the same failure mode as --in returning one hit:
// a flag accepted and ignored. Fail loudly instead.
func TestFindRejectsJSONWithContext(t *testing.T) {
	err := runFind([]string{"anything", "--json", "--context", "3"})
	if err == nil {
		t.Fatal("--json --context must error, not silently drop context")
	}
	if !strings.Contains(err.Error(), "context") {
		t.Errorf("error should name the conflict, got %q", err)
	}
}
