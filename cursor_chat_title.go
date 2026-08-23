package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Cursor Agent CLI transcripts live under ~/.cursor/projects, but the title
// shown in the Cursor sidebar is written separately to
// ~/.cursor/chats/<workspace-hash>/<session-id>/meta.json. Without this lookup,
// recall titles collapse to the first user-prompt snippet (often a skill dump).

var cursorChatTitleCache sync.Map // chatsRoot -> map[sessionID]string

func defaultCursorChatsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cursor", "chats")
}

func cursorSidebarTitle(titles map[string]string, sessionID, fallback string) string {
	if t := strings.TrimSpace(titles[sessionID]); t != "" {
		return t
	}
	return fallback
}

func loadCursorChatTitleMap(chatsRoot string) map[string]string {
	if chatsRoot == "" {
		return map[string]string{}
	}
	if cached, ok := cursorChatTitleCache.Load(chatsRoot); ok {
		return cached.(map[string]string)
	}
	out := map[string]string{}
	workspaces, err := os.ReadDir(chatsRoot)
	if err != nil {
		cursorChatTitleCache.Store(chatsRoot, out)
		return out
	}
	for _, ws := range workspaces {
		if !ws.IsDir() {
			continue
		}
		wsDir := filepath.Join(chatsRoot, ws.Name())
		sessions, err := os.ReadDir(wsDir)
		if err != nil {
			continue
		}
		for _, sess := range sessions {
			if !sess.IsDir() {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(wsDir, sess.Name(), "meta.json"))
			if err != nil {
				continue
			}
			var meta struct {
				Title string `json:"title"`
			}
			if JSONUnmarshal(raw, &meta) != nil {
				continue
			}
			if title := strings.TrimSpace(meta.Title); title != "" {
				out[sess.Name()] = title
			}
		}
	}
	cursorChatTitleCache.Store(chatsRoot, out)
	return out
}
