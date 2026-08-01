package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func ftsRowCount(t *testing.T, ix *Index, pk string) int {
	t.Helper()
	var n int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE session_pk = ?`, pk).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func dupIdxRows(t *testing.T, ix *Index) int {
	t.Helper()
	var n int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM (
		SELECT session_pk, idx, COUNT(*) c FROM messages_fts GROUP BY session_pk, idx HAVING c > 1)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func sessionMsgCount(t *testing.T, ix *Index, pk string) int {
	t.Helper()
	var n int
	if err := ix.db.QueryRow(`SELECT msg_count FROM sessions WHERE id = ?`, pk).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIngestAppendInvariants(t *testing.T) {
	root := t.TempDir()
	path := piPath(root, "sess-e2e-1")
	writeLines(t, path,
		piSession("sess-e2e-1", "/work/web", "2026-05-03T08:00:00Z"),
		piMsg("user", "alpha bravo charlie", "2026-05-03T08:00:02Z"),
		piMsg("assistant", "delta echo", "2026-05-03T08:00:06Z"),
	)
	ad := &PiAdapter{Root: root}
	ix := newTestIndex(t)
	pk := "pi:sess-e2e-1"

	// Full ingest.
	s, m, next, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.IngestBatch(context.Background(), "pi", s, m); err != nil {
		t.Fatal(err)
	}
	if got := sessionMsgCount(t, ix, pk); got != 2 {
		t.Fatalf("msg_count after full = %d, want 2", got)
	}
	if got := ftsRowCount(t, ix, pk); got != 2 {
		t.Fatalf("fts rows after full = %d, want 2", got)
	}

	// Append one message and ingest via the append path.
	appendLines(t, path, piMsg("user", "foxtrot golf", "2026-05-03T08:10:00Z"))
	s2, m2, _, err := ad.Scan(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2) != 1 || !s2[0].Append {
		t.Fatalf("expected an Append session, got %+v", s2)
	}
	if err := ix.IngestBatch(context.Background(), "pi", s2, m2); err != nil {
		t.Fatal(err)
	}

	// Invariant: msg_count tracks the appended delta, FTS has no duplicates.
	if got := sessionMsgCount(t, ix, pk); got != 3 {
		t.Errorf("msg_count after append = %d, want 3", got)
	}
	if got := ftsRowCount(t, ix, pk); got != 3 {
		t.Errorf("fts rows after append = %d, want 3", got)
	}
	if got := dupIdxRows(t, ix); got != 0 {
		t.Errorf("found %d duplicated (session_pk, idx) rows after append", got)
	}

	// The appended content is searchable.
	hits, err := ix.Search("foxtrot", SearchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].SessionID != pk {
		t.Fatalf("appended message not found via search: %+v", hits)
	}
}

func TestFullReingestReplacesNotDuplicates(t *testing.T) {
	root := t.TempDir()
	path := piPath(root, "sess-e2e-2")
	writeLines(t, path,
		piSession("sess-e2e-2", "/work/web", "2026-05-03T08:00:00Z"),
		piMsg("user", "hotel india", "2026-05-03T08:00:02Z"),
	)
	ad := &PiAdapter{Root: root}
	ix := newTestIndex(t)
	pk := "pi:sess-e2e-2"

	// Ingest the same full session twice (e.g. a forced --full re-read).
	for i := 0; i < 2; i++ {
		s, m, _, err := ad.Scan(context.Background(), "") // prev="" => always full
		if err != nil {
			t.Fatal(err)
		}
		if err := ix.IngestBatch(context.Background(), "pi", s, m); err != nil {
			t.Fatal(err)
		}
	}
	if got := ftsRowCount(t, ix, pk); got != 1 {
		t.Errorf("fts rows after double full ingest = %d, want 1 (delete+reinsert)", got)
	}
	if got := dupIdxRows(t, ix); got != 0 {
		t.Errorf("found %d duplicate rows after re-ingest", got)
	}
}

func TestSearchTitleAndProjectFilter(t *testing.T) {
	root := t.TempDir()
	writeLines(t, piPath(root, "sess-f1"),
		piSession("sess-f1", "/work/web", "2026-05-03T08:00:00Z"),
		piMsg("user", "kilo lima mike", "2026-05-03T08:00:02Z"),
	)
	// a second session in a different project
	writeLines(t, filepath.Join(root, "-work-api", "20260504T080000_sess-f2.jsonl"),
		piSession("sess-f2", "/work/api", "2026-05-04T08:00:00Z"),
		piMsg("user", "kilo november", "2026-05-04T08:00:02Z"),
	)
	ad := &PiAdapter{Root: root}
	ix := newTestIndex(t)
	s, m, _, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.IngestBatch(context.Background(), "pi", s, m); err != nil {
		t.Fatal(err)
	}

	all, err := ix.Search("kilo", SearchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 hits for 'kilo', got %d", len(all))
	}
	scoped, err := ix.Search("kilo", SearchOpts{Project: "/work/api"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 || scoped[0].SessionID != "pi:sess-f2" {
		t.Fatalf("project filter failed: %+v", scoped)
	}
}

func TestFtsTerms(t *testing.T) {
	cases := map[string]string{
		"import cycle":           `"import"|"cycle"`,
		`"import cycle"`:         `"import cycle"`,
		`fix "import cycle" now`: `"fix"|"import cycle"|"now"`,
		`we:ird (chars)*`:        `"weird"|"chars"`,
		``:                       `""`,
		`"unclosed phrase`:       `"unclosed phrase"`,
	}
	for in, want := range cases {
		if got := strings.Join(ftsTerms(in), "|"); got != want {
			t.Errorf("ftsTerms(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveRepo(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "src", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveRepo(sub); got != root {
		t.Errorf("resolveRepo(%q) = %q, want %q", sub, got, root)
	}
	// no .git anywhere above: returns the abs input unchanged
	bare := t.TempDir()
	if got := resolveRepo(bare); got != bare {
		t.Errorf("resolveRepo(%q) = %q, want same", bare, got)
	}
	if got := resolveRepo(""); got != "" {
		t.Errorf("resolveRepo(\"\") = %q, want \"\"", got)
	}
}

// Ranking sessions against each other means one hit per session. Searching
// INSIDE a session means the opposite: the caller picked the session and is
// asking where in it, so every match is a location worth returning.
func TestSessionScopedSearchReturnsEveryMatch(t *testing.T) {
	ix := newTestIndex(t)
	msgs := []Message{
		{SourceID: "s1", Idx: 0, Role: "user", Text: "we should add a widget cache"},
		{SourceID: "s1", Idx: 5, Role: "assistant", Text: "the widget cache is now warm"},
		{SourceID: "s1", Idx: 9, Role: "user", Text: "is the widget cache still correct"},
	}
	if err := ix.IngestBatch(context.Background(), "pi",
		[]Session{{Source: "pi", SourceID: "s1", Title: "caching", MsgCount: len(msgs)}},
		msgs); err != nil {
		t.Fatal(err)
	}
	scoped, err := ix.Search("widget cache", SearchOpts{SessionID: "pi:s1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) < 3 {
		t.Fatalf("scoped search should report every location, got %d: %+v", len(scoped), scoped)
	}
	idx := map[int]bool{}
	for _, h := range scoped {
		idx[h.MsgIdx] = true
	}
	for _, want := range []int{0, 5, 9} {
		if !idx[want] {
			t.Errorf("missing match at msg %d (got %v)", want, idx)
		}
	}
	// Unscoped search must still collapse to one row per session.
	all, err := ix.Search("widget cache", SearchOpts{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("unscoped search should be one hit per session, got %d", len(all))
	}
}

// Hits that all share a session state the session once instead of repeating a
// 45-char id on every line.
func TestPrintHitsCompactsSingleSession(t *testing.T) {
	hits := []Hit{
		{SessionID: "pi:abc", Source: "pi", Title: "t", MsgIdx: 3, Role: "user", Snippet: "one"},
		{SessionID: "pi:abc", Source: "pi", Title: "t", MsgIdx: 9, Role: "assistant", Snippet: "two"},
	}
	var b strings.Builder
	printHits(&b, hits)
	out := b.String()
	if strings.Count(out, "pi:abc") != 1 {
		t.Errorf("session id should appear once, got %d:\n%s", strings.Count(out, "pi:abc"), out)
	}
	for _, want := range []string{"msg=3", "msg=9", "one", "two", "(2 hits)"} {
		if !strings.Contains(out, want) {
			t.Errorf("compact output lost %q:\n%s", want, out)
		}
	}
	// Mixed sessions keep the per-hit form, which carries each id.
	hits[1].SessionID = "pi:def"
	var b2 strings.Builder
	printHits(&b2, hits)
	if !strings.Contains(b2.String(), "pi:abc") || !strings.Contains(b2.String(), "pi:def") {
		t.Errorf("mixed-session output must carry every id:\n%s", b2.String())
	}
}

// Scoped to a session, a title match points at no message (idx -1) and tells
// the caller only what they already knew. It must not occupy a hit slot.
func TestSessionScopedSearchDropsTitleMatches(t *testing.T) {
	ix := newTestIndex(t)
	if err := ix.IngestBatch(context.Background(), "pi",
		[]Session{{Source: "pi", SourceID: "s1", Title: "widget cache design", MsgCount: 2}},
		[]Message{
			{SourceID: "s1", Idx: 0, Role: "user", Text: "how should the widget cache expire"},
			{SourceID: "s1", Idx: 1, Role: "assistant", Text: "the widget cache expires lazily"},
		}); err != nil {
		t.Fatal(err)
	}
	scoped, err := ix.Search("widget cache", SearchOpts{SessionID: "pi:s1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) == 0 {
		t.Fatal("expected message hits")
	}
	for _, h := range scoped {
		if h.MsgIdx < 0 {
			t.Errorf("scoped search returned a title row: %+v", h)
		}
	}
	// Unscoped, a title match is still how you discover the session.
	all, err := ix.Search("widget cache design", SearchOpts{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Error("unscoped search must still match on title")
	}
}

// PruneMissing deletes user-visible data, so its refusals matter more than its
// deletions: an empty or partial scan must never be read as "all gone".
func TestPruneMissingRefusesWeakEvidence(t *testing.T) {
	newIx := func(t *testing.T) *Index {
		ix := newTestIndex(t)
		var sess []Session
		var msgs []Message
		for _, id := range []string{"a", "b", "c"} {
			sess = append(sess, Session{Source: "pi", SourceID: id, Title: id, MsgCount: 1})
			msgs = append(msgs, Message{SourceID: id, Idx: 0, Role: "user", Text: "text " + id})
		}
		if err := ix.IngestBatch(context.Background(), "pi", sess, msgs); err != nil {
			t.Fatal(err)
		}
		return ix
	}
	count := func(t *testing.T, ix *Index) int {
		var n int
		if err := ix.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// A scan that emitted nothing proves nothing — the source may have been
	// unreadable. It must not empty the index.
	ix := newIx(t)
	if n, err := ix.PruneMissing("pi", map[string]bool{}); err != nil || n != 0 {
		t.Fatalf("empty scan pruned %d (err %v); must be 0", n, err)
	}
	if got := count(t, ix); got != 3 {
		t.Fatalf("empty scan destroyed rows: %d left", got)
	}

	// A scan that saw everything prunes nothing.
	ix = newIx(t)
	if n, _ := ix.PruneMissing("pi", map[string]bool{"a": true, "b": true, "c": true}); n != 0 {
		t.Errorf("pruned %d when everything was seen", n)
	}

	// Only what a complete scan genuinely missed is removed, and only for that
	// source.
	ix = newIx(t)
	if err := ix.IngestBatch(context.Background(), "claude",
		[]Session{{Source: "claude", SourceID: "z", Title: "z", MsgCount: 1}},
		[]Message{{SourceID: "z", Idx: 0, Role: "user", Text: "other source"}}); err != nil {
		t.Fatal(err)
	}
	n, err := ix.PruneMissing("pi", map[string]bool{"a": true})
	if err != nil || n != 2 {
		t.Fatalf("pruned %d (err %v), want 2", n, err)
	}
	if got := count(t, ix); got != 2 { // pi:a + claude:z
		t.Errorf("wrong rows left: %d", got)
	}
	var other int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE source='claude'`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if other != 1 {
		t.Error("pruning one source touched another")
	}
	// The pruned session must be gone from search too, not just the table.
	hits, err := ix.Search("text b", SearchOpts{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.SessionID == "pi:b" {
			t.Error("pruned session still searchable — FTS rows leaked")
		}
	}
}

// The FTS delete cannot seek (session_pk is UNINDEXED), so it is skipped when
// the session is new — 71% of a cold index was spent scanning to delete nothing.
// Skipping it must not leak duplicate rows when a session IS re-ingested.
func TestReingestReplacesRatherThanDuplicates(t *testing.T) {
	ix := newTestIndex(t)
	ingest := func(text string) {
		t.Helper()
		if err := ix.IngestBatch(context.Background(), "pi",
			[]Session{{Source: "pi", SourceID: "s1", Title: "t", MsgCount: 2}},
			[]Message{
				{SourceID: "s1", Idx: 0, Role: "user", Text: text + " one"},
				{SourceID: "s1", Idx: 1, Role: "assistant", Text: text + " two"},
			}); err != nil {
			t.Fatal(err)
		}
	}
	rows := func() int {
		t.Helper()
		var n int
		if err := ix.db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE session_pk='pi:s1'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	ingest("first")
	if got := rows(); got != 2 {
		t.Fatalf("first ingest wrote %d rows, want 2", got)
	}
	ingest("second") // same session again: the delete must fire this time
	if got := rows(); got != 2 {
		t.Fatalf("re-ingest left %d rows, want 2 — the skip leaked duplicates", got)
	}
	// The replacement must be visible: old text gone, new text findable.
	hits, err := ix.Search("first", SearchOpts{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("stale text still searchable after re-ingest: %+v", hits)
	}
	if hits, err = ix.Search("second", SearchOpts{Limit: 5}); err != nil || len(hits) == 0 {
		t.Errorf("replacement text not searchable (%v)", err)
	}
}

// The seekable delete relies on recorded rowid runs. It must replace exactly
// the session's own rows — never a neighbour's, never leaving orphans — and
// must still work for sessions indexed before v5, which have no run recorded.
func TestSeekableDeleteReplacesExactly(t *testing.T) {
	ix := newTestIndex(t)
	put := func(sid, text string, n int) {
		t.Helper()
		var msgs []Message
		for i := 0; i < n; i++ {
			msgs = append(msgs, Message{SourceID: sid, Idx: i, Role: "user",
				Text: text + " " + strconv.Itoa(i)})
		}
		if err := ix.IngestBatch(context.Background(), "pi",
			[]Session{{Source: "pi", SourceID: sid, Title: sid, MsgCount: n}}, msgs); err != nil {
			t.Fatal(err)
		}
	}
	count := func(pk string) int {
		t.Helper()
		var n int
		if err := ix.db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE session_pk=?`, pk).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	put("a", "alpha", 3)
	put("b", "bravo", 4)
	put("c", "charlie", 2)
	put("b", "bravo2", 5) // rewrite the middle session

	if got := count("pi:b"); got != 5 {
		t.Errorf("pi:b has %d rows, want 5", got)
	}
	for pk, want := range map[string]int{"pi:a": 3, "pi:c": 2} {
		if got := count(pk); got != want {
			t.Errorf("%s has %d rows, want %d — the range delete hit a neighbour", pk, got, want)
		}
	}
	// Old text gone, new text present.
	if h, _ := ix.Search("bravo0", SearchOpts{Limit: 5}); len(h) != 0 {
		t.Error("stale rows survived the range delete")
	}
	if h, _ := ix.Search("bravo2", SearchOpts{Limit: 5}); len(h) == 0 {
		t.Error("replacement rows missing")
	}
	// Neighbours still searchable.
	if h, _ := ix.Search("alpha", SearchOpts{Limit: 5}); len(h) == 0 {
		t.Error("neighbour session lost its rows")
	}

	// Pre-v5 shape: rows present, no recorded range. The scanning fallback must
	// still replace them cleanly.
	if _, err := ix.db.Exec(`DELETE FROM msg_ranges WHERE session_pk='pi:a'`); err != nil {
		t.Fatal(err)
	}
	put("a", "alpha3", 2)
	if got := count("pi:a"); got != 2 {
		t.Errorf("pre-v5 fallback left %d rows, want 2", got)
	}
	if h, _ := ix.Search("alpha0", SearchOpts{Limit: 5}); len(h) != 0 {
		t.Error("pre-v5 fallback left stale rows")
	}
}

// Appended rows form a second run; it must be recorded or a later delete
// orphans them in the FTS table.
func TestAppendedRowsAreDeletable(t *testing.T) {
	ix := newTestIndex(t)
	base := []Message{{SourceID: "s", Idx: 0, Role: "user", Text: "first message"}}
	if err := ix.IngestBatch(context.Background(), "pi",
		[]Session{{Source: "pi", SourceID: "s", Title: "s", MsgCount: 1}}, base); err != nil {
		t.Fatal(err)
	}
	if err := ix.IngestBatch(context.Background(), "pi",
		[]Session{{Source: "pi", SourceID: "s", Append: true, MsgCount: 1}},
		[]Message{{SourceID: "s", Idx: 1, Role: "user", Text: "appended message"}}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE session_pk='pi:s'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("want 2 rows after append, got %d", n)
	}
	// Full re-ingest must clear BOTH runs.
	if err := ix.IngestBatch(context.Background(), "pi",
		[]Session{{Source: "pi", SourceID: "s", Title: "s", MsgCount: 1}},
		[]Message{{SourceID: "s", Idx: 0, Role: "user", Text: "replaced"}}); err != nil {
		t.Fatal(err)
	}
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE session_pk='pi:s'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("appended run orphaned: %d rows left, want 1", n)
	}
	if h, _ := ix.Search("appended", SearchOpts{Limit: 5}); len(h) != 0 {
		t.Error("orphaned appended row still searchable")
	}
}

// Tool-call arguments belong in a rendered transcript but not in the search
// index. Indexing command text collapsed rank@1 from 10 to 1 against real agent
// behaviour, because a shell command shares enough vocabulary with an unrelated
// query to win the AND pass. Only the argument is stripped, never the marker:
// 13% of messages are nothing but a marker, and emptying those drops them from
// the index entirely.
func TestTrimTextDropsToolArgsButKeepsMarker(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"[tool:bash] git log --oneline -5", "[tool:bash]"},
		{"[tool_use:Grep] pattern here", "[tool_use:Grep]"},
		{"[tool:bash]", "[tool:bash]"},
		{"Let me check.\n[tool:bash] ls -la", "Let me check.\n[tool:bash]"},
		{"no markers here", "no markers here"},
		// A marker quoted inside prose is content, not scaffolding.
		{"the marker is [tool:bash] in text", "the marker is [tool:bash] in text"},
	} {
		if got := trimText(c.in); got != c.want {
			t.Errorf("trimText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A marker-only message must survive: emptying it would drop the document.
	if got := trimText("[tool:bash] some command"); got == "" {
		t.Error("marker-only message trimmed to empty — it would leave the index")
	}
}

// codex writes "[call name] args" rather than "[tool:name] args", and the strip
// must cover it: its arguments were reaching the index even after the tool/
// tool_use forms were handled.
func TestTrimTextStripsCodexCallArgs(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`[call linear_graphql] {"query":"..."}`, "[call linear_graphql]"},
		{"[call shell] ls -la", "[call shell]"},
		{"[call shell]", "[call shell]"},
	} {
		if got := trimText(c.in); got != c.want {
			t.Errorf("trimText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A raw "sql: no rows in result set" tells an agent nothing: it cannot tell a
// typo from a pruned session, so it retries blindly. Three of 329 calls in the
// real workload hit this path, all with mistyped ids.
func TestMissingSessionErrorIsActionable(t *testing.T) {
	ix := newTestIndex(t)
	if err := ix.IngestBatch(context.Background(), "pi",
		[]Session{{Source: "pi", SourceID: "019f6df5-70b0-7cfa-ad5f-328d2876b7d3", Title: "t", MsgCount: 1}},
		[]Message{{SourceID: "019f6df5-70b0-7cfa-ad5f-328d2876b7d3", Idx: 0, Role: "user", Text: "hello"}}); err != nil {
		t.Fatal(err)
	}
	full := "pi:019f6df5-70b0-7cfa-ad5f-328d2876b7d3"

	// Never leak the driver's error.
	for _, id := range []string{full[:len(full)-4], "pi:nope", "pi:0"} {
		_, err := ix.LookupSession(id)
		if err == nil {
			t.Fatalf("%s should not resolve", id)
		}
		if strings.Contains(err.Error(), "no rows in result set") {
			t.Errorf("driver error leaked for %s: %v", id, err)
		}
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error should name the id given: %v", err)
		}
	}

	// A near-miss id names the session that was meant — both a lost tail and a
	// dropped character in the middle.
	for _, typo := range []string{
		"pi:019f6df5-70b0-7cfa-ad5f-328d2876",   // truncated
		"pi:019f6df5-70b0-7cfa-ad5f-328d2876b3", // characters dropped mid-id
	} {
		_, err := ix.LookupSession(typo)
		if err == nil || !strings.Contains(err.Error(), full) {
			t.Errorf("near-miss %q should suggest %s, got: %v", typo, full, err)
		}
	}

	// An id matching nothing says how to find it instead.
	_, err := ix.LookupSession("pi:totally-unrelated")
	if err == nil || !strings.Contains(err.Error(), "recall find") {
		t.Errorf("unknown id should suggest search, got: %v", err)
	}

	// The real session still resolves.
	if _, err := ix.LookupSession(full); err != nil {
		t.Errorf("valid id broke: %v", err)
	}
}
