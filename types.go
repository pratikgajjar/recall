package main

type Session struct {
	Source    string         `json:"source"`
	SourceID  string         `json:"source_id"`
	Project   string         `json:"project"`
	Title     string         `json:"title"`
	StartedAt int64          `json:"started_at_ms"`
	EndedAt   int64          `json:"ended_at_ms"`
	MsgCount  int            `json:"msg_count"`
	Meta      map[string]any `json:"meta,omitempty"`
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

	Scan(prev string) (sessions []Session, msgs []Message, next string, err error)

	Fetch(sourceID string) ([]Message, error)

	OpenURL(sourceID string) string
}

const excerptMax = 1500
