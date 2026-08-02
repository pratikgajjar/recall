package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The embedded skill is the copy that ships; the installed copies are what
// actually steer an agent. Drift between them is invisible and expensive, so
// the shipped text must at minimum still carry the cheap-path guidance.
func TestEmbeddedSkillCarriesCheapPathGuidance(t *testing.T) {
	body := string(skillContent())
	if body == "" {
		t.Fatal("skill did not embed")
	}
	for _, must := range []string{"--in", "--context", "recall find"} {
		if !strings.Contains(body, must) {
			t.Errorf("embedded skill lost %q — agents would fall back to outlining", must)
		}
	}
	// The pre-v0.7 skill led with outline. If that ever comes back as the
	// recommended first move, the guidance has regressed.
	if strings.Contains(body, "start with `--outline") {
		t.Error("skill recommends outline as the first move again")
	}
}

func TestSkillInstallWritesAndDoctorSeesDrift(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "recall")

	if err := runSkill([]string{"install", target}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(skillContent()) {
		t.Error("installed copy differs from embedded")
	}

	// Staleness detection is the whole point: mutate and it must be noticed.
	if err := os.WriteFile(filepath.Join(target, "SKILL.md"), []byte("old guidance"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(target, "SKILL.md"))
	if string(after) == string(skillContent()) {
		t.Fatal("setup failed")
	}
	// Reinstalling repairs it.
	if err := runSkill([]string{"install", target}); err != nil {
		t.Fatal(err)
	}
	fixed, _ := os.ReadFile(filepath.Join(target, "SKILL.md"))
	if string(fixed) != string(skillContent()) {
		t.Error("reinstall did not repair a stale copy")
	}
}

func TestSkillUsageErrors(t *testing.T) {
	if err := runSkill(nil); err == nil {
		t.Error("bare `recall skill` should explain usage")
	}
	if err := runSkill([]string{"bogus"}); err == nil {
		t.Error("unknown subcommand should error")
	}
}

// `recall skill install --force` created a directory named "--force", wrote the
// skill into it, and the next `git add -A` committed it. A path starting with a
// dash then breaks any shell loop over the repo's files — `cat` and `file` both
// read it as an option.
func TestSkillInstallRejectsFlagAsDirectory(t *testing.T) {
	for _, arg := range []string{"--force", "-f", "--dir=/tmp/x"} {
		err := runSkill([]string{"install", arg})
		if err == nil {
			t.Errorf("skill install %q was accepted as a directory", arg)
			continue
		}
		if !strings.Contains(err.Error(), "directory") {
			t.Errorf("error for %q should say what the argument is for: %v", arg, err)
		}
		if _, statErr := os.Stat(arg); statErr == nil {
			os.RemoveAll(arg)
			t.Errorf("skill install %q created it on disk", arg)
		}
	}
}
