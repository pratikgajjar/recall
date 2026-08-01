package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type CodexAdapter struct {
	Root string
	// batchSessions caps sessions per ingest batch (0 = default). Test-only.
	batchSessions int
}

func (a *CodexAdapter) ID() string      { return "codex" }
func (a *CodexAdapter) Available() bool { _, err := os.Stat(a.Root); return err == nil }

func (a *CodexAdapter) Scan(ctx context.Context, prev string) ([]Session, []Message, string, error) {
	return collectScan(ctx, a, prev)
}

func (a *CodexAdapter) ScanStream(ctx context.Context, prev string, emit EmitFunc) error {
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
				nextMap[path] = fileState{Size: size, MTime: mtime, Offset: p.endOffset, Idx: p.nextIdx, SID: prevSt.SID}
				s := p.session(prevSt.SID)
				s.Append = true
				s.StartedAt, s.Project, s.Title = 0, "", ""
				return be.add(s, p.msgs)
			}
		}

		p, e := a.parse(ctx, path, 0, 0, "", true)
		nextMap[path] = fileState{Size: size, MTime: mtime, Offset: p.endOffset, Idx: p.nextIdx, SID: p.sessID}
		if e != nil || p.sessID == "" || len(p.msgs) == 0 {
			return nil
		}
		return be.add(p.session(p.sessID), p.msgs)
	})
	if err != nil {
		return err
	}
	return be.flush()
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
	usage              Session // usage fields only; see (codexParse).session
}

func (p codexParse) session(sourceID string) Session {
	s := p.usage
	s.Source, s.SourceID = "codex", sourceID
	s.Project, s.Title = p.project, titleFromPrompt(p.firstUser)
	s.StartedAt, s.EndedAt = p.startedAt, p.endedAt
	s.MsgCount = len(p.msgs)
	return s
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
	callNames := map[string]string{} // call_id -> tool name, for attributing outputs
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
		case "turn_context":
			if ev.Payload.Model != "" {
				res.usage.Model = ev.Payload.Model
			}
		case "event_msg":
			// token_count reports a running total for the session, so the last
			// one wins — summing them would multiply-count every earlier turn.
			if tu := ev.Payload.Info.TotalTokenUsage; tu.TotalTokens > 0 {
				res.usage.TokensIn = tu.InputTokens - tu.CachedInputTokens
				res.usage.TokensOut = tu.OutputTokens
				res.usage.CacheRead = tu.CachedInputTokens
			}
		case "response_item":
			role, text := ev.Payload.flatten(callNames)
			if text == "" {
				return nil
			}
			res.usage.Chars += int64(len(text))
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

	Model string         `json:"model"` // on type=turn_context
	Info  CodexUsageInfo `json:"info"`  // on type=event_msg (token_count)

	ItemType  string       `json:"type"`
	Role      string       `json:"role"`
	Content   []CodexPart  `json:"content"`
	Name      string       `json:"name"`
	Arguments string       `json:"arguments"`
	CallID    string       `json:"call_id"` // links a call to its output
	Output    CodexCallOut `json:"output"`
	Summary   codexParts   `json:"summary"`

	Timestamp string `json:"timestamp"`
}

// CodexUsageInfo carries the running token tally codex emits in token_count
// events. Cost is absent: codex logs usage but not what it billed.
type CodexUsageInfo struct {
	TotalTokenUsage struct {
		InputTokens       int64 `json:"input_tokens"`
		CachedInputTokens int64 `json:"cached_input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
		TotalTokens       int64 `json:"total_tokens"`
	} `json:"total_token_usage"`
}

type CodexPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type CodexCallOut struct {
	Content string `json:"content"`
}

// codexParts decodes codex's `summary`, which is an array of parts on a
// response_item but a bare string ("auto") on a turn_context. Decoding it
// strictly fails the whole turn_context line — and that line is the only place
// the model name appears, so every codex session indexed as model-unknown.
type codexParts []CodexPart

func (s *codexParts) UnmarshalJSON(data []byte) error {
	for i, c := range data {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '[':
			type alias codexParts
			return JSONUnmarshal(data[i:], (*alias)(s))
		}
		break
	}
	return nil // a scalar summary carries no parts
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

// flatten renders one payload. callNames maps a call_id to the tool that was
// invoked, so a result can be attributed the way pi's already is: codex records
// the link but the transcript was throwing it away, leaving "## 7 tool" with no
// indication of which tool produced the output below it.
func (p CodexPayload) flatten(callNames map[string]string) (role, text string) {
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
		// Was 400 characters of raw JSON per call, which buries the one field that
		// identifies the call. Same clipped summary every other adapter uses.
		if p.CallID != "" && p.Name != "" && callNames != nil {
			callNames[p.CallID] = p.Name
		}
		// The role names the tool, so repeating it in the body is the same
		// redundancy the message header used to carry.
		if a := argSummary(json.RawMessage(p.Arguments)); a != "" {
			return "tool:" + p.Name, a
		}
		args := p.Arguments
		if len(args) > 400 {
			args = args[:400]
		}
		return "tool:" + p.Name, args
	case "function_call_output":
		s := p.Output.Content
		if len(s) > 400 {
			s = s[:400]
		}
		// The "[output] " prefix said what the role already says; naming the tool
		// in the role is both shorter and more informative.
		role = "tool"
		if n := callNames[p.CallID]; n != "" {
			role = "tool:" + n
		}
		return role, s
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
