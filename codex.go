package main

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type CodexAdapter struct {
	Root string // ~/.codex/sessions
}

func (a *CodexAdapter) ID() string      { return "codex" }
func (a *CodexAdapter) Available() bool { _, err := os.Stat(a.Root); return err == nil }

func (a *CodexAdapter) Scan(prev string) ([]Session, []Message, string, error) {
	prevMap := parseFileCkpt(prev)
	nextMap := map[string]string{}

	var sessions []Session
	var msgs []Message
	err := filepath.WalkDir(a.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
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
		tok := fileTok(st)
		nextMap[path] = tok
		if prevMap[path] == tok {
			return nil // unchanged — skip
		}
		s, mm, e := a.readSession(path, true)
		if e != nil || s == nil {
			return nil
		}
		sessions = append(sessions, *s)
		msgs = append(msgs, mm...)
		return nil
	})
	return sessions, msgs, encodeFileCkpt(nextMap), err
}

// Fetch walks the sessions tree to find the rollout file for this id, then returns
// untruncated messages. Codex sessions are named rollout-<ts>-<id>.jsonl, so a
// suffix match is enough.
func (a *CodexAdapter) Fetch(sourceID string) ([]Message, error) {
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
	_, msgs, err := a.readSession(found, false)
	return msgs, err
}

// OpenURL — Codex resumes via `codex resume <session-id>` (or `--last` for most recent).
func (a *CodexAdapter) OpenURL(sourceID string) string {
	return "codex resume " + sourceID
}

func (a *CodexAdapter) readSession(path string, truncate bool) (*Session, []Message, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<16), 16<<20)

	var sessID, project string
	var startedAt, endedAt int64
	var msgs []Message
	var firstUser string
	idx := 0

	for sc.Scan() {
		var ev CodexEvent
		if err := JSONUnmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		ts := parseClaudeTime(ev.Timestamp)
		switch ev.Type {
		case "session_meta":
			if ev.Payload.ID != "" {
				sessID = ev.Payload.ID
			}
			if ev.Payload.CWD != "" {
				project = ev.Payload.CWD
			}
			if t := parseClaudeTime(ev.Payload.Timestamp); t > 0 {
				startedAt = t
			} else if ts > 0 {
				startedAt = ts
			}
		case "response_item":
			role, text := ev.Payload.flatten()
			if text == "" {
				continue
			}
			if ts > endedAt {
				endedAt = ts
			}
			if startedAt == 0 || (ts > 0 && ts < startedAt) {
				startedAt = ts
			}
			if role == "user" && firstUser == "" && !looksLikeWrapper(text) {
				firstUser = text
			}
			stored := text
			if truncate && len(stored) > excerptMax {
				stored = stored[:excerptMax]
			}
			msgs = append(msgs, Message{
				SourceID: sessID, Idx: idx, Role: role, TS: ts, Text: stored,
			})
			idx++
		}
	}
	if sessID == "" || len(msgs) == 0 {
		return nil, nil, nil
	}
	return &Session{
		Source: "codex", SourceID: sessID,
		Project: project, Title: titleFromPrompt(firstUser),
		StartedAt: startedAt, EndedAt: endedAt,
		MsgCount: len(msgs),
	}, msgs, nil
}

// CodexEvent models one JSONL line from a Codex rollout file.
// Only the fields recall consumes are typed; sonic skips the rest.
type CodexEvent struct {
	Type      string       `json:"type"` // "session_meta" | "response_item" | …
	Timestamp string       `json:"timestamp"`
	Payload   CodexPayload `json:"payload"`
}

// CodexPayload is the union of fields seen across session_meta and
// response_item.payload variants. Unused fields stay zero-valued.
type CodexPayload struct {
	// session_meta
	ID  string `json:"id"`
	CWD string `json:"cwd"`

	// response_item — every variant carries a `type`.
	ItemType  string       `json:"type"`
	Role      string       `json:"role"`      // message
	Content   []CodexPart  `json:"content"`   // message
	Name      string       `json:"name"`      // function_call
	Arguments string       `json:"arguments"` // function_call
	Output    CodexCallOut `json:"output"`    // function_call_output
	Summary   []CodexPart  `json:"summary"`   // reasoning

	// session_meta also has a timestamp at the payload level.
	Timestamp string `json:"timestamp"`
}

// CodexPart is one element of `content` / `summary` in a response_item.
type CodexPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// CodexCallOut wraps function_call_output.output, which is sometimes an
// object {content:"…"} and sometimes a raw string. UnmarshalJSON handles
// the polymorphism so the rest of the code sees a single `Content` field.
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

// flatten reduces a response_item payload to (role, text) — recall's
// canonical excerpt form.
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
