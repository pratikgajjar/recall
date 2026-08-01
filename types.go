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

// Search ranks a pool of the best rows by bm25 and applies the recency tiebreak
// to those, rather than to every match. poolFactor sets how much bigger the pool
// is than the caller's limit; poolMin keeps it sane for small limits.
const (
	poolFactor = 8
	poolMin    = 200
)

// tailWeight discounts matches in the tail column — everything past the opening
// of a long message. Deep text should be findable when it is the only thing that
// matches, without outranking a message whose opening is about the query.
const tailWeight = 0.25

// Windowing bounds for the tail column (see splitForIndex).
const (
	// maxChunks bounds a single message's windows. At 40 it left 3.5M
	// characters of real corpus unreachable — 7 messages, but among them a
	// pasted design document and an article, not only the stack traces and HAR
	// dumps you would expect. Indexing all of it costs 1.4% more rows and
	// measured no ranking change at all, since content like that competes for
	// no ordinary query.
	//
	// 1000 windows is 1.5MB of text in one message; past that it is a file
	// somebody pasted, and its opening plus a megabyte is ample to find it by.
	maxChunks    = 1000
	chunkOverlap = 150
	chunkSnap    = 200
)

// indexTextMax is how much of a message an adapter hands to the indexer. It was
// excerptMax, which meant text was already truncated before the indexer saw it —
// so windowing the tail was a silent no-op on every source. The bound remains
// only to keep a pasted megabyte out of memory.
const indexTextMax = maxChunks * excerptMax

// When per-session collapsing leaves a search short of what was asked for, the
// pool is widened and read again.
const (
	poolGrowthFactor = 4
	poolGrowthSteps  = 2
)
