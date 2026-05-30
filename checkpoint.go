package main

import (
	"fmt"
	"io/fs"
)

func fileTok(st fs.FileInfo) string {
	return fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
}

func parseFileCkpt(s string) map[string]string {
	if s == "" {
		return map[string]string{}
	}
	var m map[string]string
	if err := JSONUnmarshal([]byte(s), &m); err != nil {
		return map[string]string{}
	}
	return m
}

func encodeFileCkpt(m map[string]string) string {
	b, _ := JSONMarshal(m)
	return string(b)
}

type cursorCkpt struct {
	RowID int64 `json:"rowid"`
}

func parseCursorCkpt(s string) int64 {
	if s == "" {
		return 0
	}
	var c cursorCkpt
	if err := JSONUnmarshal([]byte(s), &c); err != nil {
		return 0
	}
	return c.RowID
}

func encodeCursorCkpt(rowid int64) string {
	b, _ := JSONMarshal(cursorCkpt{RowID: rowid})
	return string(b)
}
