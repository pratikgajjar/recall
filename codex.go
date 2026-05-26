package main

import (
	"bufio"
	"encoding/json"
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
		var ev map[string]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		ty, _ := ev["type"].(string)
		ts := parseTime(ev["timestamp"])
		switch ty {
		case "session_meta":
			p, _ := ev["payload"].(map[string]any)
			if p == nil {
				continue
			}
			if id, _ := p["id"].(string); id != "" {
				sessID = id
			}
			if cwd, _ := p["cwd"].(string); cwd != "" {
				project = cwd
			}
			startedAt = parseTime(p["timestamp"])
			if startedAt == 0 {
				startedAt = ts
			}
		case "response_item":
			p, _ := ev["payload"].(map[string]any)
			if p == nil {
				continue
			}
			role, text := extractCodexMsg(p)
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

// extractCodexMsg handles response_item.payload variants:
//
//	{type:"message", role:"user"|"assistant", content:[{type:"input_text"|"output_text"|"text", text:"…"}]}
//	{type:"function_call", name, arguments}
//	{type:"function_call_output", call_id, output:{...}}
//	{type:"reasoning", summary:[{type:"summary_text", text:"…"}]}
func extractCodexMsg(p map[string]any) (role, text string) {
	switch p["type"] {
	case "message":
		role, _ = p["role"].(string)
		c, ok := p["content"].([]any)
		if !ok {
			return role, ""
		}
		var b strings.Builder
		for _, part := range c {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := pm["text"].(string); t != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(t)
			}
		}
		return role, b.String()
	case "function_call":
		name, _ := p["name"].(string)
		args, _ := p["arguments"].(string)
		if len(args) > 400 {
			args = args[:400]
		}
		return "tool", "[call " + name + "] " + args
	case "function_call_output":
		out, _ := p["output"].(map[string]any)
		if out == nil {
			return "tool", ""
		}
		s, _ := out["content"].(string)
		if s == "" {
			b, _ := json.Marshal(out)
			s = string(b)
		}
		if len(s) > 400 {
			s = s[:400]
		}
		return "tool", "[output] " + s
	case "reasoning":
		sum, ok := p["summary"].([]any)
		if !ok {
			return "", ""
		}
		var b strings.Builder
		for _, part := range sum {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := pm["text"].(string); t != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(t)
			}
		}
		return "assistant", b.String()
	}
	return "", ""
}
