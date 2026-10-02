package main

import (
	"reflect"
	"testing"
)

func TestResolveSkillPrefersNearestProjectCopy(t *testing.T) {
	cands := []Session{
		{SourceID: "/home/u/.pi/agent/skills/neuron/SKILL.md", StartedAt: 9},
		{SourceID: "/work/platform/.agents/skills/neuron/SKILL.md", StartedAt: 1},
		{SourceID: "/work/platform/svc/.agents/skills/neuron/SKILL.md", StartedAt: 1},
		{SourceID: "/work/platform-other/.agents/skills/neuron/SKILL.md", StartedAt: 5},
	}
	all := func(string) bool { return true }

	got, ok := resolveSkill(cands, "/work/platform/svc/api", all)
	if !ok || got.SourceID != "/work/platform/svc/.agents/skills/neuron/SKILL.md" {
		t.Fatalf("nearest project copy: got %+v ok=%v", got, ok)
	}
	// "platform-other" must not match cwd "/work/platform" by string prefix.
	got, _ = resolveSkill(cands, "/work/platform", all)
	if got.SourceID != "/work/platform/.agents/skills/neuron/SKILL.md" {
		t.Fatalf("project root itself: got %+v", got)
	}
	got, _ = resolveSkill(cands, "/elsewhere", all)
	if got.SourceID != "/home/u/.pi/agent/skills/neuron/SKILL.md" {
		t.Fatalf("global fallback: got %+v", got)
	}
}

func TestResolveSkillSkipsMissingFilesAndPicksNewestGlobal(t *testing.T) {
	cands := []Session{
		{SourceID: "/a/.pi/agent/skills/recall/SKILL.md", StartedAt: 1},
		{SourceID: "/b/.agents/skills-old/recall/SKILL.md", StartedAt: 3},
		{SourceID: "/gone/recall/SKILL.md", StartedAt: 7},
	}
	exists := func(p string) bool { return p != "/gone/recall/SKILL.md" }
	got, ok := resolveSkill(cands, "/x", exists)
	if !ok || got.SourceID != "/b/.agents/skills-old/recall/SKILL.md" {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	if _, ok := resolveSkill(nil, "/x", exists); ok {
		t.Fatal("no candidates must not resolve")
	}
}

func TestMemoryFindArgsTagsSourceAndDefaultsLimit(t *testing.T) {
	got := memoryFindArgs([]string{"edit", "tool"})
	want := []string{"edit", "tool", "--tag", "source:memory", "--limit", "5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	got = memoryFindArgs([]string{"x", "--limit", "20"})
	want = []string{"x", "--limit", "20", "--tag", "source:memory"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit limit: got %v want %v", got, want)
	}
}
