package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
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

// An unfiltered search ranks a pool of best-by-bm25 rows instead of every match,
// which is what took search p95 from 1,500ms to under 200ms. That is only sound
// when nothing narrows the search: the globally best rows may contain none from
// the subset the caller asked for. Filtering after pooling returned an empty
// list on a corpus full of answers, twice, so it is pinned here.
func TestFilteredSearchIsNotPooled(t *testing.T) {
	ix := newTestIndex(t)
	ctx := context.Background()

	// Enough noise that the wanted session cannot be in any global top-N.
	var sessions []Session
	var msgs []Message
	for i := 0; i < poolMin*2; i++ {
		id := fmt.Sprintf("noise-%d", i)
		sessions = append(sessions, Session{Source: "pi", SourceID: id,
			Project: "/work/other", Title: "noise", MsgCount: 1})
		msgs = append(msgs, Message{SourceID: id, Idx: 0, Role: "user",
			Text: "shibboleth appears here among a great many other sessions"})
	}
	// One session in a different project, deliberately last so a pool that
	// ignored the filter would cut it.
	sessions = append(sessions, Session{Source: "pi", SourceID: "wanted",
		Project: "/work/needle", Title: "wanted", MsgCount: 1})
	msgs = append(msgs, Message{SourceID: "wanted", Idx: 0, Role: "user",
		Text: "shibboleth appears here too, in the project being asked about"})
	if err := ix.IngestBatch(ctx, "pi", sessions, msgs); err != nil {
		t.Fatal(err)
	}

	hits, err := ix.Search("shibboleth", SearchOpts{Project: "/work/needle", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("filtered search returned nothing; the filter was applied after the pool")
	}
	for _, h := range hits {
		if h.Project != "/work/needle" {
			t.Errorf("hit from %q leaked past the project filter", h.Project)
		}
	}

	// The unfiltered search still works and still ranks.
	all, err := ix.Search("shibboleth", SearchOpts{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Errorf("unfiltered search returned %d hits, want 5", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Rank < all[i-1].Rank-0.5 {
			t.Errorf("results are not in rank order at %d: %v then %v",
				i, all[i-1].Rank, all[i].Rank)
		}
	}
}

// The opening of a message is indexed normally; everything after it goes into a
// column the ranking discounts. Indexing all of it at equal weight was measured
// three ways and cost 7-12.5% of MRR each time, so the split is the point.
func TestSplitForIndexKeepsTailSearchable(t *testing.T) {
	short := "a short message"
	if h, tail := splitForIndex(short); h != short || tail != nil {
		t.Fatalf("short text must pass through whole, got %q / %v", h, tail)
	}
	if h, _ := splitForIndex("   "); h != "" {
		t.Error("blank text should produce no rows")
	}

	var b strings.Builder
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&b, "word%d ", i)
	}
	head, tail := splitForIndex(b.String())
	if len(head) > excerptMax {
		t.Errorf("head is %d bytes, over excerptMax %d", len(head), excerptMax)
	}
	if len(tail) == 0 {
		t.Fatal("a long message must produce tail windows")
	}
	joined := head + " " + strings.Join(tail, " ")
	for _, probe := range []string{"word0", "word1999", "word3999"} {
		if !strings.Contains(joined, probe) {
			t.Errorf("%q was dropped — it would be unsearchable", probe)
		}
	}
	for i, c := range tail {
		if len(c) > excerptMax {
			t.Errorf("tail window %d is %d bytes, over excerptMax", i, len(c))
		}
		if !utf8.ValidString(c) {
			t.Errorf("tail window %d split a rune", i)
		}
	}

	// A phrase straddling the head/tail boundary must survive whole somewhere,
	// or it becomes unsearchable exactly at the seam.
	seam := strings.Repeat("x", excerptMax-20) + " distinctive phrase here " + strings.Repeat("y", 4000)
	h2, t2 := splitForIndex(seam)
	var whole bool
	for _, c := range append([]string{h2}, t2...) {
		if strings.Contains(c, "distinctive phrase here") {
			whole = true
		}
	}
	if !whole {
		t.Error("a phrase across the head/tail boundary must survive in one window")
	}

	for _, c := range func() []string {
		h, tl := splitForIndex(strings.Repeat("日本語のテキスト ", 900))
		return append([]string{h}, tl...)
	}() {
		if !utf8.ValidString(c) {
			t.Fatal("multibyte text split mid-rune")
		}
	}
	if _, tl := splitForIndex(strings.Repeat("z", 5_000_000)); len(tl) > maxChunks {
		t.Errorf("got %d tail windows, maxChunks is %d", len(tl), maxChunks)
	}
}

// Rows collapse to one hit per session, and the pool that gets ranked can be
// dominated by a few talkative sessions: a search for 15 came back with 6 while
// 30 other sessions matched every term. The caller cannot work around that —
// asking for more returns the same few — so the pool is widened until the page
// is full or the matches genuinely run out.
func TestSearchDeliversTheLimitWhenMatchesExist(t *testing.T) {
	ix := newTestIndex(t)
	ctx := context.Background()
	var sessions []Session
	var msgs []Message

	// Two sessions that mention the term constantly, and would otherwise fill
	// any pool ranked by row.
	for s := 0; s < 2; s++ {
		id := fmt.Sprintf("loud-%d", s)
		sessions = append(sessions, Session{Source: "pi", SourceID: id,
			Project: "/w", Title: "loud", MsgCount: 400})
		for i := 0; i < 400; i++ {
			msgs = append(msgs, Message{SourceID: id, Idx: i, Role: "user",
				Text: fmt.Sprintf("kryptonite mentioned again and again, line %d", i)})
		}
	}
	// Plenty of quiet sessions that mention it once.
	for s := 0; s < 30; s++ {
		id := fmt.Sprintf("quiet-%d", s)
		sessions = append(sessions, Session{Source: "pi", SourceID: id,
			Project: "/w", Title: fmt.Sprintf("quiet %d", s), MsgCount: 1})
		msgs = append(msgs, Message{SourceID: id, Idx: 0, Role: "user",
			Text: fmt.Sprintf("kryptonite appears here once, in session %d", s)})
	}
	if err := ix.IngestBatch(ctx, "pi", sessions, msgs); err != nil {
		t.Fatal(err)
	}

	hits, err := ix.Search("kryptonite", SearchOpts{Limit: 15})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 15 {
		t.Errorf("asked for 15 and got %d, with 32 sessions matching", len(hits))
	}
	seen := map[string]bool{}
	for _, h := range hits {
		if seen[h.SessionID] {
			t.Errorf("session %s returned twice", h.SessionID)
		}
		seen[h.SessionID] = true
	}
	// And it must still stop when the matches really are exhausted.
	few, err := ix.Search("kryptonite", SearchOpts{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(few) > 32 {
		t.Errorf("got %d hits from 32 sessions", len(few))
	}
}

// Real transcripts contain truncated multibyte sequences — a tool that printed
// half a rune before dying. Stored raw, the snippet of such a row is not valid
// UTF-8, and a JSON document containing it is not parseable: two of six sample
// searches returned a body that no JSON parser would accept, so an agent got a
// crash instead of results.
func TestInvalidUTF8NeverReachesTheIndex(t *testing.T) {
	broken := "a valid opening, then a truncated rune \xc3 and more text after it"
	if utf8.ValidString(broken) {
		t.Fatal("fixture is not actually broken")
	}
	head, tail := splitForIndex(broken)
	if !utf8.ValidString(head) {
		t.Error("head kept invalid UTF-8")
	}
	for i, w := range tail {
		if !utf8.ValidString(w) {
			t.Errorf("tail window %d kept invalid UTF-8", i)
		}
	}
	// Long enough to window, with the damage deep inside.
	long := strings.Repeat("padding words here ", 200) + "\xed\xa0\x80" + strings.Repeat(" more padding ", 200)
	h2, t2 := splitForIndex(long)
	for _, part := range append([]string{h2}, t2...) {
		if !utf8.ValidString(part) {
			t.Fatal("windowed text kept invalid UTF-8")
		}
	}
	// Valid text must survive untouched, including multibyte.
	clean := "日本語のテキスト and ascii"
	if h, _ := splitForIndex(clean); h != clean {
		t.Errorf("valid text was altered: %q", h)
	}
}

// The encoder must not emit a document its own reader would reject.
func TestJSONOutputIsAlwaysValidUTF8(t *testing.T) {
	out, err := JSONMarshal(map[string]string{"snippet": "half a rune \xc3 here"})
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(out) {
		t.Errorf("encoder emitted invalid UTF-8: %q", out)
	}
	var back map[string]string
	if err := JSONUnmarshal(out, &back); err != nil {
		t.Errorf("emitted JSON does not parse: %v", err)
	}
}

// A single message can be enormous — a pasted design doc, a megabyte of HAR.
// It must window all the way through rather than stop at an arbitrary depth,
// while still refusing to expand without limit.
func TestHugeMessageIsWindowedThroughout(t *testing.T) {
	// Distinct words throughout, so nothing collapses as a duplicate window.
	var b strings.Builder
	for i := 0; b.Len() < 300_000; i++ {
		fmt.Fprintf(&b, "paragraph%d discusses topic%d in detail. ", i, i*7)
	}
	head, tail := splitForIndex(b.String())
	joined := head + " " + strings.Join(tail, " ")
	for _, probe := range []string{"paragraph1 ", "paragraph2000 ", "paragraph5000 "} {
		if strings.Contains(b.String(), probe) && !strings.Contains(joined, probe) {
			t.Errorf("%q is in the message but reaches no window", probe)
		}
	}
	if len(tail) > maxChunks {
		t.Errorf("%d windows, maxChunks is %d", len(tail), maxChunks)
	}
	// The bound must still hold for something absurd.
	if _, huge := splitForIndex(strings.Repeat("abcdefghij ", 2_000_000)); len(huge) > maxChunks {
		t.Errorf("unbounded: %d windows", len(huge))
	}
}

// A schema bump must rebuild the FTS table, not merely record a new number.
// v5->v6 added a `tail` column and the migration enumerated version transitions
// one at a time, so it missed it: the marker was written as 6, the table stayed
// at 5, and because the marker then matched nothing ever noticed. Every later
// ingest failed with "table messages_fts has no column named tail" while the
// index still looked current — silent, and permanent until someone deleted it.
func TestSchemaUpgradeRebuildsTheTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.sqlite")

	// An index shaped like the previous schema: no tail column, a checkpoint
	// claiming the work is already done, and the old version marker.
	raw, err := sql.Open("sqlite", indexDSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE VIRTUAL TABLE messages_fts USING fts5(session_pk UNINDEXED, idx UNINDEXED, role UNINDEXED, text)`,
		`CREATE TABLE meta(k TEXT PRIMARY KEY, v TEXT)`,
		`INSERT INTO meta(k,v) VALUES('schema_version','5')`,
		`INSERT INTO meta(k,v) VALUES('ckpt:pi','{"done":true}')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	ix, err := openIndex(path)
	if err != nil {
		t.Fatalf("opening an older index should upgrade it, got: %v", err)
	}
	defer ix.Close()

	// The table must now accept what this build writes.
	if err := ix.IngestBatch(context.Background(), "pi",
		[]Session{{Source: "pi", SourceID: "s", Title: "t", MsgCount: 1}},
		[]Message{{SourceID: "s", Idx: 0, Role: "user", Text: "hello"}}); err != nil {
		t.Fatalf("ingest after upgrade failed — the table was not rebuilt: %v", err)
	}
	// ...and the checkpoint must be gone, or the rebuilt table stays empty
	// because every adapter believes its work is already done.
	var ckpt int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM meta WHERE k LIKE 'ckpt:%'`).Scan(&ckpt); err != nil {
		t.Fatal(err)
	}
	if ckpt != 0 {
		t.Errorf("%d checkpoints survived the rebuild; sessions would never be re-indexed", ckpt)
	}
	hits, err := ix.Search("hello", SearchOpts{Limit: 5})
	if err != nil || len(hits) == 0 {
		t.Errorf("upgraded index does not search: %v, %d hits", err, len(hits))
	}
}

// An index newer than this build must be left alone. Treating it as "outdated"
// and rebuilding is how a shared index gets downgraded: the newer tool then
// finds an old index and rebuilds it forward, and the two take turns destroying
// each other's work at minutes a cycle. This machine runs several agents, each
// potentially carrying its own recall binary, so the case is routine.
func TestNewerIndexIsRefusedNotDowngraded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "future.sqlite")

	ix, err := openIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.IngestBatch(context.Background(), "pi",
		[]Session{{Source: "pi", SourceID: "s", Title: "t", MsgCount: 1}},
		[]Message{{SourceID: "s", Idx: 0, Role: "user", Text: "written by the future"}}); err != nil {
		t.Fatal(err)
	}
	ix.Close()

	raw, err := sql.Open("sqlite", indexDSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE meta SET v='9' WHERE k='schema_version'`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	for _, open := range []struct {
		name string
		fn   func(string) (*Index, error)
	}{{"write", openIndex}, {"read", openIndexRead}} {
		got, err := open.fn(path)
		if err == nil {
			got.Close()
			t.Fatalf("%s path accepted a newer index", open.name)
		}
		if !errors.Is(err, errNewerSchema) {
			t.Errorf("%s path: %v — should name the version problem", open.name, err)
		}
		if strings.Contains(err.Error(), "rebuild`") || strings.Contains(err.Error(), "run `recall index`") {
			t.Errorf("%s path advises rebuilding, which downgrades: %v", open.name, err)
		}
	}

	// Nothing may have been written on the way past.
	raw, _ = sql.Open("sqlite", indexDSN(path, true))
	defer raw.Close()
	var v string
	if err := raw.QueryRow(`SELECT v FROM meta WHERE k='schema_version'`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != "9" {
		t.Errorf("version marker was rewritten to %q — the index was downgraded", v)
	}
	var n int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM messages_fts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("content changed: %d rows, want 1", n)
	}
}
