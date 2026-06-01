package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// setBatch temporarily forces the streaming flush thresholds (so a small
// fixture produces one batch per session) and returns a restore func.
func setBatch(msgs, sessions int) func() {
	om, os := streamBatchMessages, streamBatchSessions
	streamBatchMessages, streamBatchSessions = msgs, sessions
	return func() { streamBatchMessages, streamBatchSessions = om, os }
}

func piFile(root, id string) string {
	return filepath.Join(root, "-work-web", "20260503T0800_"+id+".jsonl")
}

// A scan that gets interrupted mid-provider must persist progress so a re-run
// only processes the sessions it hadn't reached yet.
func TestStreamingResumeMidProvider(t *testing.T) {
	defer setBatch(1, 1)() // one emit per session
	root := t.TempDir()
	for _, id := range []string{"a", "b", "c"} {
		writeLines(t, piFile(root, id),
			piSession("sess-"+id, "/work/web", "2026-05-03T08:00:00Z"),
			piMsg("user", "message "+id, "2026-05-03T08:00:02Z"),
		)
	}
	ad := &PiAdapter{Root: root}
	ctx := context.Background()

	// First run: succeed for the first two sessions, then "crash" before the third.
	var committed string
	done := 0
	err := ad.ScanStream(ctx, "", func(s []Session, m []Message, ckpt string) error {
		if len(s) == 0 { // trailing flush — ignore
			return nil
		}
		if done == 2 {
			return fmt.Errorf("boom") // interrupted before the 3rd session
		}
		committed = ckpt
		done++
		return nil
	})
	if err == nil {
		t.Fatal("expected the simulated interruption error")
	}
	if done != 2 || committed == "" {
		t.Fatalf("expected 2 committed sessions, got %d (ckpt=%q)", done, committed)
	}

	// Resume from the persisted checkpoint: only the un-reached session should appear.
	var resumed []string
	if err := ad.ScanStream(ctx, committed, func(s []Session, m []Message, ckpt string) error {
		for _, ss := range s {
			resumed = append(resumed, ss.SourceID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 || resumed[0] != "sess-c" {
		t.Fatalf("resume should process only the un-reached session, got %v", resumed)
	}
}

// After a complete scan, re-scanning with the returned checkpoint must yield
// nothing — proving each provider only re-indexes genuinely new data.
func TestStreamingOnlyNewData(t *testing.T) {
	root := t.TempDir()
	writeLines(t, piFile(root, "x"),
		piSession("sess-x", "/work/web", "2026-05-03T08:00:00Z"),
		piMsg("user", "first", "2026-05-03T08:00:02Z"),
	)
	ad := &PiAdapter{Root: root}
	ctx := context.Background()

	_, _, ckpt, err := ad.Scan(ctx, "")
	if err != nil {
		t.Fatal(err)
	}

	// No changes → zero new sessions.
	s2, _, ckpt2, err := ad.Scan(ctx, ckpt)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2) != 0 {
		t.Fatalf("no-change rescan should emit 0 sessions, got %d", len(s2))
	}

	// A new session appears → only it is emitted.
	writeLines(t, piFile(root, "y"),
		piSession("sess-y", "/work/web", "2026-05-04T08:00:00Z"),
		piMsg("user", "second", "2026-05-04T08:00:02Z"),
	)
	s3, _, _, err := ad.Scan(ctx, ckpt2)
	if err != nil {
		t.Fatal(err)
	}
	if len(s3) != 1 || s3[0].SourceID != "sess-y" {
		t.Fatalf("should emit only the new session, got %+v", s3)
	}
}
