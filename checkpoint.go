package main

import (
	"bufio"
	"context"
	"io"
	"strconv"
	"strings"
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
	if err := JSONUnmarshal([]byte(s), &m); err == nil {
		return m
	}
	// Legacy format was map[path]"size:mtime". Migrate it so unchanged files
	// still skip; Offset/Idx/SID are unknown, so the first change re-parses in
	// full (then re-saves in the new format).
	var legacy map[string]string
	if err := JSONUnmarshal([]byte(s), &legacy); err != nil {
		return map[string]fileState{}
	}
	m = make(map[string]fileState, len(legacy))
	for path, tok := range legacy {
		size, mtime, ok := strings.Cut(tok, ":")
		if !ok {
			continue
		}
		sz, err1 := strconv.ParseInt(size, 10, 64)
		mt, err2 := strconv.ParseInt(mtime, 10, 64)
		if err1 == nil && err2 == nil {
			m[path] = fileState{Size: sz, MTime: mt}
		}
	}
	return m
}

func encodeFileCkpt(m map[string]fileState) string {
	b, _ := JSONMarshal(m)
	return string(b)
}

func scanLines(ctx context.Context, r io.Reader, fn func(line []byte) error) (int64, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	var consumed int64
	var n int
	for {
		if n&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return consumed, err
			}
		}
		n++
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
