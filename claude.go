package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ClaudeAdapter struct {
	Root string
}

func (a *ClaudeAdapter) ID() string      { return "claude" }
func (a *ClaudeAdapter) Available() bool { _, err := os.Stat(a.Root); return err == nil }

func (a *ClaudeAdapter) Scan(prev string) ([]Session, []Message, string, error) {
	prevMap := parseFileCkpt(prev)
	nextMap := map[string]string{}

	entries, err := os.ReadDir(a.Root)
	if err != nil {
		return nil, nil, "", err
	}
	var sessions []Session
	var msgs []Message
	for _, e := range entries {
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
			tok := fileTok(st)
			nextMap[full] = tok
			if prevMap[full] == tok {
				continue
			}
			sessID := strings.TrimSuffix(name, ".jsonl")
			s, mm, err := a.readSession(full, sessID, project)
			if err != nil {
				continue
			}
			if s != nil {
				sessions = append(sessions, *s)
				msgs = append(msgs, mm...)
			}
		}
	}
	return sessions, msgs, encodeFileCkpt(nextMap), nil
}

func (a *ClaudeAdapter) Fetch(sourceID string) ([]Message, error) {
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
			_, msgs, err := a.readSessionFull(p, sourceID)
			return msgs, err
		}
	}
	return nil, fmt.Errorf("claude session %s not found", sourceID)
}

func (a *ClaudeAdapter) OpenURL(sourceID string) string {
	return "claude --resume " + sourceID
}

func (a *ClaudeAdapter) readSessionFull(path, sessID string) (*Session, []Message, error) {
	return a.readSessionImpl(path, sessID, "", false)
}

func (a *ClaudeAdapter) readSession(path, sessID, project string) (*Session, []Message, error) {
	return a.readSessionImpl(path, sessID, project, true)
}

func (a *ClaudeAdapter) readSessionImpl(path, sessID, project string, truncate bool) (*Session, []Message, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<16), 16<<20)

	var msgs []Message
	idx := 0
	var startedAt, endedAt int64
	var firstUser string
	var cwd string

	for sc.Scan() {
		var ev ClaudeEvent
		if err := JSONUnmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if cwd == "" && ev.CWD != "" {
			cwd = ev.CWD
		}
		if ev.Type != "user" && ev.Type != "assistant" {
			continue
		}
		ts := parseClaudeTime(ev.Timestamp)
		if ts > 0 {
			if startedAt == 0 || ts < startedAt {
				startedAt = ts
			}
			if ts > endedAt {
				endedAt = ts
			}
		}
		text := ev.Message.Text()
		if text == "" {
			continue
		}
		if ev.Type == "user" && firstUser == "" && !looksLikeWrapper(text) {
			firstUser = text
		}
		stored := text
		if truncate && len(stored) > excerptMax {
			stored = stored[:excerptMax]
		}
		msgs = append(msgs, Message{
			SourceID: sessID, Idx: idx, Role: ev.Type, TS: ts, Text: stored,
		})
		idx++
	}
	if len(msgs) == 0 {
		return nil, nil, nil
	}

	if cwd != "" {
		project = cwd
	}
	title := titleFromPrompt(firstUser)
	return &Session{
		Source: "claude", SourceID: sessID,
		Project: project, Title: title,
		StartedAt: startedAt, EndedAt: endedAt,
		MsgCount: len(msgs),
	}, msgs, nil
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

func parseTime(v any) int64 {
	s, _ := v.(string)
	return parseClaudeTime(s)
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

var _ = sort.Strings
