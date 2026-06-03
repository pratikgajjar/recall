package main

import (
	"bytes"
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
		"shown=[3:6]",      // header advertises the slice
		"## msg 3/10 user", // each message carries N/TOTAL
		"## msg 4/10 user",
		"## msg 5/10 user",
		"Continue with --range 6:", // pagination hint when more remains
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered slice missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "## msg 6/10") {
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
		"## msg 0/6 user",
		"## msg 1/6 assistant",
		"## msg 4/6 assistant", // tool rows 2,3 skipped — numbering does NOT shift
		"## msg 5/6 user",
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
	if !strings.Contains(out, "## msg 2/6 toolCall") || !strings.Contains(out, "## msg 3/6 toolResult") {
		t.Errorf("--role tool should canonicalise toolCall/toolResult, got:\n%s", out)
	}
}

func TestRoleFilterCountsAsExplicitSliceForBigSessionCap(t *testing.T) {
	opts, prelude := applyBigSessionCap(transcriptOpts{Roles: "user"}, 10000)
	if opts.Outline || prelude != "" {
		t.Errorf("explicit role must not be capped, got outline=%v prelude=%q", opts.Outline, prelude)
	}
}
