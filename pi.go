package main

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// PiAdapter indexes the pi harness's session store. Files live at
//
//	~/.pi/agent/sessions/<sanitized-cwd>/<timestamp>_<uuid>.jsonl
//
// First line is `{type:"session", id, timestamp, cwd}`; subsequent lines are
// `{type:"message", id, parentId, timestamp, message:{role, content:[...]}}`.
// content parts use `type` ∈ {"text","thinking","toolCall","toolResult"}.
type PiAdapter struct {
	Root string // ~/.pi/agent/sessions
}

func (a *PiAdapter) ID() string      { return "pi" }
func (a *PiAdapter) Available() bool { _, err := os.Stat(a.Root); return err == nil }

func (a *PiAdapter) Scan(prev string) ([]Session, []Message, string, error) {
	prevMap := parseFileCkpt(prev)
	nextMap := map[string]string{}

	var sessions []Session
	var msgs []Message
	err := filepath.WalkDir(a.Root, func(path string, d fs.DirEntry, err error) error {
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
		tok := fileTok(st)
		nextMap[path] = tok
		if prevMap[path] == tok {
			return nil
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

func (a *PiAdapter) Fetch(sourceID string) ([]Message, error) {
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
	_, msgs, err := a.readSession(found, false)
	return msgs, err
}

// OpenURL — pi doesn't expose a resume command line yet; surface the file path
// so the user can `cat` / `less` / pipe it as they like.
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

func (a *PiAdapter) readSession(path string, truncate bool) (*Session, []Message, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<16), 16<<20)

	var sessID, project string
	var startedAt, endedAt int64
	var firstUser string
	var msgs []Message
	idx := 0

	for sc.Scan() {
		var ev PiEvent
		if err := JSONUnmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		ts := parseClaudeTime(ev.Timestamp)
		switch ev.Type {
		case "session":
			if ev.ID != "" {
				sessID = ev.ID
			}
			if ev.CWD != "" {
				project = ev.CWD
			}
			startedAt = ts
		case "message":
			text := ev.Message.Text()
			if text == "" {
				continue
			}
			if ts > endedAt {
				endedAt = ts
			}
			if startedAt == 0 || (ts > 0 && ts < startedAt) {
				startedAt = ts
			}
			if ev.Message.Role == "user" && firstUser == "" && !looksLikeWrapper(text) {
				firstUser = text
			}
			stored := text
			if truncate && len(stored) > excerptMax {
				stored = stored[:excerptMax]
			}
			msgs = append(msgs, Message{
				SourceID: sessID, Idx: idx, Role: ev.Message.Role, TS: ts, Text: stored,
			})
			idx++
		}
	}
	if sessID == "" || len(msgs) == 0 {
		return nil, nil, nil
	}
	return &Session{
		Source: "pi", SourceID: sessID,
		Project: project, Title: titleFromPrompt(firstUser),
		StartedAt: startedAt, EndedAt: endedAt,
		MsgCount: len(msgs),
	}, msgs, nil
}

// PiEvent is one line in a pi JSONL session file.
type PiEvent struct {
	Type      string    `json:"type"` // "session" | "message" | …
	ID        string    `json:"id"`   // session id (when type=session)
	CWD       string    `json:"cwd"`  // session cwd
	Timestamp string    `json:"timestamp"`
	Message   PiMessage `json:"message"`
}

// PiMessage is the body of a type:"message" event.
type PiMessage struct {
	Role    string        `json:"role"` // user | assistant | toolResult | system
	Content PiMessageBody `json:"content"`
}

// PiMessageBody is either a plain string or an array of typed parts.
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

// PiPart is one element inside a content array.
//
//	{type:"text",       text:"…"}
//	{type:"thinking",   thinking:"…"}    — dropped, low signal
//	{type:"toolCall",   name:"bash", input:{…}}
//	{type:"toolResult", content: string | [PiPart]}
type PiPart struct {
	Type    string       `json:"type"`
	Text    string       `json:"text"`
	Name    string       `json:"name"`
	Content PiToolResult `json:"content"`
}

// PiToolResult is content of a toolResult — string or array of parts.
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

// Text flattens a pi message body to the searchable excerpt.
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
			// drop — chain-of-thought, very noisy
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
