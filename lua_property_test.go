package main

// Property tests for the kv kind. Hermetic, fast, and biased toward the
// shapes that caused real bugs during cursor.lua bring-up: arbitrary header
// orderings, headers referencing ghost bubbles, all-empty-text composers,
// role-type variety, multi-line and unicode text. The fixture test pins one
// concrete example; this fills the input space around it.
//
// Stress mode: RECALL_PROP_ITER=500 go test -run TestCursorLuaParityProperty -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// --- TestKeyRangeProperty -------------------------------------------------
//
// Invariant: for any non-empty prefix p, keyRange(p) returns [lo, hi) such
// that every string starting with p falls inside and every other string
// falls outside. This is the predicate the kv SELECT relies on; the wrong
// upper bound would either miss rows (false negative) or pull adjacent
// prefixes (false positive).

func TestKeyRangeProperty(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	const cases = 300
	for i := 0; i < cases; i++ {
		p := randPrintable(r, 1, 12)
		lo, hi := keyRange(p)
		if lo != p {
			t.Fatalf("keyRange(%q): lo = %q, want %q", p, lo, p)
		}
		if !(hi > p) {
			t.Fatalf("keyRange(%q): hi = %q must be > lo", p, hi)
		}

		// Strings that DO start with p must lie in [lo, hi).
		for j := 0; j < 6; j++ {
			s := p + randPrintable(r, 0, 8)
			if !(s >= lo && s < hi) {
				t.Fatalf("keyRange(%q) drops a matching key %q (range [%q,%q))",
					p, s, lo, hi)
			}
		}
		// Strings that do NOT start with p must lie outside [lo, hi).
		for j := 0; j < 6; j++ {
			s := randPrintable(r, 1, 16)
			if strings.HasPrefix(s, p) {
				continue
			}
			if s >= lo && s < hi {
				t.Fatalf("keyRange(%q) pulls a non-matching key %q (range [%q,%q))",
					p, s, lo, hi)
			}
		}
	}
}

// --- TestCursorLuaParityProperty -----------------------------------------
//
// Generates randomized (composer, bubbles, headers) layouts straight into a
// throwaway sqlite, then scans through both the Go CursorAdapter and the Lua
// cursor.lua plugin. Asserts every emitted Session and Message is bit-for-bit
// identical (modulo Project, which v1 cursor.lua intentionally doesn't model).
//
// The generator targets the things that already broke parity once:
//   • headers in shuffled / partial / empty / ghost-id form (ordering edge)
//   • bubble texts spanning empty / short / multiline / large / unicode
//   • bubble types covering user (1), assistant (2), and tool (other)
//   • composers with zero bubbles and composers with only-empty-text bubbles
//     (both must be dropped — `if len(ordered) == 0 { continue }`)

func TestCursorLuaParityProperty(t *testing.T) {
	iter := 60
	if v := os.Getenv("RECALL_PROP_ITER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			iter = n
		}
	}
	for i := 0; i < iter; i++ {
		seed := int64(0xC0DE0000) + int64(i)
		t.Run(fmt.Sprintf("seed-%x", seed), func(t *testing.T) {
			r := rand.New(rand.NewSource(seed))
			comps := genComposers(r, 0, 5)
			userDir := t.TempDir()
			insertPropComposers(t, userDir, comps)

			goAd := &CursorAdapter{UserDir: userDir}
			luaAd := luaAdapterForKV(t, "plugins/cursor.lua",
				filepath.Join(userDir, "globalStorage", "state.vscdb"))

			gs, gm, _, err := goAd.Scan(context.Background(), "")
			if err != nil {
				t.Fatalf("go scan: %v", err)
			}
			ls, lm, _, err := luaAd.Scan(context.Background(), "")
			if err != nil {
				t.Fatalf("lua scan: %v", err)
			}

			if diff := diffSessions(gs, ls); diff != "" {
				t.Fatalf("session divergence:\n%s\ninput: %s", diff, summarize(comps))
			}
			if diff := diffMessages(gm, lm); diff != "" {
				t.Fatalf("message divergence:\n%s\ninput: %s", diff, summarize(comps))
			}

			// Beyond parity: both sides must hold the contract invariants.
			assertContract(t, gs, gm, comps)
			assertContract(t, ls, lm, comps)
		})
	}
}

// --- contract invariants both adapters must satisfy ----------------------

func assertContract(t *testing.T, sess []Session, msgs []Message, in []propComposer) {
	t.Helper()
	// (1) Every session id is a real composer id from the input.
	knownCID := map[string]bool{}
	for _, c := range in {
		knownCID[c.id] = true
	}
	for _, s := range sess {
		if !knownCID[s.SourceID] {
			t.Errorf("session %q not in input composers", s.SourceID)
		}
		if s.Source != "cursor" {
			t.Errorf("session %q: source = %q (want cursor)", s.SourceID, s.Source)
		}
	}
	// (2) Per-session: messages have idx 0..N-1 contiguous, role in the
	//     allowed set, and SourceID matching the session.
	bySID := map[string][]Message{}
	for _, m := range msgs {
		bySID[m.SourceID] = append(bySID[m.SourceID], m)
	}
	for _, s := range sess {
		ms := bySID[s.SourceID]
		if len(ms) != s.MsgCount {
			t.Errorf("session %q: msg_count=%d but %d messages emitted",
				s.SourceID, s.MsgCount, len(ms))
		}
		sort.Slice(ms, func(i, j int) bool { return ms[i].Idx < ms[j].Idx })
		for i, m := range ms {
			if m.Idx != i {
				t.Errorf("session %q: idx gap (want %d, got %d)", s.SourceID, i, m.Idx)
			}
			switch m.Role {
			case "user", "assistant", "tool":
			default:
				t.Errorf("session %q msg %d: role = %q (want user|assistant|tool)",
					s.SourceID, m.Idx, m.Role)
			}
			if m.Text == "" {
				t.Errorf("session %q msg %d: empty text leaked through", s.SourceID, m.Idx)
			}
		}
	}
	// (3) A composer with zero non-empty bubbles must NOT appear as a session.
	emitted := map[string]bool{}
	for _, s := range sess {
		emitted[s.SourceID] = true
	}
	for _, c := range in {
		anyText := false
		for _, b := range c.bubbles {
			if b.text != "" {
				anyText = true
				break
			}
		}
		if !anyText && emitted[c.id] {
			t.Errorf("composer %q has no non-empty bubble but was emitted", c.id)
		}
	}
}

// --- diff helpers (fail-fast, compact, parity-only) ----------------------

func diffSessions(want, got []Session) string {
	if len(want) != len(got) {
		return fmt.Sprintf("count: go=%d lua=%d", len(want), len(got))
	}
	sortSessions(want)
	sortSessions(got)
	for i := range want {
		w, g := want[i], got[i]
		if w.Source != g.Source || w.SourceID != g.SourceID ||
			w.Title != g.Title || w.StartedAt != g.StartedAt ||
			w.EndedAt != g.EndedAt || w.MsgCount != g.MsgCount || w.Append != g.Append {
			return fmt.Sprintf("session[%d]:\n  go  = %+v\n  lua = %+v", i, w, g)
		}
	}
	return ""
}

func diffMessages(want, got []Message) string {
	if len(want) != len(got) {
		return fmt.Sprintf("count: go=%d lua=%d", len(want), len(got))
	}
	sortMessages(want)
	sortMessages(got)
	for i := range want {
		w, g := want[i], got[i]
		if w.SourceID != g.SourceID || w.Idx != g.Idx ||
			w.Role != g.Role || w.TS != g.TS || w.Text != g.Text {
			return fmt.Sprintf("message[%d]:\n  go  = %+v\n  lua = %+v", i, w, g)
		}
	}
	return ""
}

// --- generator ------------------------------------------------------------

type propBubble struct {
	id   string
	typ  int
	text string
}

type propComposer struct {
	id        string
	name      string
	createdAt int64
	bubbles   []propBubble // in insertion (rowid) order
	headers   []string     // fullConversationHeadersOnly, arbitrary order
}

func genComposers(r *rand.Rand, lo, hi int) []propComposer {
	n := lo + r.Intn(hi-lo+1)
	out := make([]propComposer, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, genOneComposer(r, i))
	}
	return out
}

func genOneComposer(r *rand.Rand, ord int) propComposer {
	c := propComposer{
		id:        fmt.Sprintf("c%04x-%04x", ord, r.Intn(0x10000)),
		createdAt: 1_700_000_000_000 + int64(r.Intn(1<<24))*1000,
	}
	if r.Intn(3) > 0 {
		c.name = randSentence(r, 1, 5)
	}
	// 0..6 bubbles; sometimes zero, exercising the "no session" branch.
	nb := r.Intn(7)
	for i := 0; i < nb; i++ {
		c.bubbles = append(c.bubbles, propBubble{
			id:   fmt.Sprintf("b%04x-%04x", i, r.Intn(0x10000)),
			typ:  []int{0, 1, 1, 2, 2, 3, 7}[r.Intn(7)],
			text: genBubbleText(r),
		})
	}
	// Header order: 4 shapes covering everything we've seen go wrong.
	switch r.Intn(4) {
	case 0: // in-order — common case
		for _, b := range c.bubbles {
			c.headers = append(c.headers, b.id)
		}
	case 1: // shuffled — tail-append must follow storage order, not headers
		ids := bubbleIDs(c.bubbles)
		r.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
		c.headers = ids
	case 2: // partial — some bubbles only reachable via the tail loop
		for _, b := range c.bubbles {
			if r.Intn(2) == 0 {
				c.headers = append(c.headers, b.id)
			}
		}
	case 3: // empty — everything reaches via the tail loop
	}
	// Spice with a ghost reference some of the time. Both adapters must
	// ignore it (no matching bubble row).
	if r.Intn(3) == 0 {
		c.headers = append(c.headers, fmt.Sprintf("ghost-%04x", r.Intn(0x10000)))
	}
	return c
}

func genBubbleText(r *rand.Rand) string {
	switch r.Intn(7) {
	case 0:
		return "" // filtered by both adapters
	case 1:
		return "ok"
	case 2:
		return "two\nlines\nhere"
	case 3:
		return strings.Repeat("aB ", 200)
	case 4:
		return "日本語 with emoji 🎯 and tabs\tend"
	case 5:
		return "  leading and trailing  "
	default:
		return randSentence(r, 3, 12)
	}
}

func randSentence(r *rand.Rand, lo, hi int) string {
	words := []string{
		"the", "quick", "brown", "fox", "jumps", "over", "lazy", "dog",
		"refactor", "fix", "race", "condition", "indexer", "cursor",
	}
	n := lo + r.Intn(hi-lo+1)
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = words[r.Intn(len(words))]
	}
	return strings.Join(parts, " ")
}

func randPrintable(r *rand.Rand, lo, hi int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz0123456789-_:."
	n := lo + r.Intn(hi-lo+1)
	b := make([]byte, n)
	for i := range b {
		b[i] = alpha[r.Intn(len(alpha))]
	}
	return string(b)
}

func bubbleIDs(bs []propBubble) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.id
	}
	return out
}

func summarize(comps []propComposer) string {
	var b strings.Builder
	for _, c := range comps {
		fmt.Fprintf(&b, "\n  %s name=%q bubbles=%d headers=%d", c.id, c.name, len(c.bubbles), len(c.headers))
	}
	return b.String()
}

// --- fixture writer (handles arbitrary header order, ghost ids) ----------

func insertPropComposers(t *testing.T, userDir string, comps []propComposer) {
	t.Helper()
	dir := filepath.Join(userDir, "globalStorage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// CursorAdapter scans workspaceStorage for the composer→project map. We
	// don't model projects in the property test (cursor.lua v1 doesn't either),
	// but the directory must exist or the Go scan fails.
	if err := os.MkdirAll(filepath.Join(userDir, "workspaceStorage"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.vscdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS cursorDiskKV (key TEXT PRIMARY KEY, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	for _, c := range comps {
		headers := make([]map[string]any, 0, len(c.headers))
		for _, hid := range c.headers {
			headers = append(headers, map[string]any{"bubbleId": hid})
		}
		cd, _ := json.Marshal(map[string]any{
			"composerId":                  c.id,
			"name":                        c.name,
			"createdAt":                   c.createdAt,
			"fullConversationHeadersOnly": headers,
		})
		if _, err := db.Exec(`INSERT INTO cursorDiskKV(key,value) VALUES(?,?)`,
			"composerData:"+c.id, cd); err != nil {
			t.Fatal(err)
		}
		for _, b := range c.bubbles {
			bv, _ := json.Marshal(map[string]any{"type": b.typ, "text": b.text})
			if _, err := db.Exec(`INSERT INTO cursorDiskKV(key,value) VALUES(?,?)`,
				"bubbleId:"+c.id+":"+b.id, bv); err != nil {
				t.Fatal(err)
			}
		}
	}
}
