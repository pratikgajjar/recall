package main

// Session is one chat thread (composer / claude session / codex rollout).
type Session struct {
	Source    string         // "cursor" | "claude" | "codex"
	SourceID  string         // native id from the source tool
	Project   string         // absolute folder path
	Title     string         // chat name or first-prompt slug
	StartedAt int64          // unix ms
	EndedAt   int64          // unix ms
	MsgCount  int
	Meta      map[string]any // extra source-specific bits
}

// Message is one turn inside a session.
type Message struct {
	SourceID string // session.SourceID
	Idx      int
	Role     string // "user" | "assistant" | "tool" | "system"
	TS       int64  // unix ms
	Text     string // trimmed to ~excerptMax
}

// Adapter reads from a single tool's storage.
type Adapter interface {
	ID() string
	Available() bool
	// Scan returns all sessions+messages currently stored.
	// V0 is idempotent (full scan, upsert). V1 will checkpoint.
	Scan() ([]Session, []Message, error)
}

const excerptMax = 1500
