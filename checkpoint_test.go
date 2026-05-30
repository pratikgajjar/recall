package main

import (
	"context"
	"strings"
	"testing"
)

func TestScanLinesSkipsPartialTrailing(t *testing.T) {
	// Two complete lines, then a partial (no trailing newline).
	in := "a\nbb\nccc"
	var got []string
	n, err := scanLines(context.Background(), strings.NewReader(in), func(line []byte) error {
		got = append(got, string(line))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "a\n" || got[1] != "bb\n" {
		t.Fatalf("lines = %q (want the two newline-terminated lines only)", got)
	}
	if n != int64(len("a\n")+len("bb\n")) {
		t.Errorf("consumed = %d, want %d (partial 'ccc' excluded)", n, len("a\n")+len("bb\n"))
	}
}

func TestScanLinesAllComplete(t *testing.T) {
	in := "x\ny\n"
	var count int
	n, err := scanLines(context.Background(), strings.NewReader(in), func(line []byte) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || n != int64(len(in)) {
		t.Errorf("count=%d n=%d want 2/%d", count, n, len(in))
	}
}

func TestFileCkptRoundTrip(t *testing.T) {
	m := map[string]fileState{
		"/a/b.jsonl": {Size: 100, MTime: 42, Offset: 90, Idx: 3, SID: "sid-1"},
	}
	enc := encodeFileCkpt(m)
	got := parseFileCkpt(enc)
	if got["/a/b.jsonl"] != m["/a/b.jsonl"] {
		t.Fatalf("round trip mismatch: %+v vs %+v", got, m)
	}
	// Legacy "size:mtime" checkpoints must migrate (so unchanged files still
	// skip) — not degrade to empty, which would force a full re-ingest.
	leg := parseFileCkpt(`{"/a/b.jsonl":"100:42"}`)
	if leg["/a/b.jsonl"] != (fileState{Size: 100, MTime: 42}) {
		t.Errorf("legacy checkpoint should migrate to Size/MTime, got %+v", leg)
	}
	// Genuinely unparseable input degrades to empty.
	if len(parseFileCkpt(`not json`)) != 0 {
		t.Error("garbage checkpoint should parse as empty map")
	}
	if len(parseFileCkpt("")) != 0 {
		t.Error("empty checkpoint should be empty map")
	}
}
