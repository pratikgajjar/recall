package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
)

// fileTok is the per-file watermark token used by file-tree adapters
// (Claude, Codex). Two files are considered "the same" if size and modtime
// match exactly. Append-only JSONL stores satisfy this trivially.
func fileTok(st fs.FileInfo) string {
	return fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
}

func parseFileCkpt(s string) map[string]string {
	if s == "" {
		return map[string]string{}
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return map[string]string{}
	}
	return m
}

func encodeFileCkpt(m map[string]string) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// Cursor uses a single max-rowid watermark from cursorDiskKV.
type cursorCkpt struct {
	RowID int64 `json:"rowid"`
}

func parseCursorCkpt(s string) int64 {
	if s == "" {
		return 0
	}
	var c cursorCkpt
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return 0
	}
	return c.RowID
}

func encodeCursorCkpt(rowid int64) string {
	b, _ := json.Marshal(cursorCkpt{RowID: rowid})
	return string(b)
}
