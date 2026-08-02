package main

import (
	"os"
	"os/exec"
	"path/filepath"
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

// `recall mcp` was a subcommand until v0.7.0. Removing it silently let the word
// fall through to search, so a client still configured with
// `claude mcp add recall -- recall mcp` piped JSON-RPC in and got ranked search
// results back: no error, an integration that quietly did nothing.
func TestRemovedMCPSubcommandSaysSo(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "recall")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "mcp")
	cmd.Env = append(os.Environ(), "RECALL_INDEX="+filepath.Join(t.TempDir(), "i.sqlite"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("`recall mcp` exited 0; an old MCP client would see success")
	}
	for _, want := range []string{"removed", "skill"} {
		if !strings.Contains(strings.ToLower(string(out)), want) {
			t.Errorf("message should mention %q, got: %s", want, out)
		}
	}
	// Searching for the word must still reach search. An empty index exits 1
	// for "no matches", the way grep does, so the check is that it did not take
	// the removal branch.
	cmd = exec.Command(bin, "find", "mcp", "--limit", "1")
	cmd.Env = append(os.Environ(), "RECALL_INDEX="+filepath.Join(t.TempDir(), "j.sqlite"))
	out, _ = cmd.CombinedOutput()
	if strings.Contains(string(out), "was removed") {
		t.Errorf("`recall find mcp` hit the removal notice instead of searching: %s", out)
	}
}
