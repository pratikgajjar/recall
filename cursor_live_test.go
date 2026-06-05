package main

// Live parity check against the host's real Cursor DB. Runs only when
// RECALL_LIVE_CURSOR_PARITY=1 so CI / fresh checkouts stay hermetic.
// Use: RECALL_LIVE_CURSOR_PARITY=1 go test -run TestLuaParityCursorLive -v

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLuaParityCursorLive(t *testing.T) {
	if os.Getenv("RECALL_LIVE_CURSOR_PARITY") != "1" {
		t.Skip("set RECALL_LIVE_CURSOR_PARITY=1 to run against the real Cursor DB")
	}
	home, _ := os.UserHomeDir()
	userDir := filepath.Join(home, "Library", "Application Support", "Cursor", "User")
	db := filepath.Join(userDir, "globalStorage", "state.vscdb")
	if _, err := os.Stat(db); err != nil {
		t.Skipf("no live Cursor DB at %s", db)
	}

	goAd := &CursorAdapter{UserDir: userDir}
	luaAd := luaAdapterForKV(t, "plugins/cursor.lua", db)

	gs, gm, _, err := goAd.Scan(context.Background(), "")
	if err != nil {
		t.Fatalf("go scan: %v", err)
	}
	ls, lm, _, err := luaAd.Scan(context.Background(), "")
	if err != nil {
		t.Fatalf("lua scan: %v", err)
	}
	t.Logf("go: %d sessions / %d messages", len(gs), len(gm))
	t.Logf("lua: %d sessions / %d messages", len(ls), len(lm))

	compareSessionsIgnoreProject(t, "cursor-live", gs, ls)
	compareMessages(t, "cursor-live", gm, lm)
}
