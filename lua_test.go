package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
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

// TestLuaCursorKVIncremental exercises the watermark-driven resume path of the
// kv kind: a first pass indexes everything, a no-op pass re-emits nothing, and
// after a new composer lands (higher rowid) only that composer comes back.
// Mirrors the Go adapter's TestCursorIncrementalByRowID.
func TestLuaCursorKVIncremental(t *testing.T) {
	userDir := t.TempDir()
	addCursorComposers(t, userDir, tComposer{
		id: "c1", createdAt: 1700000000000,
		bubbles: []tBubble{{id: "b1", typ: 1, text: "first chat"}},
	})
	db := filepath.Join(userDir, "globalStorage", "state.vscdb")
	ad := luaAdapterForKV(t, "plugins/cursor.lua", db)

	// First pass: index c1, get a non-empty checkpoint.
	s1, m1, next, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(s1) != 1 || len(m1) != 1 || s1[0].SourceID != "c1" {
		t.Fatalf("first pass: sessions=%+v msgs=%+v", s1, m1)
	}
	if next == "" {
		t.Fatal("watermark kind must emit a non-empty checkpoint")
	}

	// No-op pass: nothing changed, so nothing re-emitted.
	s0, _, next0, err := ad.Scan(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if len(s0) != 0 {
		t.Fatalf("no-op resume should yield 0 sessions, got %d", len(s0))
	}

	// A new composer appears with a higher rowid.
	addCursorComposers(t, userDir, tComposer{
		id: "c2", createdAt: 1700000100000,
		bubbles: []tBubble{{id: "b9", typ: 1, text: "second chat"}},
	})
	s2, m2, _, err := ad.Scan(context.Background(), next0)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2) != 1 || s2[0].SourceID != "c2" || len(m2) != 1 || m2[0].Text != "second chat" {
		t.Fatalf("incremental pass should return only c2, got sessions=%+v msgs=%+v", s2, m2)
	}
}

// TestLuaCursorKVResumeAfterEditedComposer proves that touching an existing
// composer's bubble (new bubble row → higher rowid) re-emits that whole
// composer on the next pass, so edits aren't missed.
func TestLuaCursorKVResumeAfterEditedComposer(t *testing.T) {
	userDir := t.TempDir()
	addCursorComposers(t, userDir, tComposer{
		id: "c1", createdAt: 1700000000000,
		bubbles: []tBubble{{id: "b1", typ: 1, text: "original"}},
	})
	db := filepath.Join(userDir, "globalStorage", "state.vscdb")
	ad := luaAdapterForKV(t, "plugins/cursor.lua", db)

	_, _, next, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	// Append a second bubble to the SAME composer (a higher rowid in the table).
	addCursorBubble(t, userDir, "c1", tBubble{id: "b2", typ: 2, text: "a reply"})
	s, m, _, err := ad.Scan(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 1 || s[0].SourceID != "c1" {
		t.Fatalf("edited composer should re-emit c1, got %+v", s)
	}
	// The re-emitted composer carries its full message set (not just the delta),
	// since kv sessions are whole-session replaces, not appends.
	if len(m) != 2 {
		t.Fatalf("re-emit should carry all 2 bubbles, got %d", len(m))
	}
}

// TestLuaParityCursor proves the `kind = "kv"` plumbing reaches Cursor's
// sqlite-backed composers and that cursor.lua reproduces what the Go adapter
// extracts from the same DB. Project mapping (which the Go adapter reads from
// workspaceStorage) is intentionally not in v1 of cursor.lua, so we compare
// every Session field except Project.
func TestLuaParityCursor(t *testing.T) {
	userDir := t.TempDir()
	addCursorComposers(t, userDir,
		tComposer{
			id: "c1", name: "", createdAt: 1700000000000,
			bubbles: []tBubble{
				{id: "b1", typ: 1, text: "investigate the race condition"},
				{id: "b2", typ: 2, text: "Looking now."},
				{id: "b3", typ: 9, text: "[tool] grep result"},
			},
		},
		tComposer{
			id: "c2", name: "named composer", createdAt: 1700000050000,
			bubbles: []tBubble{
				{id: "bA", typ: 1, text: "hello there"},
			},
		},
	)
	addCursorWorkspace(t, userDir, "ws1", "/work/acme-api", "c1", "c2")

	goAd := &CursorAdapter{UserDir: userDir}
	db := filepath.Join(userDir, "globalStorage", "state.vscdb")
	luaAd := luaAdapterForKV(t, "plugins/cursor.lua", db)

	gs, gm, _, err := goAd.Scan(context.Background(), "")
	if err != nil {
		t.Fatalf("go scan: %v", err)
	}
	ls, lm, _, err := luaAd.Scan(context.Background(), "")
	if err != nil {
		t.Fatalf("lua scan: %v", err)
	}
	compareSessionsIgnoreProject(t, "cursor", gs, ls)
	compareMessages(t, "cursor", gm, lm)

	// Fetch round-trips through the same plugin.
	full, err := luaAd.Fetch(context.Background(), "c1")
	if err != nil {
		t.Fatalf("lua fetch: %v", err)
	}
	if len(full) != 3 || full[0].Role != "user" || full[1].Role != "assistant" || full[2].Role != "tool" {
		t.Fatalf("fetch = %+v", full)
	}
}

// luaAdapterForKV loads a kv-kind plugin but points its source at a test DB.
func luaAdapterForKV(t *testing.T, pluginPath, dbPath string) *luaAdapter {
	t.Helper()
	man, err := readLuaManifest(pluginPath)
	if err != nil {
		t.Fatalf("manifest %s: %v", pluginPath, err)
	}
	man.source = dbPath
	return &luaAdapter{path: pluginPath, man: man, batchSessions: 1}
}

// compareSessionsIgnoreProject is compareSessions minus the Project field.
// Used where the Lua port reaches a parity-tested source but its v1 manifest
// doesn't yet model the auxiliary scan that yields the project path.
func compareSessionsIgnoreProject(t *testing.T, label string, want, got []Session) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: session count: go=%d lua=%d", label, len(want), len(got))
	}
	sortSessions(want)
	sortSessions(got)
	for i := range want {
		w, g := want[i], got[i]
		if w.Source != g.Source || w.SourceID != g.SourceID ||
			w.Title != g.Title || w.StartedAt != g.StartedAt || w.EndedAt != g.EndedAt ||
			w.MsgCount != g.MsgCount || w.Append != g.Append {
			t.Errorf("%s: session mismatch:\n  go  = %+v\n  lua = %+v", label, w, g)
		}
	}
}

// TestLuaParityClaudeArrayToolResult guards the real-world case (found by
// `recall plugin test` on a live transcript) where tool_result content is an
// array, not a string. The Go adapter skips it; the Lua plugin must too.
func TestLuaParityClaudeArrayToolResult(t *testing.T) {
	root := t.TempDir()
	raw := `{"type":"assistant","timestamp":"2026-05-01T10:00:10Z","message":{"role":"assistant","content":[{"type":"text","text":"done"},{"type":"tool_result","content":[{"type":"text","text":"ignored array"}]}]}}`
	writeLines(t, filepath.Join(root, "-work-x", "sess-arr.jsonl"),
		claudeUser("run it", "2026-05-01T10:00:00Z", "/work/x"),
		raw,
	)
	assertParity(t, "claude-array-toolresult", &ClaudeAdapter{Root: root}, luaAdapterFor(t, "plugins/claude.lua", root))
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

// TestMergeAdaptersOverride proves a Lua plugin replaces a built-in of the same
// id, and that new ids are appended.
func TestMergeAdaptersOverride(t *testing.T) {
	builtin := &ClaudeAdapter{Root: "/x"}
	override := &luaAdapter{path: "claude.lua", man: luaManifest{id: "claude", kind: "line"}}
	extra := &luaAdapter{path: "obsidian.lua", man: luaManifest{id: "obsidian", kind: "file"}}

	out := mergeAdapters([]Adapter{builtin}, []Adapter{override, extra})
	if len(out) != 2 {
		t.Fatalf("want 2 adapters, got %d", len(out))
	}
	var claude, obsidian Adapter
	for _, a := range out {
		switch a.ID() {
		case "claude":
			claude = a
		case "obsidian":
			obsidian = a
		}
	}
	if _, ok := claude.(*luaAdapter); !ok {
		t.Errorf("lua plugin should override built-in claude, got %T", claude)
	}
	if obsidian == nil {
		t.Error("new lua source should be appended")
	}
}

// TestLuaRunawayPluginSkipped proves the per-file timeout interrupts a runaway
// transform: the bad file is skipped and the scan returns instead of hanging.
func TestLuaRunawayPluginSkipped(t *testing.T) {
	root := t.TempDir()
	plug := filepath.Join(root, "runaway.lua")
	if err := os.WriteFile(plug, []byte(`return {
	  id = "runaway", kind = "line", glob = "*.jsonl", resume = "",
	  line = function(line, st) while true do end end,
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeLines(t, filepath.Join(root, "data", "x.jsonl"), `{"a":1}`)

	man, err := readLuaManifest(plug)
	if err != nil {
		t.Fatal(err)
	}
	man.roots = []string{filepath.Join(root, "data")}
	ad := &luaAdapter{path: plug, man: man, batchSessions: 1}

	old := luaFileTimeout
	luaFileTimeout = 150 * time.Millisecond
	defer func() { luaFileTimeout = old }()

	done := make(chan struct{})
	var sessions []Session
	go func() {
		sessions, _, _, _ = ad.Scan(context.Background(), "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runaway plugin was not interrupted by the per-file timeout")
	}
	if len(sessions) != 0 {
		t.Fatalf("runaway plugin should yield no sessions, got %d", len(sessions))
	}
}

// TestPluginTestSample exercises the `recall plugin test <file> <sample>`
// dry-run path for both kinds.
func TestPluginTestSample(t *testing.T) {
	root := t.TempDir()

	// line kind
	jsonl := filepath.Join(root, "s.jsonl")
	writeLines(t, jsonl,
		claudeUser("hello sample", "2026-05-01T10:00:00Z", "/work/x"),
		claudeAssistant("hi", "2026-05-01T10:00:05Z"),
	)
	man, err := readLuaManifest("plugins/claude.lua")
	if err != nil {
		t.Fatal(err)
	}
	ad := &luaAdapter{path: "plugins/claude.lua", man: man}
	ss, mm, err := ad.testSample(jsonl)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].SourceID != "s" || len(mm) != 2 {
		t.Fatalf("line sample = %+v / %+v", ss, mm)
	}

	// file kind
	md := filepath.Join(root, "note.md")
	writeLines(t, md, "# Title", "body text here")
	man2, err := readLuaManifest("plugins/obsidian.lua")
	if err != nil {
		t.Fatal(err)
	}
	ad2 := &luaAdapter{path: "plugins/obsidian.lua", man: man2}
	ss2, mm2, err := ad2.testSample(md)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss2) != 1 || ss2[0].Title != "Title" || len(mm2) != 1 || mm2[0].Role != "note" {
		t.Fatalf("file sample = %+v / %+v", ss2, mm2)
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

// TestIDFromRelatedKey pins the related-key → header-id recovery used by the
// kv watermark scan, including the edge cases the reviewer flagged: the
// first-delimiter split (correct when ids can't contain relSuf), empty-id
// rejection, and prefix/suffix mismatches.
func TestIDFromRelatedKey(t *testing.T) {
	cases := []struct {
		name          string
		key, pre, suf string
		wantID        string
		wantOK        bool
	}{
		{"cursor shape", "bubbleId:c1:b2", "bubbleId:", ":", "c1", true},
		{"uuid id", "bubbleId:0006e07f-93cf:bX", "bubbleId:", ":", "0006e07f-93cf", true},
		{"first-delimiter split", "p:a:b:c", "p:", ":", "a", true},
		{"empty id rejected", "bubbleId::b2", "bubbleId:", ":", "", false},
		{"no suffix present", "bubbleId:c1", "bubbleId:", ":", "", false},
		{"wrong prefix", "other:c1:b2", "bubbleId:", ":", "", false},
		{"empty suffix takes rest", "note:c1", "note:", "", "c1", true},
		{"empty suffix empty rest", "note:", "note:", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, ok := idFromRelatedKey(c.key, c.pre, c.suf)
			if id != c.wantID || ok != c.wantOK {
				t.Fatalf("idFromRelatedKey(%q,%q,%q) = (%q,%v), want (%q,%v)",
					c.key, c.pre, c.suf, id, ok, c.wantID, c.wantOK)
			}
		})
	}
}

// TestAllShippedPluginsValid guards every plugins/*.lua as a shippable
// artifact: it must load in the sandbox, declare a valid manifest, expose the
// transform function its kind requires, and carry a resume template that
// OpenURL can substitute. This catches a broken or half-finished plugin the
// moment it lands, without needing a bespoke parity test for each one.
func TestAllShippedPluginsValid(t *testing.T) {
	paths, err := filepath.Glob("plugins/*.lua")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) < 5 {
		t.Fatalf("expected at least 5 shipped plugins, found %d (%v)", len(paths), paths)
	}

	// transform fn required per kind.
	wantFn := map[string]string{"line": "line", "file": "file", "kv": "session"}
	seenID := map[string]string{}

	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			man, err := readLuaManifest(p)
			if err != nil {
				t.Fatalf("manifest invalid: %v", err)
			}
			if prev, dup := seenID[man.id]; dup {
				t.Fatalf("duplicate plugin id %q (also in %s)", man.id, prev)
			}
			seenID[man.id] = p

			// kind is constrained by readLuaManifest, but assert the fn exists.
			L := newLuaState(context.Background())
			defer L.Close()
			mod, err := loadModule(L, p)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			fn := wantFn[man.kind]
			if _, ok := mod.RawGetString(fn).(*lua.LFunction); !ok {
				t.Errorf("kind %q requires a %s() function", man.kind, fn)
			}

			// A shipped plugin must be resumable into its source tool.
			if man.resume == "" || !strings.Contains(man.resume, "{id}") {
				t.Errorf("resume = %q, want a non-empty template containing {id}", man.resume)
			}
			ad := &luaAdapter{path: p, man: man}
			if got := ad.OpenURL("ABC123"); !strings.Contains(got, "ABC123") {
				t.Errorf("OpenURL did not substitute id: %q", got)
			}
			// Available() must not panic regardless of host filesystem.
			_ = ad.Available()
		})
	}
}
