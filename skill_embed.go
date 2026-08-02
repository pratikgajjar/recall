package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// embeddedSkill bundles the agent skill into the binary. It is installed by
// copying a file into a skills directory, which means every installed copy
// silently rots the moment the guidance changes — and stale guidance is
// expensive: the pre-v0.7 skill told agents to outline a session (~4k chars)
// rather than search inside it (~400). Shipping it in the binary is what lets
// `recall doctor` notice the drift.
//
//go:embed skills/recall/SKILL.md
var embeddedSkill embed.FS

const skillPath = "skills/recall/SKILL.md"

func skillContent() []byte {
	b, err := embeddedSkill.ReadFile(skillPath)
	if err != nil {
		return nil // impossible: go:embed fails the build if the file is missing
	}
	return b
}

func shortHash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:4])
}

// skillDirs are the conventional per-agent skill locations. Only directories
// that already hold a recall skill are considered installed; recall does not
// invent skill homes for agents the user does not use.
func skillDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".pi", "agent", "skills", "recall"),
		filepath.Join(home, ".claude", "skills", "recall"),
		filepath.Join(home, ".agents", "skills", "recall"),
	}
}

type skillState struct {
	dir     string
	present bool
	stale   bool
}

func installedSkills() []skillState {
	want := skillContent()
	var out []skillState
	for _, d := range skillDirs() {
		got, err := os.ReadFile(filepath.Join(d, "SKILL.md"))
		if err != nil {
			out = append(out, skillState{dir: d})
			continue
		}
		out = append(out, skillState{dir: d, present: true, stale: string(got) != string(want)})
	}
	return out
}

// runSkill implements `recall skill install` — writes the embedded skill over
// every installed copy, so updating the binary can actually update the guidance.
//
// It installs what is embedded in *this binary*, not what is on disk: editing
// skills/recall/SKILL.md and running install without rebuilding first writes the
// previous text. `recall doctor` catches that, which is how it was found.
func runSkill(args []string) error {
	if len(args) == 0 || args[0] != "install" {
		return fmt.Errorf("usage: recall skill install [dir]")
	}
	body := skillContent()
	targets := []string{}
	if len(args) > 1 {
		// The optional argument is a directory. Anything that looks like a flag
		// is a mistake — `recall skill install --force` created a directory
		// named "--force" and wrote the skill into it, which then took a commit
		// and broke every shell loop that iterated over the repo's files.
		if strings.HasPrefix(args[1], "-") {
			return fmt.Errorf("recall skill install takes a directory, not %q\n"+
				"  recall skill install                       # every installed copy\n"+
				"  recall skill install ~/.claude/skills/recall", args[1])
		}
		targets = append(targets, args[1])
	} else {
		for _, s := range installedSkills() {
			if s.present {
				targets = append(targets, s.dir)
			}
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("no installed recall skill found; pass a directory, e.g.\n" +
			"  recall skill install ~/.claude/skills/recall")
	}
	for _, dir := range targets {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		dst := filepath.Join(dir, "SKILL.md")
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			return err
		}
		fmt.Printf("installed %s (%s)\n", dst, shortHash(body))
	}
	return nil
}
