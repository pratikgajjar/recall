package main

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// luaAdapterFor loads a repo plugin but points its roots at a test fixture dir.
func luaAdapterFor(t *testing.T, pluginPath, root string) *luaAdapter {
	t.Helper()
	man, err := readLuaManifest(pluginPath)
	if err != nil {
		t.Fatalf("manifest %s: %v", pluginPath, err)
	}
	man.roots = []string{root}
	return &luaAdapter{path: pluginPath, man: man, batchSessions: 1}
}

func sortSessions(s []Session) {
	sort.Slice(s, func(i, j int) bool { return s[i].SourceID < s[j].SourceID })
}
func sortMessages(m []Message) {
	sort.Slice(m, func(i, j int) bool {
		if m[i].SourceID != m[j].SourceID {
			return m[i].SourceID < m[j].SourceID
		}
		return m[i].Idx < m[j].Idx
	})
}

// assertParity scans a fixture with both the built-in Go adapter and the Lua
// plugin and asserts the normalized output is identical.
func assertParity(t *testing.T, label string, goAd, luaAd Adapter) {
	t.Helper()
	gs, gm, _, err := goAd.Scan(context.Background(), "")
	if err != nil {
		t.Fatalf("%s: go scan: %v", label, err)
	}
	ls, lm, _, err := luaAd.Scan(context.Background(), "")
	if err != nil {
		t.Fatalf("%s: lua scan: %v", label, err)
	}
	compareSessions(t, label, gs, ls)
	compareMessages(t, label, gm, lm)
}

func compareSessions(t *testing.T, label string, want, got []Session) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: session count: go=%d lua=%d", label, len(want), len(got))
	}
	sortSessions(want)
	sortSessions(got)
	for i := range want {
		w, g := want[i], got[i]
		if w.Source != g.Source || w.SourceID != g.SourceID || w.Project != g.Project ||
			w.Title != g.Title || w.StartedAt != g.StartedAt || w.EndedAt != g.EndedAt ||
			w.MsgCount != g.MsgCount || w.Append != g.Append {
			t.Errorf("%s: session mismatch:\n  go  = %+v\n  lua = %+v", label, w, g)
		}
	}
}

func compareMessages(t *testing.T, label string, want, got []Message) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: message count: go=%d lua=%d", label, len(want), len(got))
	}
	sortMessages(want)
	sortMessages(got)
	for i := range want {
		w, g := want[i], got[i]
		if w.SourceID != g.SourceID || w.Idx != g.Idx || w.Role != g.Role || w.TS != g.TS || w.Text != g.Text {
			t.Errorf("%s: message %d mismatch:\n  go  = %+v\n  lua = %+v", label, i, w, g)
		}
	}
}

func TestLuaParityClaude(t *testing.T) {
	root := t.TempDir()
	writeLines(t, filepath.Join(root, "-work-acme-api", "sess-claude-1.jsonl"),
		claudeUser("fix the auth bug", "2026-05-01T10:00:00Z", "/work/acme-api"),
		claudeAssistant("On it.", "2026-05-01T10:00:05Z"),
	)
	assertParity(t, "claude", &ClaudeAdapter{Root: root}, luaAdapterFor(t, "plugins/claude.lua", root))
}

func TestLuaParityCodex(t *testing.T) {
	root := t.TempDir()
	writeLines(t, codexPath(root, "sess-codex-1"),
		codexMeta("sess-codex-1", "/work/api", "2026-05-02T09:00:00Z"),
		codexMsg("user", "add retry logic", "2026-05-02T09:00:03Z"),
		codexMsg("assistant", "Added.", "2026-05-02T09:00:08Z"),
		codexCall("shell", `{"cmd":"go test"}`, "2026-05-02T09:00:09Z"),
	)
	assertParity(t, "codex", &CodexAdapter{Root: root}, luaAdapterFor(t, "plugins/codex.lua", root))
}

func TestLuaParityPi(t *testing.T) {
	root := t.TempDir()
	writeLines(t, piPath(root, "sess-pi-1"),
		piSession("sess-pi-1", "/work/web", "2026-05-03T08:00:00Z"),
		piMsg("user", "refactor the parser", "2026-05-03T08:00:02Z"),
		piMsg("assistant", "Sure.", "2026-05-03T08:00:06Z"),
	)
	assertParity(t, "pi", &PiAdapter{Root: root}, luaAdapterFor(t, "plugins/pi.lua", root))
}

// TestLuaParityClaudeIncremental proves the offset-resume / append path matches
// too — the hard part the Go adapters get right.
func TestLuaParityClaudeIncremental(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "-work-acme-api", "sess-claude-2.jsonl")
	writeLines(t, path,
		claudeUser("start the refactor", "2026-05-01T10:00:00Z", "/work/acme-api"),
		claudeAssistant("Sure.", "2026-05-01T10:00:05Z"),
	)
	goAd := &ClaudeAdapter{Root: root}
	luaAd := luaAdapterFor(t, "plugins/claude.lua", root)

	_, _, goNext, err := goAd.Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, luaNext, err := luaAd.Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	appendLines(t, path, claudeUser("now run the tests", "2026-05-01T10:01:00Z", "/work/acme-api"))

	gs, gm, _, err := goAd.Scan(context.Background(), goNext)
	if err != nil {
		t.Fatal(err)
	}
	ls, lm, _, err := luaAd.Scan(context.Background(), luaNext)
	if err != nil {
		t.Fatal(err)
	}
	compareSessions(t, "claude-incremental", gs, ls)
	compareMessages(t, "claude-incremental", gm, lm)
	if len(ls) != 1 || !ls[0].Append || len(lm) != 1 || lm[0].Idx != 2 || lm[0].Text != "now run the tests" {
		t.Fatalf("unexpected append delta: sessions=%+v msgs=%+v", ls, lm)
	}
}

// TestLuaObsidian proves recall is not chat-specific: a Markdown vault indexes
// through the same contract, with no Go changes.
func TestLuaObsidian(t *testing.T) {
	root := t.TempDir()
	note := filepath.Join(root, "vault", "architecture.md")
	writeLines(t, note, "# Indexing design", "", "We use FTS5 over excerpts.")

	ad := luaAdapterFor(t, "plugins/obsidian.lua", root)
	sessions, msgs, _, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 note-session, got %d", len(sessions))
	}
	s := sessions[0]
	if s.Source != "obsidian" || s.SourceID != note {
		t.Errorf("session id = %q (want %q)", s.SourceID, note)
	}
	if s.Title != "Indexing design" {
		t.Errorf("title = %q (want first heading)", s.Title)
	}
	if len(msgs) != 1 || msgs[0].Role != "note" {
		t.Fatalf("want 1 note record, got %+v", msgs)
	}
	if !strings.Contains(msgs[0].Text, "FTS5 over excerpts") {
		t.Errorf("note body not indexed: %q", msgs[0].Text)
	}

	// Fetch round-trips the full note by its id.
	full, err := ad.Fetch(context.Background(), note)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 1 || !strings.Contains(full[0].Text, "Indexing design") {
		t.Fatalf("fetch = %+v", full)
	}
}

// TestLuaSandbox proves the VM has no I/O or code-loading escape hatches.
func TestLuaSandbox(t *testing.T) {
	L := newLuaState(context.Background())
	defer L.Close()
	if err := L.DoString(`
		assert(os == nil, "os must be unavailable")
		assert(io == nil, "io must be unavailable")
		assert(require == nil, "require must be unavailable")
		assert(load == nil, "load must be unavailable")
		assert(loadfile == nil, "loadfile must be unavailable")
		assert(dofile == nil, "dofile must be unavailable")
		assert(type(recall.get) == "function", "recall helpers must be present")
	`); err != nil {
		t.Fatalf("sandbox check failed: %v", err)
	}
}
