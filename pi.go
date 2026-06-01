package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type PiAdapter struct {
	Root string
	// batchSessions caps sessions per ingest batch (0 = default). Test-only.
	batchSessions int
}

func (a *PiAdapter) ID() string      { return "pi" }
func (a *PiAdapter) Available() bool { _, err := os.Stat(a.Root); return err == nil }

func (a *PiAdapter) Scan(ctx context.Context, prev string) ([]Session, []Message, string, error) {
	return collectScan(ctx, a, prev)
}

func (a *PiAdapter) ScanStream(ctx context.Context, prev string, emit EmitFunc) error {
	prevMap := parseFileCkpt(prev)
	nextMap := map[string]fileState{}
	be := newBatchEmitter(emit, func() string { return encodeFileCkpt(nextMap) }, a.batchSessions)

	err := filepath.WalkDir(a.Root, func(path string, d fs.DirEntry, err error) error {
		if cErr := ctx.Err(); cErr != nil {
			return cErr
		}
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		st, sErr := d.Info()
		if sErr != nil {
			return nil
		}
		size, mtime := st.Size(), st.ModTime().UnixNano()
		prevSt, ok := prevMap[path]

		if ok && prevSt.Size == size && prevSt.MTime == mtime {
			nextMap[path] = prevSt
			return nil
		}
		if ok && prevSt.SID != "" && size > prevSt.Size && prevSt.Offset <= size {
			if p, e := a.parse(ctx, path, prevSt.Offset, prevSt.Idx, prevSt.SID, true); e == nil && len(p.msgs) > 0 {
				nextMap[path] = fileState{Size: size, MTime: mtime, Offset: p.endOffset, Idx: p.nextIdx, SID: prevSt.SID}
				return be.add(Session{
					Source: "pi", SourceID: prevSt.SID, Append: true,
					EndedAt: p.endedAt, MsgCount: len(p.msgs),
				}, p.msgs)
			}
		}

		p, e := a.parse(ctx, path, 0, 0, "", true)
		nextMap[path] = fileState{Size: size, MTime: mtime, Offset: p.endOffset, Idx: p.nextIdx, SID: p.sessID}
		if e != nil || p.sessID == "" || len(p.msgs) == 0 {
			return nil
		}
		return be.add(Session{
			Source: "pi", SourceID: p.sessID,
			Project: p.project, Title: titleFromPrompt(p.firstUser),
			StartedAt: p.startedAt, EndedAt: p.endedAt,
			MsgCount: len(p.msgs),
		}, p.msgs)
	})
	if err != nil {
		return err
	}
	return be.flush()
}

func (a *PiAdapter) Fetch(ctx context.Context, sourceID string) ([]Message, error) {
	suffix := "_" + sourceID + ".jsonl"
	var found string
	_ = filepath.WalkDir(a.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), suffix) {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if found == "" {
		return nil, fmt.Errorf("pi session %s not found", sourceID)
	}
	res, err := a.parse(ctx, found, 0, 0, sourceID, false)
	return res.msgs, err
}

func (a *PiAdapter) OpenURL(sourceID string) string {
	suffix := "_" + sourceID + ".jsonl"
	var found string
	_ = filepath.WalkDir(a.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), suffix) {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

type piParse struct {
	sessID, project    string
	startedAt, endedAt int64
	firstUser          string
	msgs               []Message
	endOffset          int64
	nextIdx            int
}

func (a *PiAdapter) parse(ctx context.Context, path string, startOffset int64, startIdx int, knownSID string, truncate bool) (piParse, error) {
	res := piParse{sessID: knownSID, nextIdx: startIdx}
	fh, err := os.Open(path)
	if err != nil {
		return res, err
	}
	defer fh.Close()
	if startOffset > 0 {
		if _, err := fh.Seek(startOffset, io.SeekStart); err != nil {
			return res, err
		}
	}
	idx := startIdx
	consumed, err := scanLines(ctx, fh, func(line []byte) error {
		var ev PiEvent
		if JSONUnmarshal(line, &ev) != nil {
			return nil
		}
		ts := parseClaudeTime(ev.Timestamp)
		switch ev.Type {
		case "session":
			if ev.ID != "" {
				res.sessID = ev.ID
			}
			if ev.CWD != "" {
				res.project = ev.CWD
			}
			res.startedAt = ts
		case "message":
			text := ev.Message.Text()
			if text == "" {
				return nil
			}
			if ts > res.endedAt {
				res.endedAt = ts
			}
			if res.startedAt == 0 || (ts > 0 && ts < res.startedAt) {
				res.startedAt = ts
			}
			if ev.Message.Role == "user" && res.firstUser == "" && !looksLikeWrapper(text) {
				res.firstUser = text
			}
			if truncate && len(text) > excerptMax {
				text = text[:excerptMax]
			}
			res.msgs = append(res.msgs, Message{SourceID: res.sessID, Idx: idx, Role: ev.Message.Role, TS: ts, Text: text})
			idx++
		}
		return nil
	})
	res.endOffset = startOffset + consumed
	res.nextIdx = idx
	for i := range res.msgs {
		if res.msgs[i].SourceID == "" {
			res.msgs[i].SourceID = res.sessID
		}
	}
	return res, err
}

type PiEvent struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	CWD       string    `json:"cwd"`
	Timestamp string    `json:"timestamp"`
	Message   PiMessage `json:"message"`
}

type PiMessage struct {
	Role    string        `json:"role"`
	Content PiMessageBody `json:"content"`
}

type PiMessageBody struct {
	str   string
	parts []PiPart
}

func (b *PiMessageBody) UnmarshalJSON(data []byte) error {
	for i, c := range data {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '"':
			return JSONUnmarshal(data[i:], &b.str)
		case '[':
			return JSONUnmarshal(data[i:], &b.parts)
		}
		break
	}
	return nil
}

type PiPart struct {
	Type    string       `json:"type"`
	Text    string       `json:"text"`
	Name    string       `json:"name"`
	Content PiToolResult `json:"content"`
}

type PiToolResult struct {
	str   string
	parts []PiPart
}

func (r *PiToolResult) UnmarshalJSON(data []byte) error {
	for i, c := range data {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '"':
			return JSONUnmarshal(data[i:], &r.str)
		case '[':
			return JSONUnmarshal(data[i:], &r.parts)
		}
		break
	}
	return nil
}

func (m PiMessage) Text() string {
	if m.Content.str != "" {
		return m.Content.str
	}
	var b strings.Builder
	for _, p := range m.Content.parts {
		switch p.Type {
		case "text":
			if p.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		case "thinking":

		case "toolCall":
			if p.Name == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString("[tool:")
			b.WriteString(p.Name)
			b.WriteByte(']')
		case "toolResult":
			if s := p.Content.str; s != "" {
				if len(s) > 400 {
					s = s[:400]
				}
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString("[result] ")
				b.WriteString(s)
			}
			for _, inner := range p.Content.parts {
				if inner.Text == "" {
					continue
				}
				t := inner.Text
				if len(t) > 400 {
					t = t[:400]
				}
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString("[result] ")
				b.WriteString(t)
			}
		}
	}
	return b.String()
}
