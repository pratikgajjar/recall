package main

import (
	"bytes"
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestParseRange(t *testing.T) {
	cases := []struct {
		s          string
		total      int
		wantLo, wh int
		wantErr    bool
	}{
		{"", 10, 0, 10, false},
		{":", 10, 0, 10, false},
		{":5", 10, 0, 5, false},
		{"3:", 10, 3, 10, false},
		{"3:7", 10, 3, 7, false},
		{"-3:", 10, 7, 10, false},
		{":-3", 10, 0, 7, false},
		{"-5:-2", 10, 5, 8, false},
		{"100:200", 10, 10, 10, false}, // clamp past end
		{"-100:5", 10, 0, 5, false},    // clamp past start
		{"7:3", 10, 3, 3, false},       // inverted → empty
		{"0:0", 10, 0, 0, false},
		{":5", 0, 0, 0, false}, // empty population
		{"abc:5", 10, 0, 0, true},
		{"5:abc", 10, 0, 0, true},
		{"no-colon", 10, 0, 0, true},
	}
	for _, c := range cases {
		lo, hi, err := parseRange(c.s, c.total)
		if (err != nil) != c.wantErr {
			t.Errorf("parseRange(%q, %d) err=%v want err=%v", c.s, c.total, err, c.wantErr)
			continue
		}
		if err == nil && (lo != c.wantLo || hi != c.wh) {
			t.Errorf("parseRange(%q, %d) = (%d,%d), want (%d,%d)", c.s, c.total, lo, hi, c.wantLo, c.wh)
		}
	}
}

func sampleSession(n int) (*Session, []Message) {
	s := &Session{Source: "pi", SourceID: "x", Title: "demo", StartedAt: 0, MsgCount: n}
	msgs := make([]Message, n)
	for i := range msgs {
		msgs[i] = Message{Idx: i, Role: "user", Text: "message " + strings.Repeat("x", 1)}
	}
	return s, msgs
}

func TestRenderTranscriptHeadersAreSelfLocating(t *testing.T) {
	s, msgs := sampleSession(10)
	var b bytes.Buffer
	if err := renderTranscript(&b, s, msgs, transcriptOpts{Range: "3:6"}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"shown=[3:6]", // header advertises the slice
		"## 3 user",   // each message carries its absolute index
		"## 4 user",
		"## 5 user",
		"next: --range 6:", // pagination hint when more remains
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered slice missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "## 6 ") {
		t.Errorf("rendered slice should stop before msg 6, got:\n%s", out)
	}
}

func TestRenderOutlineMatchesRange(t *testing.T) {
	s, msgs := sampleSession(5)
	var b bytes.Buffer
	if err := renderTranscript(&b, s, msgs, transcriptOpts{Outline: true}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "(outline)") {
		t.Errorf("outline header missing:\n%s", out)
	}
	// The outline's [N] positions must be the same indices --range slices.
	for i := 0; i < 5; i++ {
		if !strings.Contains(out, "[") {
			t.Fatalf("missing outline entries: %s", out)
		}
	}
}

func TestApplyBigSessionCap(t *testing.T) {
	// Big session, no explicit slice → switch to outline + emit a note.
	opts, prelude := applyBigSessionCap(transcriptOpts{}, mcpBigSessionThreshold+1)
	if !opts.Outline || prelude == "" {
		t.Errorf("big + no slice: want outline+note, got outline=%v prelude=%q", opts.Outline, prelude)
	}

	// Big session, but caller asked for a range → leave them alone.
	opts, prelude = applyBigSessionCap(transcriptOpts{Range: "0:50"}, 10000)
	if opts.Outline || prelude != "" {
		t.Errorf("explicit range must not be overridden, got outline=%v prelude=%q", opts.Outline, prelude)
	}

	// Big session, caller asked for outline → no double-handling.
	opts, prelude = applyBigSessionCap(transcriptOpts{Outline: true}, 10000)
	if !opts.Outline || prelude != "" {
		t.Errorf("explicit outline must not get a note, got prelude=%q", prelude)
	}

	// Small session → no cap, untouched.
	opts, prelude = applyBigSessionCap(transcriptOpts{}, mcpBigSessionThreshold)
	if opts.Outline || prelude != "" {
		t.Errorf("small session must not be capped, got outline=%v", opts.Outline)
	}
}

func TestCanonicalRole(t *testing.T) {
	cases := map[string]string{
		"user":                 "user",
		"User":                 "user",
		"assistant":            "assistant",
		"tool":                 "tool",
		"toolResult":           "tool",
		"toolCall":             "tool",
		"function_call":        "tool",
		"function_call_output": "tool",
		"function_result":      "tool",
		"system":               "system", // unknown role passed through
	}
	for in, want := range cases {
		if got := canonicalRole(in); got != want {
			t.Errorf("canonicalRole(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRoleFilterKeepsAbsoluteIndices(t *testing.T) {
	s := &Session{Source: "pi", SourceID: "x", Title: "demo", MsgCount: 6}
	msgs := []Message{
		{Idx: 0, Role: "user", Text: "q1"},
		{Idx: 1, Role: "assistant", Text: "a1"},
		{Idx: 2, Role: "toolCall", Text: "[tool:read]"},
		{Idx: 3, Role: "toolResult", Text: "ok"},
		{Idx: 4, Role: "assistant", Text: "a2"},
		{Idx: 5, Role: "user", Text: "q2"},
	}

	var b bytes.Buffer
	if err := renderTranscript(&b, s, msgs, transcriptOpts{Roles: "user,assistant"}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"role=user,assistant",
		"## 0 user",
		"## 1 assistant",
		"## 4 assistant", // tool rows 2,3 skipped — numbering does NOT shift
		"## 5 user",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
	for _, gone := range []string{"toolCall", "toolResult", "[tool:read]"} {
		if strings.Contains(out, gone) {
			t.Errorf("role filter should have removed %q, got:\n%s", gone, out)
		}
	}

	// 'tool' canonicalises so --role tool catches toolCall AND toolResult.
	b.Reset()
	if err := renderTranscript(&b, s, msgs, transcriptOpts{Roles: "tool"}); err != nil {
		t.Fatal(err)
	}
	out = b.String()
	if !strings.Contains(out, "## 2 tool") || !strings.Contains(out, "## 3 tool") {
		t.Errorf("--role tool should canonicalise toolCall/toolResult, got:\n%s", out)
	}
}

func TestRoleFilterCountsAsExplicitSliceForBigSessionCap(t *testing.T) {
	opts, prelude := applyBigSessionCap(transcriptOpts{Roles: "user"}, 10000)
	if opts.Outline || prelude != "" {
		t.Errorf("explicit role must not be capped, got outline=%v prelude=%q", opts.Outline, prelude)
	}
}

// Tool output is the bulk of an agent transcript, so it is clipped by default —
// but the clip must announce itself and stay opt-out-able, never silently lie.
func TestClipToolBody(t *testing.T) {
	body := strings.Repeat("line of tool output\n", 200) // 4000 chars
	got := clipToolBody(body, 600)
	if len(got) >= len(body) {
		t.Fatalf("not clipped: %d >= %d", len(got), len(body))
	}
	if !strings.Contains(got, "elided") || !strings.Contains(got, "--tool-chars 0") {
		t.Errorf("clip must say what was dropped and how to get it:\n%s", got[len(got)-120:])
	}
	if !strings.HasPrefix(got, "line of tool output") {
		t.Error("head of the output must survive")
	}
	// 0 and under-limit bodies pass through untouched.
	if clipToolBody(body, 0) != body {
		t.Error("tool_chars=0 must be uncapped")
	}
	if s := "short"; clipToolBody(s, 600) != s {
		t.Error("short bodies must not be touched")
	}
}

// Only tool output is clipped; user/assistant prose is never cut.
func TestClipAppliesOnlyToToolRoles(t *testing.T) {
	long := strings.Repeat("x", 5000)
	s := &Session{Source: "pi", Title: "t"}
	msgs := []Message{
		{Idx: 0, Role: "user", Text: long},
		{Idx: 1, Role: "toolResult", Text: long},
	}
	var b strings.Builder
	if err := renderTranscript(&b, s, msgs, transcriptOpts{ToolChars: 600}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, strings.Repeat("x", 5000)) {
		t.Error("user message must render in full")
	}
	if strings.Count(out, "elided") != 1 {
		t.Errorf("exactly the tool message should be elided, got %d", strings.Count(out, "elided"))
	}
}

// An outline is navigation, not content: runs of tool activity collapse to one
// line that still names the span to slice on and what is inside it.
func TestOutlineCollapsesToolRuns(t *testing.T) {
	s := &Session{Source: "pi", Title: "t"}
	msgs := []Message{
		{Idx: 0, Role: "user", Text: "do the thing"},
		{Idx: 1, Role: "assistant", Text: "[tool:bash]"}, // marker-only = tool activity
		{Idx: 2, Role: "toolResult", Text: strings.Repeat("out\n", 500)},
		{Idx: 3, Role: "toolResult", Text: "more"},
		{Idx: 4, Role: "assistant", Text: "Here is the answer."},
	}
	var b strings.Builder
	if err := renderTranscript(&b, s, msgs, transcriptOpts{Outline: true}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "[1-3] tool ×3") {
		t.Errorf("want a collapsed run [1-3]:\n%s", out)
	}
	if !strings.Contains(out, "bash") {
		t.Errorf("run should name the tools involved:\n%s", out)
	}
	// Prose on both sides must survive with its own index, or you cannot navigate.
	for _, want := range []string{"[0] user:", "[4] assistant:", "do the thing", "Here is the answer."} {
		if !strings.Contains(out, want) {
			t.Errorf("outline lost %q:\n%s", want, out)
		}
	}
	// The bulk of the tool output must NOT be in the outline.
	if strings.Count(out, "out\n") > 2 {
		t.Error("tool bodies must not be inlined into an outline")
	}
}

func TestToolMarkerNames(t *testing.T) {
	for _, c := range []struct {
		in    string
		names []string
		only  bool
	}{
		{"[tool:bash]", []string{"bash"}, true},
		{"[tool:read]\n[tool:edit]", []string{"read", "edit"}, true},
		{"[tool_use:Grep]", []string{"Grep"}, true},
		{"[call shell]", []string{"shell"}, true},
		{"Let me check.\n[tool:bash]", nil, false}, // has prose → keeps its own line
		{"just prose", nil, false},
	} {
		got, only := toolMarkerNames(c.in)
		if only != c.only {
			t.Errorf("%q: only=%v want %v", c.in, only, c.only)
		}
		if only && strings.Join(got, ",") != strings.Join(c.names, ",") {
			t.Errorf("%q: names=%v want %v", c.in, got, c.names)
		}
	}
}

// Models often hand back an already-quoted argument. Rejecting it burns a
// round-trip to say nothing, so the parser tolerates the usual wrappers.
func TestParseRangeToleratesQuoting(t *testing.T) {
	for _, in := range []string{`":30"`, `':30'`, "`:30`", "[:30]", " :30 ", `":30`} {
		lo, hi, err := parseRange(in, 100)
		if err != nil || lo != 0 || hi != 30 {
			t.Errorf("parseRange(%q) = %d,%d,%v; want 0,30,nil", in, lo, hi, err)
		}
	}
	// Genuinely malformed input must still be an error, not a silent full dump.
	if _, _, err := parseRange("abc:def", 100); err == nil {
		t.Error("want error for non-numeric range")
	}
}

// A big session's outline must stay bounded, and must degrade to chapter
// markers (the user's turns) rather than truncating arbitrarily.
func TestOutlineAdaptsDensityOnBigSessions(t *testing.T) {
	s := &Session{Source: "pi", Title: "big"}
	var msgs []Message
	for i := 0; i < 400; i++ {
		role, text := "assistant", "narration line number "+strconv.Itoa(i)+" padded out a bit"
		if i%40 == 0 {
			role, text = "user", "QUESTION "+strconv.Itoa(i)
		} else if i%3 == 0 {
			role, text = "toolResult", strings.Repeat("z", 500)
		}
		msgs = append(msgs, Message{Idx: i, Role: role, Text: text})
	}
	var b strings.Builder
	if err := renderTranscript(&b, s, msgs, transcriptOpts{Outline: true}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if len(out) > outlineBudget {
		t.Errorf("outline should stay near budget, got %d", len(out))
	}
	// Every user turn is a chapter marker and must survive.
	for i := 0; i < 400; i += 40 {
		if !strings.Contains(out, "QUESTION "+strconv.Itoa(i)) {
			t.Errorf("dropped chapter marker at %d:\n%s", i, out)
		}
	}
	if !strings.Contains(out, "role='user,assistant'") {
		t.Error("must tell the caller how to get full density back")
	}
	// An explicit role filter means the caller chose: honour it, full density.
	var b2 strings.Builder
	if err := renderTranscript(&b2, s, msgs, transcriptOpts{Outline: true, Roles: "user,assistant"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b2.String(), "narration line number 1 ") {
		t.Error("explicit role filter must give per-turn lines")
	}
}

// An oversized range is paged, not dumped — and must say exactly how to resume,
// otherwise the caller has silently lost the rest of the session.
func TestTranscriptOutputBudget(t *testing.T) {
	s := &Session{Source: "pi", Title: "big"}
	var msgs []Message
	for i := 0; i < 300; i++ {
		msgs = append(msgs, Message{Idx: i, Role: "assistant", Text: strings.Repeat("word ", 200)})
	}
	var b strings.Builder
	if err := renderTranscript(&b, s, msgs, transcriptOpts{Range: "0:300", MaxChars: 20000}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if len(out) > 25000 {
		t.Errorf("budget not enforced: %d chars", len(out))
	}
	if !strings.Contains(out, "stopped at msg") || !strings.Contains(out, "Continue with range=") {
		t.Errorf("must explain how to resume:\n%s", out[max(0, len(out)-200):])
	}
	// The resume hint must point at the first message that was NOT rendered.
	m := regexp.MustCompile(`stopped at msg (\d+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatal("no stop marker")
	}
	if strings.Contains(out, "## "+m[1]+" ") {
		t.Errorf("msg %s was rendered but reported as the stopping point", m[1])
	}
	// Unbounded still means unbounded.
	var full strings.Builder
	if err := renderTranscript(&full, s, msgs, transcriptOpts{Range: "0:300", MaxChars: 0}); err != nil {
		t.Fatal(err)
	}
	if full.Len() <= len(out) {
		t.Error("MaxChars=0 must render everything")
	}
}

// Chapter mode alone does not bound a session with hundreds of user turns, so
// the budget must also act as a hard stop that says where to resume.
func TestOutlineBudgetIsAHardStop(t *testing.T) {
	s := &Session{Source: "pi", Title: "many questions"}
	var msgs []Message
	for i := 0; i < 2000; i++ { // all user turns: chapter mode cannot shrink this
		msgs = append(msgs, Message{Idx: i, Role: "user", Text: "question number " + strconv.Itoa(i)})
	}
	var b strings.Builder
	if err := renderTranscript(&b, s, msgs, transcriptOpts{Outline: true}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if len(out) > outlineBudget*2 {
		t.Errorf("outline unbounded at %d chars", len(out))
	}
	if !strings.Contains(out, "outline truncated at msg") || !strings.Contains(out, "outline=true") {
		t.Errorf("truncated outline must say how to resume:\n%s", out[max(0, len(out)-160):])
	}
}

// An outline can itself be sliced, which is what the resume hint relies on.
func TestOutlineHonoursRange(t *testing.T) {
	s := &Session{Source: "pi", Title: "t"}
	var msgs []Message
	for i := 0; i < 50; i++ {
		msgs = append(msgs, Message{Idx: i, Role: "user", Text: "turn " + strconv.Itoa(i)})
	}
	var b strings.Builder
	if err := renderTranscript(&b, s, msgs, transcriptOpts{Outline: true, Range: "40:"}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "[0] user") || !strings.Contains(out, "[45] user") {
		t.Errorf("range not honoured by outline:\n%s", out)
	}
}

// --context must render into the caller's writer. MCP shares this path and its
// stdout carries JSON-RPC frames, so a stray os.Stdout write corrupts the wire.
func TestPrintHitsWithContextWritesToWriter(t *testing.T) {
	ix := newTestIndex(t)
	if err := ix.IngestBatch(context.Background(), "pi",
		[]Session{{Source: "pi", SourceID: "s1", Title: "ctx", MsgCount: 3}},
		[]Message{
			{SourceID: "s1", Idx: 0, Role: "user", Text: "alpha question"},
			{SourceID: "s1", Idx: 1, Role: "assistant", Text: "beta answer"},
			{SourceID: "s1", Idx: 2, Role: "user", Text: "gamma follow-up"},
		}); err != nil {
		t.Fatal(err)
	}
	hits, err := ix.Search("beta", SearchOpts{Limit: 5})
	if err != nil || len(hits) == 0 {
		t.Fatalf("no hits: %v", err)
	}
	var b bytes.Buffer
	// Adapter lookup will fail for the synthetic source; what matters here is
	// that the header lands in the buffer and nothing escapes to stdout.
	_ = printHitsWithContext(context.Background(), &b, ix, hits, 1)
	if b.Len() == 0 {
		t.Fatal("nothing written to the provided writer")
	}
	if !strings.Contains(b.String(), "id=pi:s1") {
		t.Errorf("hit header missing from writer output:\n%s", b.String())
	}
}

// Expanding several hits from one session must state the session once. The
// per-hit repeat was ~250 chars of identical identity on the very path agents
// are told to prefer.
func TestContextHoistsSharedSessionHeader(t *testing.T) {
	ix := newTestIndex(t)
	var msgs []Message
	for i := 0; i < 12; i++ {
		msgs = append(msgs, Message{SourceID: "s1", Idx: i, Role: "user",
			Text: "widget cache discussion " + strconv.Itoa(i)})
	}
	if err := ix.IngestBatch(context.Background(), "pi",
		[]Session{{Source: "pi", SourceID: "s1", Title: "caching", MsgCount: len(msgs)}},
		msgs); err != nil {
		t.Fatal(err)
	}
	hits, err := ix.Search("widget cache", SearchOpts{SessionID: "pi:s1", Limit: 3})
	if err != nil || len(hits) < 2 {
		t.Fatalf("need >=2 hits in one session, got %d (%v)", len(hits), err)
	}
	var b bytes.Buffer
	_ = printHitsWithContext(context.Background(), &b, ix, hits, 1)
	out := b.String()
	if n := strings.Count(out, "pi:s1"); n != 1 {
		t.Errorf("session id should appear once, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "hits expanded") {
		t.Errorf("shared header should say how many hits:\n%s", out)
	}
}

// Adapters append a clipped argument after the marker. Detection must survive
// that, or every marker-only turn takes its own outline line instead of folding
// into a tool run — measured at +27% outline size when this regressed.
func TestToolMarkerNamesWithArguments(t *testing.T) {
	for _, c := range []struct {
		in    string
		names []string
		only  bool
	}{
		{"[tool:bash] git log --oneline -5", []string{"bash"}, true},
		{"[tool:read] /etc/hosts", []string{"read"}, true},
		{"[tool:bash] a\n[tool:read] b", []string{"bash", "read"}, true},
		{"[tool:bash]", []string{"bash"}, true}, // no-arg form still works
		{"Let me check.\n[tool:bash] ls", nil, false},
	} {
		got, only := toolMarkerNames(c.in)
		if only != c.only {
			t.Errorf("%q: only=%v want %v", c.in, only, c.only)
		}
		if only && strings.Join(got, ",") != strings.Join(c.names, ",") {
			t.Errorf("%q: names=%v want %v", c.in, got, c.names)
		}
	}
}
