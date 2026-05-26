package main

import (
	"bufio"
	"encoding/json"
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
		var ev map[string]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		ty, _ := ev["type"].(string)
		ts := parseTime(ev["timestamp"])
		switch ty {
		case "session":
			if id, _ := ev["id"].(string); id != "" {
				sessID = id
			}
			if cwd, _ := ev["cwd"].(string); cwd != "" {
				project = cwd
			}
			startedAt = ts
		case "message":
			m, _ := ev["message"].(map[string]any)
			if m == nil {
				continue
			}
			role, _ := m["role"].(string)
			text := extractPiText(m)
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
		Source: "pi", SourceID: sessID,
		Project: project, Title: titleFromPrompt(firstUser),
		StartedAt: startedAt, EndedAt: endedAt,
		MsgCount: len(msgs),
	}, msgs, nil
}

// extractPiText handles content arrays with mixed parts:
//
//	{type:"text", text:"…"}
//	{type:"thinking", thinking:"…"}
//	{type:"toolCall", name, input}
//	{type:"toolResult", content:[{type:"text", text:"…"}]} or string
//
// For toolResult, message.content can also be a flat string.
func extractPiText(m map[string]any) string {
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
			case "thinking":
				// Skip in FTS — model's chain-of-thought, low signal-to-noise.
			case "toolCall":
				if name, _ := pm["name"].(string); name != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString("[tool:")
					b.WriteString(name)
					b.WriteByte(']')
				}
			case "toolResult":
				inner := pm["content"]
				switch iv := inner.(type) {
				case string:
					if iv != "" {
						if b.Len() > 0 {
							b.WriteByte('\n')
						}
						s := iv
						if len(s) > 400 {
							s = s[:400]
						}
						b.WriteString("[result] ")
						b.WriteString(s)
					}
				case []any:
					for _, ip := range iv {
						ipm, ok := ip.(map[string]any)
						if !ok {
							continue
						}
						if t, _ := ipm["text"].(string); t != "" {
							if b.Len() > 0 {
								b.WriteByte('\n')
							}
							if len(t) > 400 {
								t = t[:400]
							}
							b.WriteString("[result] ")
							b.WriteString(t)
						}
					}
				}
			}
		}
		return b.String()
	}
	return ""
}
