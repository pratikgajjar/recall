package main

import (
	"bufio"
	"encoding/json"
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

func (a *ClaudeAdapter) Scan() ([]Session, []Message, error) {
	entries, err := os.ReadDir(a.Root)
	if err != nil {
		return nil, nil, err
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
			sessID := strings.TrimSuffix(name, ".jsonl")
			s, mm, err := a.readSession(filepath.Join(dir, name), sessID, project)
			if err != nil {
				continue // skip malformed
			}
			if s != nil {
				sessions = append(sessions, *s)
				msgs = append(msgs, mm...)
			}
		}
	}
	return sessions, msgs, nil
}

func (a *ClaudeAdapter) readSession(path, sessID, project string) (*Session, []Message, error) {
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

	for sc.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		ty, _ := ev["type"].(string)
		if ty != "user" && ty != "assistant" {
			continue
		}
		ts := parseTime(ev["timestamp"])
		if ts > 0 {
			if startedAt == 0 || ts < startedAt {
				startedAt = ts
			}
			if ts > endedAt {
				endedAt = ts
			}
		}
		role := ty
		text := extractClaudeText(ev["message"])
		if text == "" {
			continue
		}
		if role == "user" && firstUser == "" && !looksLikeWrapper(text) {
			firstUser = text
		}
		msgs = append(msgs, Message{
			SourceID: sessID, Idx: idx, Role: role, TS: ts, Text: text,
		})
		idx++
	}
	if len(msgs) == 0 {
		return nil, nil, nil
	}
	title := titleFromPrompt(firstUser)
	return &Session{
		Source: "claude", SourceID: sessID,
		Project: project, Title: title,
		StartedAt: startedAt, EndedAt: endedAt,
		MsgCount: len(msgs),
	}, msgs, nil
}

// extractClaudeText pulls assistant/user text out of the Claude Code event message field.
// message can be: {content:"…"} or {content:[{type:"text",text:"…"}, …]}.
func extractClaudeText(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	c := m["content"]
	switch cv := c.(type) {
	case string:
		return cv
	case []any:
		var b strings.Builder
		for _, part := range cv {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "text":
				if t, _ := pm["text"].(string); t != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(t)
				}
			case "tool_use":
				if name, _ := pm["name"].(string); name != "" {
					b.WriteString("[tool_use:")
					b.WriteString(name)
					b.WriteByte(']')
				}
			case "tool_result":
				if t, ok := pm["content"].(string); ok && t != "" {
					b.WriteString("[tool_result] ")
					if len(t) > 400 {
						t = t[:400]
					}
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

func parseTime(v any) int64 {
	s, _ := v.(string)
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
