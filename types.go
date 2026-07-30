package main

import "context"

type Session struct {
	Source    string `json:"source"`
	SourceID  string `json:"source_id"`
	Project   string `json:"project"`
	Title     string `json:"title"`
	StartedAt int64  `json:"started_at_ms"`
	EndedAt   int64  `json:"ended_at_ms"`
	MsgCount  int    `json:"msg_count"`

	// Usage — what the model cost. Sources that record it (pi per-message,
	// codex token_count events, cursor composer usageData) report real
	// numbers; everything else falls back to a chars/4 estimate at ingest,
	// flagged by Estimated so the UI can mark it with a ~.
	Model      string  `json:"model,omitempty"`
	TokensIn   int64   `json:"tokens_in,omitempty"`
	TokensOut  int64   `json:"tokens_out,omitempty"`
	CacheRead  int64   `json:"cache_read,omitempty"`
	CacheWrite int64   `json:"cache_write,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	Estimated  bool    `json:"estimated,omitempty"`

	// Chars is the pre-truncation text size, kept only long enough to derive
	// the fallback token estimate at ingest. Never persisted.
	Chars int64 `json:"-"`

	// Append marks an incremental update for an already-indexed session:
	// only the new messages are carried, and ingest appends them to FTS
	// instead of replacing the session's rows. EndedAt/MsgCount are deltas.
	Append bool `json:"-"`
}

type Message struct {
	SourceID string `json:"source_id"`
	Idx      int    `json:"idx"`
	Role     string `json:"role"`
	TS       int64  `json:"ts_ms"`
	Text     string `json:"text"`
}

type Adapter interface {
	ID() string
	Available() bool

	Scan(ctx context.Context, prev string) (sessions []Session, msgs []Message, next string, err error)

	Fetch(ctx context.Context, sourceID string) ([]Message, error)

	OpenURL(sourceID string) string
}

const excerptMax = 1500
