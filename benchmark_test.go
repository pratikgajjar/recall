package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkClaudeScan times the default-path Claude adapter on a realistic
// transcript (text, tool_use, and string-content tool_result parts — the shapes
// that exercise the claudeContent unmarshaler). Self-contained so it compiles
// and runs identically on main, for an apples-to-apples regression check.
func BenchmarkClaudeScan(b *testing.B) {
	root := b.TempDir()
	dir := filepath.Join(root, "-work-bench")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	var buf []byte
	for i := 0; i < 6000; i++ {
		switch i % 3 {
		case 0:
			buf = append(buf, fmt.Sprintf(`{"type":"user","timestamp":"2026-05-01T10:00:%02dZ","cwd":"/work/bench","message":{"role":"user","content":"please investigate issue number %d in the parser and propose a fix"}}`+"\n", i%60, i)...)
		case 1:
			buf = append(buf, fmt.Sprintf(`{"type":"assistant","timestamp":"2026-05-01T10:00:%02dZ","message":{"role":"assistant","content":[{"type":"text","text":"Looking into %d. I'll run the tests."},{"type":"tool_use","name":"Bash"}]}}`+"\n", i%60, i)...)
		default:
			buf = append(buf, fmt.Sprintf(`{"type":"user","timestamp":"2026-05-01T10:00:%02dZ","message":{"role":"user","content":[{"type":"tool_result","content":"test output for iteration %d: ok, all green, 42 passed"}]}}`+"\n", i%60, i)...)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "bench-sess.jsonl"), buf, 0o644); err != nil {
		b.Fatal(err)
	}

	ad := &ClaudeAdapter{Root: root}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, msgs, _, err := ad.Scan(ctx, "")
		if err != nil {
			b.Fatal(err)
		}
		if len(msgs) == 0 {
			b.Fatal("no messages parsed")
		}
	}
}
