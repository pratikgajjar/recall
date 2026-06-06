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

// CursorAgentAdapter indexes Cursor Agent CLI transcripts, stored at
// ~/.cursor/projects/<slug>/agent-transcripts/<id>/<id>.jsonl. Each line is one
// turn: {"role":"user"|"assistant","message":{"content":[parts]}} where a part
// is {"type":"text",...} or {"type":"tool_use","name":...}. There are no
// per-event timestamps, so sessions are dated by file mtime. This is the
// canonical built-in; plugins/cursor-agent.lua mirrors it (parity-tested) as a
// Lua-tier example and override template.
type CursorAgentAdapter struct {
	Root string // ~/.cursor/projects
	// batchSessions caps sessions per ingest batch (0 = default). Test-only.
	batchSessions int
}

func (a *CursorAgentAdapter) ID() string { return "cursor-agent" }
func (a *CursorAgentAdapter) Available() bool {
	_, err := os.Stat(a.Root)
	return err == nil
}

func (a *CursorAgentAdapter) Scan(ctx context.Context, prev string) ([]Session, []Message, string, error) {
	return collectScan(ctx, a, prev)
}

// isAgentTranscript matches only the agent-transcripts/*.jsonl files, so other
// JSONL under ~/.cursor/projects (if any) is ignored.
func isAgentTranscript(path, name string) bool {
	return strings.HasSuffix(name, ".jsonl") && strings.Contains(path, "/agent-transcripts/")
}

// cursorAgentSlug returns the project slug: the directory segment immediately
// before /agent-transcripts/ (the absolute cwd with slashes flattened to dashes).
func cursorAgentSlug(path string) string {
	if i := strings.Index(path, "/agent-transcripts/"); i >= 0 {
		return filepath.Base(path[:i])
	}
	return ""
}

func (a *CursorAgentAdapter) ScanStream(ctx context.Context, prev string, emit EmitFunc) error {
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
		if !isAgentTranscript(path, d.Name()) {
			return nil
		}
		st, sErr := d.Info()
		if sErr != nil {
			return nil
		}
		size, mtime := st.Size(), st.ModTime().UnixNano()
		mtimeMs := mtime / 1e6
		prevSt, ok := prevMap[path]

		if ok && prevSt.Size == size && prevSt.MTime == mtime {
			nextMap[path] = prevSt
			return nil
		}
		// Append-only growth: resume from the last byte offset.
		if ok && prevSt.SID != "" && size > prevSt.Size && prevSt.Offset <= size {
			if p, e := a.parse(ctx, path, prevSt.Offset, prevSt.Idx, prevSt.SID, true); e == nil && len(p.msgs) > 0 {
				nextMap[path] = fileState{Size: size, MTime: mtime, Offset: p.endOffset, Idx: p.nextIdx, SID: prevSt.SID}
				return be.add(Session{
					Source: "cursor-agent", SourceID: prevSt.SID, Append: true,
					EndedAt: mtimeMs, MsgCount: len(p.msgs),
				}, p.msgs)
			}
		}

		sid := stem(d.Name())
		p, e := a.parse(ctx, path, 0, 0, sid, true)
		nextMap[path] = fileState{Size: size, MTime: mtime, Offset: p.endOffset, Idx: p.nextIdx, SID: sid}
		if e != nil || len(p.msgs) == 0 {
			return nil
		}
		return be.add(Session{
			Source: "cursor-agent", SourceID: sid,
			Project: cursorAgentSlug(path), Title: titleFromPrompt(p.firstUser),
			StartedAt: mtimeMs, EndedAt: mtimeMs, MsgCount: len(p.msgs),
		}, p.msgs)
	})
	if err != nil {
		return err
	}
	return be.flush()
}

func (a *CursorAgentAdapter) Fetch(ctx context.Context, sourceID string) ([]Message, error) {
	target := sourceID + ".jsonl"
	var found string
	_ = filepath.WalkDir(a.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Name() == target && strings.Contains(path, "/agent-transcripts/") {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if found == "" {
		return nil, fmt.Errorf("cursor-agent session %s not found", sourceID)
	}
	res, err := a.parse(ctx, found, 0, 0, sourceID, false)
	return res.msgs, err
}

func (a *CursorAgentAdapter) OpenURL(sourceID string) string {
	return "cursor-agent --resume " + sourceID
}

type cursorAgentParse struct {
	sessID    string
	firstUser string
	msgs      []Message
	endOffset int64
	nextIdx   int
}

func (a *CursorAgentAdapter) parse(ctx context.Context, path string, startOffset int64, startIdx int, knownSID string, truncate bool) (cursorAgentParse, error) {
	res := cursorAgentParse{sessID: knownSID, nextIdx: startIdx}
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
		var rec cursorAgentRecord
		if JSONUnmarshal(line, &rec) != nil {
			return nil
		}
		if rec.Role != "user" && rec.Role != "assistant" {
			return nil
		}
		text := flattenCursorAgent(rec.Message.Content, rec.Role)
		if text == "" {
			return nil
		}
		if rec.Role == "user" && res.firstUser == "" && !looksLikeWrapper(text) {
			res.firstUser = text
		}
		if truncate && len(text) > excerptMax {
			text = text[:excerptMax]
		}
		res.msgs = append(res.msgs, Message{SourceID: res.sessID, Idx: idx, Role: rec.Role, TS: 0, Text: text})
		idx++
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

type cursorAgentRecord struct {
	Role    string `json:"role"`
	Message struct {
		Content []cursorAgentPart `json:"content"`
	} `json:"message"`
}

type cursorAgentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
}

// flattenCursorAgent joins a turn's parts: text verbatim, tool_use as
// "[tool_use:Name]". User text has its <timestamp>/<user_query> wrappers
// stripped so titles and indexed text are the bare prompt.
func flattenCursorAgent(parts []cursorAgentPart, role string) string {
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text":
			t := p.Text
			if role == "user" {
				t = stripCursorAgentWrappers(t)
			}
			if t == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(t)
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
		}
	}
	return b.String()
}

func stripCursorAgentWrappers(s string) string {
	for {
		i := strings.Index(s, "<timestamp>")
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], "</timestamp>")
		if j < 0 {
			break
		}
		s = s[:i] + s[i+j+len("</timestamp>"):]
	}
	s = strings.ReplaceAll(s, "<user_query>", "")
	s = strings.ReplaceAll(s, "</user_query>", "")
	return strings.TrimSpace(s)
}
