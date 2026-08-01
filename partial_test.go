package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A session file is appended to while recall is indexing it — routine on a
// machine running several agents. The final line is then half written. If the
// checkpoint advances past those bytes, the message is lost for good: when the
// agent finishes the line, recall resumes after it and never looks back.
func TestHalfWrittenLineIsNotSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "2026-08-01T00-00-00-000Z_019fcccc-1111-7000-8000-000000000001.jsonl")

	line := func(text string) string {
		b, _ := json.Marshal(map[string]any{
			"type": "message",
			"message": map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": text}},
			},
		})
		return string(b) + "\n"
	}
	header, _ := json.Marshal(map[string]any{
		"type": "session", "version": 3,
		"id":        "019fcccc-1111-7000-8000-000000000001",
		"timestamp": "2026-08-01T00:00:00.000Z", "cwd": "/tmp/proj",
	})

	complete := string(header) + "\n" + line("first complete message zebrafish")
	halfWritten := strings.TrimSuffix(line("second message mentions axolotl"), "\n")
	halfWritten = halfWritten[:len(halfWritten)/2]
	if err := os.WriteFile(path, []byte(complete+halfWritten), 0o644); err != nil {
		t.Fatal(err)
	}

	ad := &PiAdapter{Root: dir}
	_, msgs, ckpt, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("first scan got %d messages, want the 1 complete one", len(msgs))
	}
	if strings.Contains(msgs[0].Text, "axolotl") {
		t.Error("a half-written line was indexed as if complete")
	}

	// The agent finishes the line it was in the middle of.
	if err := os.WriteFile(path, []byte(complete+line("second message mentions axolotl")), 0o644); err != nil {
		t.Fatal(err)
	}

	_, msgs2, _, err := ad.Scan(context.Background(), ckpt)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	var found bool
	for _, m := range msgs2 {
		if strings.Contains(m.Text, "axolotl") {
			found = true
		}
	}
	if !found {
		t.Errorf("the completed message was never picked up: the checkpoint advanced past a "+
			"half-written line. second scan returned %d messages", len(msgs2))
	}
}

// Tags are the only thing in the index that cannot be rebuilt from source: the
// help text calls the index disposable *because* tags survive. A schema upgrade
// now drops and recreates tables, so that promise is one careless DROP away from
// being false, and nothing would notice until someone looked for a bookmark.
func TestTagsSurviveRebuildAndSchemaUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "i.sqlite")

	ix, err := openIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	sid := "pi:s1"
	if err := ix.IngestBatch(context.Background(), "pi",
		[]Session{{Source: "pi", SourceID: "s1", Title: "t", MsgCount: 1}},
		[]Message{{SourceID: "s1", Idx: 0, Role: "user", Text: "a decision worth bookmarking"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.AddTags(sid, []string{"important-decision"}); err != nil {
		t.Fatal(err)
	}
	ix.Close()

	// Pretend an older build wrote this, so reopening runs the migration.
	raw, err := sql.Open("sqlite", indexDSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE meta SET v='4' WHERE k='schema_version'`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	ix2, err := openIndex(path)
	if err != nil {
		t.Fatalf("upgrade failed: %v", err)
	}
	defer ix2.Close()

	tags, err := ix2.SessionTags(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) == 0 {
		t.Fatal("the tag did not survive the schema upgrade — it cannot be rebuilt from source")
	}
	var found bool
	for _, tg := range tags {
		if tg == "important-decision" {
			found = true
		}
	}
	if !found {
		t.Errorf("tags after upgrade = %v, want important-decision", tags)
	}
}
