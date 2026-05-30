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

type CodexAdapter struct {
	Root string
}

func (a *CodexAdapter) ID() string      { return "codex" }
func (a *CodexAdapter) Available() bool { _, err := os.Stat(a.Root); return err == nil }

func (a *CodexAdapter) Scan(ctx context.Context, prev string) ([]Session, []Message, string, error) {
	prevMap := parseFileCkpt(prev)
	nextMap := map[string]fileState{}

	var sessions []Session
	var msgs []Message
	err := filepath.WalkDir(a.Root, func(path string, d fs.DirEntry, err error) error {
		if cErr := ctx.Err(); cErr != nil {
			return cErr
		}
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		st, statErr := d.Info()
		if statErr != nil {
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
				sessions = append(sessions, Session{
					Source: "codex", SourceID: prevSt.SID, Append: true,
					EndedAt: p.endedAt, MsgCount: len(p.msgs),
				})
				msgs = append(msgs, p.msgs...)
				nextMap[path] = fileState{Size: size, MTime: mtime, Offset: p.endOffset, Idx: p.nextIdx, SID: prevSt.SID}
				return nil
			}
		}

		p, e := a.parse(ctx, path, 0, 0, "", true)
		nextMap[path] = fileState{Size: size, MTime: mtime, Offset: p.endOffset, Idx: p.nextIdx, SID: p.sessID}
		if e != nil || p.sessID == "" || len(p.msgs) == 0 {
			return nil
		}
		sessions = append(sessions, Session{
			Source: "codex", SourceID: p.sessID,
			Project: p.project, Title: titleFromPrompt(p.firstUser),
			StartedAt: p.startedAt, EndedAt: p.endedAt,
			MsgCount: len(p.msgs),
		})
		msgs = append(msgs, p.msgs...)
		return nil
	})
	return sessions, msgs, encodeFileCkpt(nextMap), err
}

func (a *CodexAdapter) Fetch(ctx context.Context, sourceID string) ([]Message, error) {
	suffix := sourceID + ".jsonl"
	var found string
	_ = filepath.WalkDir(a.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), suffix) {
			found = path
			return io.EOF
		}
		return nil
	})
	if found == "" {
		return nil, fmt.Errorf("codex session %s not found", sourceID)
	}
	res, err := a.parse(ctx, found, 0, 0, sourceID, false)
	return res.msgs, err
}

func (a *CodexAdapter) OpenURL(sourceID string) string {
	return "codex resume " + sourceID
}

type codexParse struct {
	sessID, project    string
	startedAt, endedAt int64
	firstUser          string
	msgs               []Message
	endOffset          int64
	nextIdx            int
}

func (a *CodexAdapter) parse(ctx context.Context, path string, startOffset int64, startIdx int, knownSID string, truncate bool) (codexParse, error) {
	res := codexParse{sessID: knownSID, nextIdx: startIdx}
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
		var ev CodexEvent
		if JSONUnmarshal(line, &ev) != nil {
			return nil
		}
		ts := parseClaudeTime(ev.Timestamp)
		switch ev.Type {
		case "session_meta":
			if ev.Payload.ID != "" {
				res.sessID = ev.Payload.ID
			}
			if ev.Payload.CWD != "" {
				res.project = ev.Payload.CWD
			}
			if t := parseClaudeTime(ev.Payload.Timestamp); t > 0 {
				res.startedAt = t
			} else if ts > 0 {
				res.startedAt = ts
			}
		case "response_item":
			role, text := ev.Payload.flatten()
			if text == "" {
				return nil
			}
			if ts > res.endedAt {
				res.endedAt = ts
			}
			if res.startedAt == 0 || (ts > 0 && ts < res.startedAt) {
				res.startedAt = ts
			}
			if role == "user" && res.firstUser == "" && !looksLikeWrapper(text) {
				res.firstUser = text
			}
			if truncate && len(text) > excerptMax {
				text = text[:excerptMax]
			}
			res.msgs = append(res.msgs, Message{SourceID: res.sessID, Idx: idx, Role: role, TS: ts, Text: text})
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

type CodexEvent struct {
	Type      string       `json:"type"`
	Timestamp string       `json:"timestamp"`
	Payload   CodexPayload `json:"payload"`
}

type CodexPayload struct {
	ID  string `json:"id"`
	CWD string `json:"cwd"`

	ItemType  string       `json:"type"`
	Role      string       `json:"role"`
	Content   []CodexPart  `json:"content"`
	Name      string       `json:"name"`
	Arguments string       `json:"arguments"`
	Output    CodexCallOut `json:"output"`
	Summary   []CodexPart  `json:"summary"`

	Timestamp string `json:"timestamp"`
}

type CodexPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type CodexCallOut struct {
	Content string `json:"content"`
}

func (o *CodexCallOut) UnmarshalJSON(data []byte) error {
	for i, c := range data {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '"':
			return JSONUnmarshal(data[i:], &o.Content)
		case '{':
			type alias CodexCallOut
			return JSONUnmarshal(data[i:], (*alias)(o))
		}
		break
	}
	return nil
}

func (p CodexPayload) flatten() (role, text string) {
	switch p.ItemType {
	case "message":
		var b strings.Builder
		for _, part := range p.Content {
			if part.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(part.Text)
		}
		return p.Role, b.String()
	case "function_call":
		args := p.Arguments
		if len(args) > 400 {
			args = args[:400]
		}
		return "tool", "[call " + p.Name + "] " + args
	case "function_call_output":
		s := p.Output.Content
		if len(s) > 400 {
			s = s[:400]
		}
		return "tool", "[output] " + s
	case "reasoning":
		var b strings.Builder
		for _, part := range p.Summary {
			if part.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(part.Text)
		}
		return "assistant", b.String()
	}
	return "", ""
}
