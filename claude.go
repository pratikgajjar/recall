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
	Root string // ~/.claude/projects
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
				continue // unchanged — skip
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

// Fetch re-reads the JSONL for one session and returns untruncated messages.
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

// OpenURL — Claude Code resumes via `claude --resume <session-id>` in the project dir.
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
	// Prefer the cwd stamped on the events over the lossy folder-name reverse.
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

// ClaudeEvent is the typed view of one line in a Claude Code JSONL file.
// Only fields recall consumes are decoded; sonic skips everything else.
type ClaudeEvent struct {
	Type      string        `json:"type"`      // "user" | "assistant" | …
	Timestamp string        `json:"timestamp"` // RFC3339
	CWD       string        `json:"cwd"`
	Message   ClaudeMessage `json:"message"`
}

// ClaudeMessage models the `message` field. Content is sometimes a plain
// string, sometimes an array of typed parts.
type ClaudeMessage struct {
	Role    string            `json:"role"`
	Content ClaudeMessageBody `json:"content"`
}

// ClaudeMessageBody handles content being either a string or an array of parts.
type ClaudeMessageBody struct {
	str   string
	parts []ClaudePart
}

// UnmarshalJSON dispatches on the first non-whitespace byte.
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

// ClaudePart is one element inside a content array.
type ClaudePart struct {
	Type    string `json:"type"` // "text" | "tool_use" | "tool_result"
	Text    string `json:"text"`
	Name    string `json:"name"`    // tool_use
	Content string `json:"content"` // tool_result (only when it's a plain string)
}

// Text flattens a message body to the searchable text excerpt recall stores.
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

// parseTime kept for codex.go / pi.go that pass interface values from raw maps.
func parseTime(v any) int64 {
	s, _ := v.(string)
	return parseClaudeTime(s)
}

// unsanitize converts "-Users-pratikgajjar-code-acme-api" → "/Users/pratikgajjar/code/acme-api".
// We can't perfectly recover '-' inside names; we just replace '-' with '/' at known boundaries.
// This is best-effort; we keep the original in case the user wants to search by either form.
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
	// strip command-message wrappers
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

// ensure stable sort for deterministic output downstream
var _ = sort.Strings
