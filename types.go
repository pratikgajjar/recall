package main

// Session is one chat thread (composer / claude session / codex rollout).
type Session struct {
	Source    string // "cursor" | "claude" | "codex"
	SourceID  string // native id from the source tool
	Project   string // absolute folder path
	Title     string // chat name or first-prompt slug
	StartedAt int64  // unix ms
	EndedAt   int64  // unix ms
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
	// Scan returns sessions+messages plus a new opaque checkpoint string.
	// If prev is non-empty, the adapter should attempt an incremental scan
	// (returning only changed sessions) and produce a fresh checkpoint.
	// If prev is empty or unparseable, a full scan is performed.
	Scan(prev string) (sessions []Session, msgs []Message, next string, err error)
	// Fetch returns the full transcript for one session, untruncated.
	// Used by `recall show` / `recall last` so piped output is faithful.
	Fetch(sourceID string) ([]Message, error)
	// OpenURL returns a best-effort URI/cmd to reopen the chat in its
	// native tool. Empty string if not supported.
	OpenURL(sourceID string) string
}

const excerptMax = 1500
