package main

import (
	"bufio"
	"io"
)

type fileState struct {
	Size   int64  `json:"s"`
	MTime  int64  `json:"m"`
	Offset int64  `json:"o"`
	Idx    int    `json:"i"`
	SID    string `json:"d,omitempty"`
}

func parseFileCkpt(s string) map[string]fileState {
	if s == "" {
		return map[string]fileState{}
	}
	var m map[string]fileState
	if err := JSONUnmarshal([]byte(s), &m); err != nil {
		return map[string]fileState{}
	}
	return m
}

func encodeFileCkpt(m map[string]fileState) string {
	b, _ := JSONMarshal(m)
	return string(b)
}

func scanLines(r io.Reader, fn func(line []byte) error) (int64, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	var consumed int64
	for {
		line, err := br.ReadBytes('\n')
		if err == nil {
			consumed += int64(len(line))
			if e := fn(line); e != nil {
				return consumed, e
			}
			continue
		}
		if err == io.EOF {
			return consumed, nil
		}
		return consumed, err
	}
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
