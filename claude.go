package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ClaudeAdapter struct {
	Root string
}

func (a *ClaudeAdapter) ID() string      { return "claude" }
func (a *ClaudeAdapter) Available() bool { _, err := os.Stat(a.Root); return err == nil }

func (a *ClaudeAdapter) Scan(ctx context.Context, prev string) ([]Session, []Message, string, error) {
	return collectScan(ctx, a, prev)
}

func (a *ClaudeAdapter) ScanStream(ctx context.Context, prev string, emit EmitFunc) error {
	prevMap := parseFileCkpt(prev)
	nextMap := map[string]fileState{}
	be := newBatchEmitter(emit, func() string { return encodeFileCkpt(nextMap) })

	entries, err := os.ReadDir(a.Root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.IsDir() {
			continue
		}
		project := unsanitize(e.Name())
		dir := filepath.Join(a.Root, e.Name())
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			name := f.Name()
			if !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			full := filepath.Join(dir, name)
			st, err := os.Stat(full)
			if err != nil {
				continue
			}
			sessID := strings.TrimSuffix(name, ".jsonl")
			size, mtime := st.Size(), st.ModTime().UnixNano()
			prevSt, ok := prevMap[full]

			if ok && prevSt.Size == size && prevSt.MTime == mtime {
				nextMap[full] = prevSt
				continue
			}
			if ok && size > prevSt.Size && prevSt.Offset <= size {
				if p, err := a.parse(ctx, full, prevSt.Offset, prevSt.Idx, sessID, true); err == nil && len(p.msgs) > 0 {
					nextMap[full] = fileState{Size: size, MTime: mtime, Offset: p.endOffset, Idx: p.nextIdx, SID: sessID}
					if err := be.add(Session{
						Source: "claude", SourceID: sessID, Append: true,
						EndedAt: p.endedAt, MsgCount: len(p.msgs),
					}, p.msgs); err != nil {
						return err
					}
					continue
				}
				// fall through to a full re-read on parse failure / truncation
			}

			p, err := a.parse(ctx, full, 0, 0, sessID, true)
			nextMap[full] = fileState{Size: size, MTime: mtime, Offset: p.endOffset, Idx: p.nextIdx, SID: sessID}
			if err != nil || len(p.msgs) == 0 {
				continue
			}
			proj := project
			if p.cwd != "" {
				proj = p.cwd
			}
			if err := be.add(Session{
				Source: "claude", SourceID: sessID,
				Project: proj, Title: titleFromPrompt(p.firstUser),
				StartedAt: p.startedAt, EndedAt: p.endedAt,
				MsgCount: len(p.msgs),
			}, p.msgs); err != nil {
				return err
			}
		}
	}
	return be.flush()
}

func (a *ClaudeAdapter) Fetch(ctx context.Context, sourceID string) ([]Message, error) {
	entries, err := os.ReadDir(a.Root)
	if err != nil {
		return nil, err
	}
	target := sourceID + ".jsonl"
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(a.Root, e.Name(), target)
		if _, err := os.Stat(p); err == nil {
			res, err := a.parse(ctx, p, 0, 0, sourceID, false)
			return res.msgs, err
		}
	}
	return nil, fmt.Errorf("claude session %s not found", sourceID)
}

func (a *ClaudeAdapter) OpenURL(sourceID string) string {
	return "claude --resume " + sourceID
}

type claudeParse struct {
	msgs               []Message
	startedAt, endedAt int64
	firstUser, cwd     string
	endOffset          int64
	nextIdx            int
}

func (a *ClaudeAdapter) parse(ctx context.Context, path string, startOffset int64, startIdx int, sessID string, truncate bool) (claudeParse, error) {
	res := claudeParse{nextIdx: startIdx}
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
		var ev ClaudeEvent
		if JSONUnmarshal(line, &ev) != nil {
			return nil
		}
		if res.cwd == "" && ev.CWD != "" {
			res.cwd = ev.CWD
		}
		if ev.Type != "user" && ev.Type != "assistant" {
			return nil
		}
		ts := parseClaudeTime(ev.Timestamp)
		if ts > 0 {
			if res.startedAt == 0 || ts < res.startedAt {
				res.startedAt = ts
			}
			if ts > res.endedAt {
				res.endedAt = ts
			}
		}
		text := ev.Message.Text()
		if text == "" {
			return nil
		}
		if ev.Type == "user" && res.firstUser == "" && !looksLikeWrapper(text) {
			res.firstUser = text
		}
		if truncate && len(text) > excerptMax {
			text = text[:excerptMax]
		}
		res.msgs = append(res.msgs, Message{SourceID: sessID, Idx: idx, Role: ev.Type, TS: ts, Text: text})
		idx++
		return nil
	})
	res.endOffset = startOffset + consumed
	res.nextIdx = idx
	return res, err
}

type ClaudeEvent struct {
	Type      string        `json:"type"`
	Timestamp string        `json:"timestamp"`
	CWD       string        `json:"cwd"`
	Message   ClaudeMessage `json:"message"`
}

type ClaudeMessage struct {
	Role    string            `json:"role"`
	Content ClaudeMessageBody `json:"content"`
}

type ClaudeMessageBody struct {
	str   string
	parts []ClaudePart
}

func (b *ClaudeMessageBody) UnmarshalJSON(data []byte) error {
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

type ClaudePart struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

func (m ClaudeMessage) Text() string {
	if m.Content.str != "" {
		return m.Content.str
	}
	if len(m.Content.parts) == 0 {
		return ""
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
		case "tool_use":
			if p.Name == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString("[tool_use:")
			b.WriteString(p.Name)
			b.WriteByte(']')
		case "tool_result":
			if p.Content == "" {
				continue
			}
			t := p.Content
			if len(t) > 400 {
				t = t[:400]
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString("[tool_result] ")
			b.WriteString(t)
		}
	}
	return b.String()
}

func parseClaudeTime(s string) int64 {
	if s == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

func unsanitize(s string) string {
	if !strings.HasPrefix(s, "-") {
		return s
	}
	return strings.ReplaceAll(s, "-", "/")
}

func looksLikeWrapper(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, "<command-message>") ||
		strings.HasPrefix(t, "<command-name>") ||
		strings.HasPrefix(t, "<local-command-") ||
		strings.HasPrefix(t, "# AGENTS.md") ||
		strings.HasPrefix(t, "<environment_context>") ||
		strings.HasPrefix(t, "<user_instructions>")
}

func titleFromPrompt(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}

	if i := strings.Index(p, "</command-args>"); i > 0 {
		p = p[:i]
	}
	p = strings.NewReplacer("<command-name>", "", "</command-name>", " ",
		"<command-args>", "", "<command-message>", "", "</command-message>", " ").Replace(p)
	p = strings.Join(strings.Fields(p), " ")
	if len(p) > 80 {
		p = p[:80]
	}
	return p
}
