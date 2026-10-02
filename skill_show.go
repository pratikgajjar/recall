package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// recall skill <name> prints an installed skill's SKILL.md by name, so an agent's
// prompt can list skills as name + description without a full path per skill.
// Skills come from the "skill" source (Pi's mirrored skill catalog).

// projectSkillDirs are the project-local skill roots Pi discovers from the
// working directory up through its ancestors.
var projectSkillDirs = []string{"/.agents/skills/", "/.pi/skills/"}

// resolveSkill picks the copy of a skill the agent in cwd would see: the
// project-local copy under the nearest ancestor of cwd, else the global copy
// most recently mirrored (the catalog rewrites a file when a session syncs it).
func resolveSkill(cands []Session, cwd string, exists func(string) bool) (Session, bool) {
	cwd = filepath.Clean(cwd)
	var best Session
	bestRoot := -1
	var globals []Session
	for _, c := range cands {
		if !exists(c.SourceID) {
			continue
		}
		root, local := projectRoot(c.SourceID)
		if !local {
			globals = append(globals, c)
			continue
		}
		if (cwd == root || strings.HasPrefix(cwd, root+"/")) && len(root) > bestRoot {
			best, bestRoot = c, len(root)
		}
	}
	if bestRoot >= 0 {
		return best, true
	}
	if len(globals) == 0 {
		return Session{}, false
	}
	sort.SliceStable(globals, func(i, j int) bool { return globals[i].StartedAt > globals[j].StartedAt })
	return globals[0], true
}

func projectRoot(path string) (string, bool) {
	for _, dir := range projectSkillDirs {
		if i := strings.Index(path, dir); i > 0 {
			return path[:i], true
		}
	}
	return "", false
}

func (ix *Index) skillCandidates(name string) ([]Session, error) {
	rows, err := ix.db.Query(`SELECT source_id, COALESCE(project,''), COALESCE(started_at,0)
		FROM sessions WHERE source = 'skill' AND title = ?`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		s := Session{Source: "skill", Title: name}
		if err := rows.Scan(&s.SourceID, &s.Project, &s.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func lookupSkill(name, cwd string) (Session, bool, error) {
	ix, err := openIndexRead(defaultIndexPath())
	if err != nil {
		return Session{}, false, err
	}
	defer ix.Close()
	cands, err := ix.skillCandidates(name)
	if err != nil && err != sql.ErrNoRows {
		return Session{}, false, err
	}
	s, ok := resolveSkill(cands, cwd, fileExists)
	return s, ok, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// refreshIndex re-runs `recall index` once when a lookup misses; tests stub it,
// since os.Executable() is the test binary there.
var refreshIndex = func() {
	if exe, err := os.Executable(); err == nil {
		_ = exec.Command(exe, "index").Run()
	}
}

func runSkillShow(args []string) error {
	fs := flag.NewFlagSet("skill", flag.ExitOnError)
	cwdFlag := fs.String("cwd", "", "resolve project-local skills from this directory (default: current)")
	flagArgs, posArgs := splitFlagsAndArgs(args, nil)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(posArgs) != 1 {
		return fmt.Errorf("usage: recall skill <name> [--cwd dir]")
	}
	name := posArgs[0]
	cwd := *cwdFlag
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	s, ok, err := lookupSkill(name, cwd)
	if err != nil || !ok {
		// A skill mirrored on this prompt, or the whole index, may not exist yet; refresh once.
		refreshIndex()
		s, ok, err = lookupSkill(name, cwd)
	}
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no installed skill named %q; search with: recall find %q --tag source:skill", name, name)
	}
	body, err := os.ReadFile(s.SourceID)
	if err != nil {
		return err
	}
	base := s.Project
	if base == "" {
		base = filepath.Dir(s.SourceID)
	}
	fmt.Printf("Skill: %s\nBase: %s (resolve relative paths in this skill against Base)\n\n%s", name, base, body)
	return nil
}

// recall memory [terms] is `recall find --tag source:memory`, five hits by
// default: memory lookups are frequent enough to deserve their own verb.
func memoryFindArgs(args []string) []string {
	out := append([]string{}, args...)
	out = append(out, "--tag", "source:memory")
	for _, a := range args {
		if a == "--limit" || a == "-limit" || strings.HasPrefix(a, "--limit=") || strings.HasPrefix(a, "-limit=") {
			return out
		}
	}
	return append(out, "--limit", "5")
}

func runMemory(args []string) error { return runFind(memoryFindArgs(args)) }
