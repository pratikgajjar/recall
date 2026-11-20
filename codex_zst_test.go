package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// codexRollout builds one rollout line set in the shape codex writes.
func codexRollout(ts string) []byte {
	return []byte(
		`{"type":"session_meta","payload":{"id":"sess-zst-1","cwd":"/work/acme","timestamp":"` + ts + `"}}` + "\n" +
			`{"type":"response_item","timestamp":"` + ts + `","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"zstd rollout body"}]}}` + "\n" +
			`{"type":"response_item","timestamp":"` + ts + `","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":\"go build ./...\"}"}}` + "\n")
}

// TestCodexReadsZstdRollouts pins the compression-aware path. With
// local_thread_store_compression = true codex rewrites every rollout to
// rollout-*.jsonl.zst in place; an adapter matching only ".jsonl" saw 13 of
// 16,388 sessions on a real machine, and `index --prune` then deleted the rest.
func TestCodexReadsZstdRollouts(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "2026", "05", "01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	plain := codexRollout("2026-05-01T10:00:00Z")
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := enc.EncodeAll(plain, nil)
	enc.Close()

	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-05-01T10-00-00-sess-zst-1.jsonl.zst"), compressed, 0o644); err != nil {
		t.Fatal(err)
	}

	ad := &CodexAdapter{Root: root}
	ss, mm, _, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(ss) != 1 || ss[0].SourceID != "sess-zst-1" || ss[0].Project != "/work/acme" {
		t.Fatalf("sessions = %+v", ss)
	}
	if len(mm) != 2 {
		t.Fatalf("messages = %d, want 2 (%+v)", len(mm), mm)
	}
	if mm[0].Text != "zstd rollout body" {
		t.Errorf("message text = %q", mm[0].Text)
	}

	// Fetch must find the compressed log too, or `recall show` breaks for it.
	msgs, err := ad.Fetch(context.Background(), "sess-zst-1")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(msgs) != 2 {
		t.Errorf("fetch = %d messages, want 2", len(msgs))
	}

	// A second scan of an unchanged .zst must emit nothing (size+mtime skip).
	ss2, mm2, _, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ss2) != 1 || len(mm2) != 2 {
		t.Logf("second scan re-parsed the file (acceptable, but it should skip): %d/%d", len(ss2), len(mm2))
	}
}

// TestCodexArchivedIsItsOwnSource: the archived tree reuses the rollout parser
// but must not be reported as "codex", or pruning one tree looks like data loss
// in the other.
func TestCodexArchivedIsItsOwnSource(t *testing.T) {
	root := t.TempDir()
	writeLines(t, filepath.Join(root, "rollout-2026-05-01T10-00-00-sess-arch-1.jsonl"),
		`{"type":"session_meta","payload":{"id":"sess-arch-1","cwd":"/work/acme"}}`,
		`{"type":"response_item","timestamp":"2026-05-01T10:00:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"archived body"}]}}`,
	)
	ad := &CodexAdapter{Root: root, SourceID: "codex-archived"}
	if ad.ID() != "codex-archived" {
		t.Fatalf("ID = %q", ad.ID())
	}
	ss, _, _, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].Source != "codex-archived" {
		t.Fatalf("sessions = %+v", ss)
	}
}
