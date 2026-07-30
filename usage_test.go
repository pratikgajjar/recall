package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// piMsgUsage is an assistant turn carrying the usage block pi records on every
// real API response — the numbers `/usage` sums up inside the agent.
func piMsgUsage(text, ts, model string, in, out, cacheR, cacheW int64, cost float64) string {
	return fmt.Sprintf(
		`{"type":"message","timestamp":%q,"message":{"role":"assistant","model":%q,"content":%q,`+
			`"usage":{"input":%d,"output":%d,"cacheRead":%d,"cacheWrite":%d,`+
			`"cost":{"total":%g}}}}`,
		ts, model, text, in, out, cacheR, cacheW, cost)
}

// Real usage must be summed across turns and win over any estimate.
func TestPiUsageSummed(t *testing.T) {
	root := t.TempDir()
	path := piPath(root, "sess-usage-1")
	writeLines(t, path,
		piSession("sess-usage-1", "/work/web", "2026-05-03T08:00:00Z"),
		`{"type":"model_change","modelId":"claude-opus-4-8"}`,
		piMsg("user", "add a cache", "2026-05-03T08:00:02Z"),
		piMsgUsage("Done.", "2026-05-03T08:00:06Z", "claude-opus-4-8", 10, 100, 1000, 50, 0.25),
		piMsgUsage("Also this.", "2026-05-03T08:00:09Z", "claude-opus-4-8", 5, 40, 2000, 10, 0.10),
	)
	sessions, _, _, err := (&PiAdapter{Root: root}).Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sessions))
	}
	s := sessions[0]
	if s.Model != "claude-opus-4-8" {
		t.Errorf("model = %q, want claude-opus-4-8", s.Model)
	}
	if s.TokensIn != 15 || s.TokensOut != 140 {
		t.Errorf("tokens in/out = %d/%d, want 15/140", s.TokensIn, s.TokensOut)
	}
	if s.CacheRead != 3000 || s.CacheWrite != 60 {
		t.Errorf("cache read/write = %d/%d, want 3000/60", s.CacheRead, s.CacheWrite)
	}
	if s.CostUSD < 0.34 || s.CostUSD > 0.36 {
		t.Errorf("cost = %v, want ~0.35", s.CostUSD)
	}

	// Real numbers present ⇒ ingest must not overwrite them with an estimate.
	got := withEstimate(s)
	if got.Estimated {
		t.Error("session with real usage must not be flagged estimated")
	}
	if got.TokensIn != 15 {
		t.Errorf("estimate clobbered real tokens: %d", got.TokensIn)
	}
}

// A source that reports nothing falls back to chars/charsPerToken.
func TestUsageEstimateFallback(t *testing.T) {
	s := withEstimate(Session{Chars: 3000})
	if !s.Estimated {
		t.Error("want Estimated=true when no usage was reported")
	}
	if want := int64(3000 / charsPerToken); s.TokensIn != want {
		t.Errorf("tokens = %d, want %d", s.TokensIn, want)
	}

	// Nothing to go on at all: stay at zero rather than invent a number.
	if e := withEstimate(Session{}); e.Estimated || e.TokensIn != 0 {
		t.Errorf("empty session should not be estimated, got %+v", e)
	}
}

// Codex reports a *running* session total, so the last event wins; summing
// them would multiply-count every earlier turn.
func TestCodexTokenCountIsRunningTotal(t *testing.T) {
	root := t.TempDir()
	path := codexPath(root, "sess-codex-usage")
	tokenCount := func(in, cached, out, total int64) string {
		return fmt.Sprintf(
			`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":`+
				`{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"total_tokens":%d}}}}`,
			in, cached, out, total)
	}
	writeLines(t, path,
		codexMeta("sess-codex-usage", "/work/api", "2026-05-03T09:00:00Z"),
		`{"type":"turn_context","payload":{"model":"gpt-5.6-sol"}}`,
		codexMsg("user", "ship it", "2026-05-03T09:00:01Z"),
		tokenCount(500, 100, 50, 550),
		codexMsg("assistant", "shipped", "2026-05-03T09:00:05Z"),
		tokenCount(1200, 400, 90, 1290),
	)
	sessions, _, _, err := (&CodexAdapter{Root: root}).Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sessions))
	}
	s := sessions[0]
	if s.Model != "gpt-5.6-sol" {
		t.Errorf("model = %q", s.Model)
	}
	// Last event only: input minus the cached portion, which is billed apart.
	if s.TokensIn != 800 || s.CacheRead != 400 || s.TokensOut != 90 {
		t.Errorf("tokens in/cache/out = %d/%d/%d, want 800/400/90",
			s.TokensIn, s.CacheRead, s.TokensOut)
	}
}

func TestFmtTokens(t *testing.T) {
	cases := []struct {
		n    int64
		est  bool
		want string
	}{
		{0, false, "-"},
		{0, true, "-"},
		{512, false, "512"},
		{2500, false, "2k"},
		{1_500_000, false, "1.5M"},
		{2_000_000_000, false, "2.0B"},
		{1_500_000, true, "~1.5M"},
	}
	for _, c := range cases {
		if got := fmtTokens(c.n, c.est); got != c.want {
			t.Errorf("fmtTokens(%d, %v) = %q, want %q", c.n, c.est, got, c.want)
		}
	}
}

func TestFmtCost(t *testing.T) {
	for _, c := range []struct {
		usd  float64
		want string
	}{{0, "-"}, {0.35, "35¢"}, {12.5, "$12.50"}} {
		if got := fmtCost(c.usd); got != c.want {
			t.Errorf("fmtCost(%v) = %q, want %q", c.usd, got, c.want)
		}
	}
}

// Usage must survive a round trip through the index and come back out of
// Stats/ModelStats aggregated.
func TestUsagePersistedAndAggregated(t *testing.T) {
	ix := newTestIndex(t)
	err := ix.IngestBatch(context.Background(), "pi",
		[]Session{
			{Source: "pi", SourceID: "a", Project: "/work/web", Title: "one",
				MsgCount: 1, Model: "opus", TokensIn: 100, TokensOut: 10,
				CacheRead: 900, CacheWrite: 5, CostUSD: 1.25},
			{Source: "pi", SourceID: "b", Project: "/work/web", Title: "two",
				MsgCount: 1, Model: "opus", TokensIn: 50, TokensOut: 5,
				CacheRead: 100, CacheWrite: 0, CostUSD: 0.75},
		},
		[]Message{
			{SourceID: "a", Idx: 0, Role: "user", Text: "hello one"},
			{SourceID: "b", Idx: 0, Role: "user", Text: "hello two"},
		})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := ix.Stats(SearchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 stat row, got %d", len(rows))
	}
	// 100+10+900+5 + 50+5+100+0
	if rows[0].Tokens != 1170 {
		t.Errorf("tokens = %d, want 1170", rows[0].Tokens)
	}
	if rows[0].CostUSD < 1.99 || rows[0].CostUSD > 2.01 {
		t.Errorf("cost = %v, want ~2.00", rows[0].CostUSD)
	}
	if rows[0].Estimated {
		t.Error("real usage must not be reported as estimated")
	}

	models, err := ix.ModelStats(SearchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("want 1 model row, got %d", len(models))
	}
	if models[0].Model != "opus" || models[0].Sessions != 2 {
		t.Errorf("model row = %+v", models[0])
	}
	if models[0].CacheRead != 1000 {
		t.Errorf("cache read = %d, want 1000", models[0].CacheRead)
	}
}

// A session with no model must not create an "(unknown)" bucket.
func TestModelStatsSkipsUnknown(t *testing.T) {
	ix := newTestIndex(t)
	if err := ix.IngestBatch(context.Background(), "claude",
		[]Session{{Source: "claude", SourceID: "x", Title: "no model", MsgCount: 1}},
		[]Message{{SourceID: "x", Idx: 0, Role: "user", Text: strings.Repeat("a", 300)}},
	); err != nil {
		t.Fatal(err)
	}
	models, err := ix.ModelStats(SearchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 0 {
		t.Errorf("want no model rows, got %+v", models)
	}
}
