package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	lua "github.com/yuin/gopher-lua"
)

// luaFileTimeout bounds how long a plugin may spend on a single file, so a
// runaway or malicious transform can't hang indexing. Generous — parsing one
// file is fast; this only catches pathological plugins. Var for tests.
var luaFileTimeout = 60 * time.Second

// luaAdapter runs a sandboxed Lua plugin as an Adapter. recall owns all I/O —
// walking files, reading bytes, checkpointing, batching — and the plugin owns
// only the transform from raw bytes to normalized Session/Message records. The
// Lua VM is built with no os/io/network libraries, so a plugin is a pure,
// deterministic function: it cannot touch the filesystem, shell out, or reach
// the wire. See docs/lua-transform-plugins.md.
type luaAdapter struct {
	path string // the .lua file
	man  luaManifest
	// batchSessions caps sessions per ingest batch (0 = default). Test-only.
	batchSessions int
}

// luaManifest is the table a plugin returns: where its data lives and how to
// resume a session, plus which transform functions it exposes.
type luaManifest struct {
	id     string
	kind   string // "line" | "file" | "kv"
	roots  []string
	glob   string // line/file kinds: matched against each file's base name
	resume string // OpenURL template, {id} substituted

	// kv-kind fields. The host opens `source` (sqlite, read-only), scans rows
	// whose key starts with `prefix` as session headers, and for each header
	// runs a range scan whose key range comes from `related` (with {id}
	// substituted). The plugin transforms (header_value, related_rows) into
	// Session + Messages via session(id, header_value, related_rows, st).
	source  string // single sqlite file path (~ expanded)
	table   string // sqlite table name (sanitized identifier)
	prefix  string // header key prefix; id = key[len(prefix):]
	related string // optional related-rows range template, {id} substituted

	// watermark names an INTEGER column on `table` whose values increase as
	// rows are inserted/updated (rowid is the canonical example). When set,
	// the kv scanner stores MAX(watermark) as the resume checkpoint and on
	// the next pass re-emits only sessions whose header or related rows have
	// advanced past that value. Empty (default) = full rescan every pass.
	watermark string
}

func (a *luaAdapter) ID() string { return a.man.id }

func (a *luaAdapter) Available() bool {
	if a.man.kind == "kv" {
		_, err := os.Stat(a.expandKVSource())
		return err == nil
	}
	for _, r := range a.expandRoots() {
		if _, err := os.Stat(r); err == nil {
			return true
		}
	}
	return false
}

func (a *luaAdapter) OpenURL(sourceID string) string {
	return strings.ReplaceAll(a.man.resume, "{id}", sourceID)
}

func (a *luaAdapter) Scan(ctx context.Context, prev string) ([]Session, []Message, string, error) {
	return collectScan(ctx, a, prev)
}

// expandRoots resolves ~ and any trailing glob (e.g. "~/.gemini/tmp/*") into
// concrete directories to walk.
func (a *luaAdapter) expandRoots() []string {
	home, _ := os.UserHomeDir()
	var out []string
	for _, r := range a.man.roots {
		if r == "~" {
			r = home
		} else if strings.HasPrefix(r, "~/") {
			r = filepath.Join(home, r[2:])
		}
		if strings.ContainsAny(r, "*?[") {
			if matches, err := filepath.Glob(r); err == nil && len(matches) > 0 {
				out = append(out, matches...)
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

// expandKVSource resolves ~ in the kv-source path. Single file, no glob.
func (a *luaAdapter) expandKVSource() string {
	s := a.man.source
	home, _ := os.UserHomeDir()
	if s == "~" {
		return home
	}
	if strings.HasPrefix(s, "~/") {
		return filepath.Join(home, s[2:])
	}
	return s
}

// walkFiles invokes fn for every file under the adapter's roots whose base name
// matches the plugin's glob. Skips heavy/irrelevant directories so a root like
// "~" doesn't drag in .git or node_modules.
func (a *luaAdapter) walkFiles(ctx context.Context, fn func(path string, d fs.DirEntry) error) error {
	for _, root := range a.expandRoots() {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if cErr := ctx.Err(); cErr != nil {
				return cErr
			}
			if err != nil {
				return nil
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", "vendor":
					return filepath.SkipDir
				}
				return nil
			}
			ok, _ := filepath.Match(a.man.glob, d.Name())
			if !ok {
				return nil
			}
			return fn(path, d)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Recover so a panic in plugin code becomes an error instead of crashing the
// host; the built-in Go adapters are trusted and intentionally have no guard.
func (a *luaAdapter) ScanStream(ctx context.Context, prev string, emit EmitFunc) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("plugin %s panicked: %v", a.man.id, r)
		}
	}()
	if a.man.kind == "kv" {
		return a.kvScanStream(ctx, prev, emit)
	}
	L, mod, err := a.newPlugin(ctx)
	if err != nil {
		return err
	}
	defer L.Close()

	prevMap := parseFileCkpt(prev)
	nextMap := map[string]fileState{}
	be := newBatchEmitter(emit, func() string { return encodeFileCkpt(nextMap) }, a.batchSessions)

	err = a.walkFiles(ctx, func(path string, d fs.DirEntry) error {
		st, sErr := d.Info()
		if sErr != nil {
			return nil
		}
		size, mtime := st.Size(), st.ModTime().UnixNano()
		prevSt, ok := prevMap[path]
		if ok && prevSt.Size == size && prevSt.MTime == mtime {
			nextMap[path] = prevSt // unchanged — keep the watermark, skip work
			return nil
		}

		// Bound each file: a runaway or DoS plugin can't hang indexing. On
		// timeout the transform returns an error, the file is skipped, and the
		// walk continues. Derived from the scan ctx, so Ctrl+C still aborts.
		fileCtx, cancel := context.WithTimeout(ctx, luaFileTimeout)
		L.SetContext(fileCtx)
		var e error
		switch a.man.kind {
		case "line":
			e = a.streamLineFile(L, mod, path, size, mtime, prevSt, ok, nextMap, be)
		default: // "file"
			e = a.streamWholeFile(L, mod, path, size, mtime, nextMap, be)
		}
		cancel()
		L.SetContext(ctx)
		return e
	})
	if err != nil {
		return err
	}
	return be.flush()
}

// streamWholeFile re-reads a changed file in full and hands its bytes to the
// plugin's file() transform. Whole-file plugins don't offset-resume.
func (a *luaAdapter) streamWholeFile(L *lua.LState, mod *lua.LTable, path string, size, mtime int64, nextMap map[string]fileState, be *batchEmitter) error {
	nextMap[path] = fileState{Size: size, MTime: mtime}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	sess, msgs, err := a.callFile(L, mod, path, data, mtime/1e6, true)
	if err != nil || sess.SourceID == "" || len(msgs) == 0 {
		return nil
	}
	return be.add(sess, msgs)
}

// streamLineFile parses a JSONL file line-by-line via the plugin's line()
// transform, resuming from the last byte offset when the file only grew.
func (a *luaAdapter) streamLineFile(L *lua.LState, mod *lua.LTable, path string, size, mtime int64, prevSt fileState, hadPrev bool, nextMap map[string]fileState, be *batchEmitter) error {
	if hadPrev && prevSt.SID != "" && size > prevSt.Size && prevSt.Offset <= size {
		p, err := a.callLine(L, mod, path, prevSt.Offset, prevSt.Idx, prevSt.SID, mtime/1e6, true)
		if err == nil && len(p.msgs) > 0 {
			nextMap[path] = fileState{Size: size, MTime: mtime, Offset: p.endOffset, Idx: p.nextIdx, SID: prevSt.SID}
			s := p.session(a.man.id, prevSt.SID)
			s.Append = true
			s.StartedAt, s.Project, s.Title = 0, "", ""
			return be.add(s, p.msgs)
		}
	}

	p, err := a.callLine(L, mod, path, 0, 0, "", mtime/1e6, true)
	nextMap[path] = fileState{Size: size, MTime: mtime, Offset: p.endOffset, Idx: p.nextIdx, SID: p.sessID}
	if err != nil || p.sessID == "" || len(p.msgs) == 0 {
		return nil
	}
	return be.add(p.session(a.man.id, p.sessID), p.msgs)
}

// session assembles the Session a line-kind parse produced, carrying over the
// usage the plugin reported (or the char count that lets ingest estimate it).
func (p luaLineParse) session(source, sourceID string) Session {
	title := p.title
	if title == "" {
		title = titleFromPrompt(p.firstUser)
	}
	s := p.usage // Model/Tokens/Cache/Cost/Chars
	s.Source, s.SourceID = source, sourceID
	s.Project, s.Title = p.project, title
	s.StartedAt, s.EndedAt = p.startedAt, p.endedAt
	s.MsgCount = len(p.msgs)
	return s
}

func (a *luaAdapter) Fetch(ctx context.Context, sourceID string) (msgs []Message, err error) {
	defer func() {
		if r := recover(); r != nil {
			msgs, err = nil, fmt.Errorf("plugin %s panicked: %v", a.man.id, r)
		}
	}()
	if a.man.kind == "kv" {
		return a.kvFetch(ctx, sourceID)
	}
	L, mod, err := a.newPlugin(ctx)
	if err != nil {
		return nil, err
	}
	defer L.Close()

	var found []Message
	err = a.walkFiles(ctx, func(path string, d fs.DirEntry) error {
		if found != nil {
			return fs.SkipAll
		}
		switch a.man.kind {
		case "line":
			p, e := a.callLine(L, mod, path, 0, 0, "", 0, false)
			if e == nil && p.sessID == sourceID {
				found = p.msgs
				return fs.SkipAll
			}
		default:
			data, e := os.ReadFile(path)
			if e != nil {
				return nil
			}
			sess, msgs, e := a.callFile(L, mod, path, data, 0, false)
			if e == nil && sess.SourceID == sourceID {
				found = msgs
				return fs.SkipAll
			}
		}
		return nil
	})
	if err != nil && err != fs.SkipAll {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("%s session %s not found", a.man.id, sourceID)
	}
	return found, nil
}

// --- plugin invocation -----------------------------------------------------

type luaLineParse struct {
	sessID, project, title string
	firstUser              string
	startedAt, endedAt     int64
	msgs                   []Message
	endOffset              int64
	nextIdx                int
	usage                  Session // only the usage fields are read off this
}

// callFile invokes the plugin's file(doc) -> session, messages.
func (a *luaAdapter) callFile(L *lua.LState, mod *lua.LTable, path string, data []byte, mtimeMs int64, truncate bool) (Session, []Message, error) {
	fn, ok := mod.RawGetString("file").(*lua.LFunction)
	if !ok {
		return Session{}, nil, fmt.Errorf("plugin %s: no file() function", a.man.id)
	}
	doc := L.NewTable()
	doc.RawSetString("text", lua.LString(data))
	doc.RawSetString("path", lua.LString(path))
	doc.RawSetString("dir", lua.LString(filepath.Dir(path)))
	doc.RawSetString("name", lua.LString(filepath.Base(path)))
	doc.RawSetString("basename", lua.LString(stem(filepath.Base(path))))
	doc.RawSetString("mtime", lua.LNumber(mtimeMs))

	if err := L.CallByParam(lua.P{Fn: fn, NRet: 2, Protect: true}, doc); err != nil {
		return Session{}, nil, err
	}
	sv := L.Get(-2)
	mv := L.Get(-1)
	L.Pop(2)

	st, ok := sv.(*lua.LTable)
	if !ok {
		return Session{}, nil, nil
	}
	sess := Session{
		Source:    a.man.id,
		SourceID:  lvStr(st.RawGetString("id")),
		Project:   lvStr(st.RawGetString("project")),
		Title:     lvStr(st.RawGetString("title")),
		StartedAt: lvInt(st.RawGetString("started_at")),
		EndedAt:   lvInt(st.RawGetString("ended_at")),
	}
	readUsage(st, &sess)
	msgs := a.tableToMessages(mv, sess.SourceID, truncate, &sess.Chars)
	return deriveSessionMeta(sess, msgs), msgs, nil
}

// readUsage pulls the optional usage fields a plugin may set on its session
// table. All are optional: a plugin that knows nothing about tokens simply
// leaves them unset and ingest falls back to the chars/4-style estimate.
func readUsage(t *lua.LTable, sess *Session) {
	sess.Model = lvStr(t.RawGetString("model"))
	sess.TokensIn = lvInt(t.RawGetString("tokens_in"))
	sess.TokensOut = lvInt(t.RawGetString("tokens_out"))
	sess.CacheRead = lvInt(t.RawGetString("cache_read"))
	sess.CacheWrite = lvInt(t.RawGetString("cache_write"))
	if v, ok := t.RawGetString("cost_usd").(lua.LNumber); ok {
		sess.CostUSD = float64(v)
	}
}

// deriveSessionMeta fills the fields a plugin can omit: when Title or StartedAt
// is unset, take the title from the first non-wrapper user prompt and
// started/ended from the min/max message timestamp. MsgCount is always set from
// the messages. Shared by every kind so sessions look identical in the index.
func deriveSessionMeta(sess Session, msgs []Message) Session {
	if sess.Title == "" || sess.StartedAt == 0 {
		var firstUser string
		for _, m := range msgs {
			if m.Role == "user" && firstUser == "" && !looksLikeWrapper(m.Text) {
				firstUser = m.Text
			}
			if sess.StartedAt == 0 || (m.TS > 0 && m.TS < sess.StartedAt) {
				sess.StartedAt = m.TS
			}
			if m.TS > sess.EndedAt {
				sess.EndedAt = m.TS
			}
		}
		if sess.Title == "" {
			sess.Title = titleFromPrompt(firstUser)
		}
	}
	sess.MsgCount = len(msgs)
	return sess
}

// callLine invokes the plugin's line(line, state) -> msg|nil for each line,
// carrying a per-file state table so the plugin can stash the session id/cwd it
// learns from a meta line.
func (a *luaAdapter) callLine(L *lua.LState, mod *lua.LTable, path string, startOffset int64, startIdx int, knownSID string, mtimeMs int64, truncate bool) (luaLineParse, error) {
	res := luaLineParse{sessID: knownSID, nextIdx: startIdx}
	fn, ok := mod.RawGetString("line").(*lua.LFunction)
	if !ok {
		return res, fmt.Errorf("plugin %s: no line() function", a.man.id)
	}
	fh, err := os.Open(path)
	if err != nil {
		return res, err
	}
	defer fh.Close()
	if startOffset > 0 {
		if _, err := fh.Seek(startOffset, 0); err != nil {
			return res, err
		}
	}

	state := L.NewTable()
	state.RawSetString("path", lua.LString(path))
	state.RawSetString("name", lua.LString(filepath.Base(path)))
	state.RawSetString("basename", lua.LString(stem(filepath.Base(path))))
	state.RawSetString("dir", lua.LString(filepath.Dir(path)))
	state.RawSetString("mtime", lua.LNumber(mtimeMs))
	if knownSID != "" {
		state.RawSetString("id", lua.LString(knownSID))
	}
	idx := startIdx
	consumed, err := scanLines(ctx2(L), fh, func(line []byte) error {
		if err := L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}, lua.LString(line), state); err != nil {
			return err
		}
		rv := L.Get(-1)
		L.Pop(1)
		mt, ok := rv.(*lua.LTable)
		if !ok {
			return nil
		}
		text := lvStr(mt.RawGetString("text"))
		if text == "" {
			return nil
		}
		role := normRole(lvStr(mt.RawGetString("role")))
		ts := lvInt(mt.RawGetString("ts"))
		if ts > 0 {
			if res.startedAt == 0 || ts < res.startedAt {
				res.startedAt = ts
			}
			if ts > res.endedAt {
				res.endedAt = ts
			}
		}
		if role == "user" && res.firstUser == "" && !looksLikeWrapper(text) {
			res.firstUser = text
		}
		res.usage.Chars += int64(len(text))
		if truncate && len(text) > indexTextMax {
			text = text[:indexTextMax]
		}
		res.msgs = append(res.msgs, Message{Idx: idx, Role: role, TS: ts, Text: text})
		idx++
		return nil
	})
	res.endOffset = startOffset + consumed
	res.nextIdx = idx

	// Pull session-level fields the plugin stashed in state.
	res.sessID = firstNonEmpty(lvStr(state.RawGetString("id")), res.sessID)
	res.project = lvStr(state.RawGetString("project"))
	res.title = lvStr(state.RawGetString("title"))
	if t := lvInt(state.RawGetString("started_at")); t > 0 && (res.startedAt == 0 || t < res.startedAt) {
		res.startedAt = t
	}
	if t := lvInt(state.RawGetString("ended_at")); t > res.endedAt {
		res.endedAt = t
	}
	chars := res.usage.Chars
	readUsage(state, &res.usage)
	res.usage.Chars = chars
	for i := range res.msgs {
		res.msgs[i].SourceID = res.sessID
	}
	return res, err
}

func (a *luaAdapter) tableToMessages(v lua.LValue, sid string, truncate bool, chars *int64) []Message {
	t, ok := v.(*lua.LTable)
	if !ok {
		return nil
	}
	var out []Message
	n := t.Len()
	for i := 1; i <= n; i++ {
		mt, ok := t.RawGetInt(i).(*lua.LTable)
		if !ok {
			continue
		}
		text := lvStr(mt.RawGetString("text"))
		if text == "" {
			continue
		}
		if chars != nil {
			*chars += int64(len(text))
		}
		if truncate && len(text) > indexTextMax {
			text = text[:indexTextMax]
		}
		idx := len(out)
		if iv := lvInt(mt.RawGetString("idx")); iv > 0 || lvHas(mt, "idx") {
			idx = int(iv)
		}
		out = append(out, Message{
			SourceID: sid,
			Idx:      idx,
			Role:     normRole(lvStr(mt.RawGetString("role"))),
			TS:       lvInt(mt.RawGetString("ts")),
			Text:     text,
		})
	}
	return out
}

// --- the sandbox + helpers -------------------------------------------------

// newPlugin builds a fresh sandboxed VM and loads the plugin's module table.
func (a *luaAdapter) newPlugin(ctx context.Context) (*lua.LState, *lua.LTable, error) {
	L := newLuaState(ctx)
	mod, err := loadModule(L, a.path)
	if err != nil {
		L.Close()
		return nil, nil, err
	}
	return L, mod, nil
}

// newLuaState returns a Lua VM with only string/table/math (and base, minus its
// file/code-loading globals). No os, no io, no package/require, no networking —
// the sandbox is what we choose to expose, and we expose no I/O. A recall.*
// helper table is injected for the parsing primitives plugins actually need.
func newLuaState(ctx context.Context) *lua.LState {
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	for _, lib := range []struct {
		name string
		fn   lua.LGFunction
	}{
		{lua.BaseLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
		{lua.StringLibName, lua.OpenString},
		{lua.MathLibName, lua.OpenMath},
	} {
		L.Push(L.NewFunction(lib.fn))
		L.Push(lua.LString(lib.name))
		L.Call(1, 0)
	}
	// Drop the base-lib globals that can load code or escape the sandbox.
	// gopher-lua's base lib bundles require/module and the fenv/proxy escape
	// hatches; none are wanted in a pure transform.
	for _, g := range []string{
		"dofile", "loadfile", "load", "loadstring", "collectgarbage",
		"require", "module", "newproxy", "getfenv", "setfenv",
	} {
		L.SetGlobal(g, lua.LNil)
	}
	if ctx != nil {
		L.SetContext(ctx)
	}
	registerRecallLib(L)
	return L
}

// ctx2 recovers the context set on the state (scanLines wants a context). The
// state always has one set in newLuaState; fall back to Background defensively.
func ctx2(L *lua.LState) context.Context {
	if c := L.Context(); c != nil {
		return c
	}
	return context.Background()
}

func loadModule(L *lua.LState, path string) (*lua.LTable, error) {
	if err := L.DoFile(path); err != nil {
		return nil, err
	}
	mod, ok := L.Get(-1).(*lua.LTable)
	L.Pop(1)
	if !ok {
		return nil, fmt.Errorf("plugin %s must return a table", path)
	}
	return mod, nil
}

// registerRecallLib injects the recall.* helpers. They are pure (no I/O): they
// only transform values the host already handed the plugin.
func registerRecallLib(L *lua.LState) {
	t := L.NewTable()
	L.SetFuncs(t, map[string]lua.LGFunction{
		"get":      luaGet,
		"json":     luaJSON,
		"lines":    luaLines,
		"time":     luaTime,
		"truncate": luaTruncate,
	})
	L.SetGlobal("recall", t)
}

// recall.get(jsonbytes, "a.b[0].c") -> scalar|table|nil. Pulls one value by
// path using sonic's AST, so the heavy decode stays in Go and big blobs aren't
// marshaled into Lua wholesale.
func luaGet(L *lua.LState) int {
	src := L.CheckString(1)
	node, err := sonic.Get([]byte(src), parseJSONPath(L.CheckString(2))...)
	if err != nil {
		L.Push(lua.LNil)
		return 1
	}
	iv, err := node.Interface()
	if err != nil {
		L.Push(lua.LNil)
		return 1
	}
	L.Push(goToLua(L, iv))
	return 1
}

// recall.json(s) -> decoded value (full decode escape hatch).
func luaJSON(L *lua.LState) int {
	var v any
	if err := sonic.Unmarshal([]byte(L.CheckString(1)), &v); err != nil {
		L.Push(lua.LNil)
		return 1
	}
	L.Push(goToLua(L, v))
	return 1
}

// recall.lines(s) -> array of lines (no trailing empty line).
func luaLines(L *lua.LState) int {
	s := L.CheckString(1)
	t := L.NewTable()
	i := 0
	for _, ln := range strings.Split(s, "\n") {
		i++
		t.RawSetInt(i, lua.LString(strings.TrimSuffix(ln, "\r")))
	}
	// drop a trailing empty element from a final newline
	if i > 0 && lvStr(t.RawGetInt(i)) == "" {
		t.RawSetInt(i, lua.LNil)
	}
	L.Push(t)
	return 1
}

// recall.time(s [, fmt]) -> unix ms. fmt: "rfc3339"(default)|"unix_ms"|"unix_s"
// |a Go time layout.
func luaTime(L *lua.LState) int {
	s := L.CheckString(1)
	fm := L.OptString(2, "rfc3339")
	var ms int64
	switch fm {
	case "rfc3339", "":
		ms = parseClaudeTime(s)
	case "unix_ms":
		ms, _ = strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	case "unix_s":
		if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			ms = n * 1000
		}
	default:
		if tm, err := time.Parse(fm, s); err == nil {
			ms = tm.UnixMilli()
		}
	}
	L.Push(lua.LNumber(ms))
	return 1
}

// recall.truncate(s, n) -> s capped to n bytes.
func luaTruncate(L *lua.LState) int {
	s := L.CheckString(1)
	n := L.CheckInt(2)
	if n >= 0 && len(s) > n {
		s = s[:n]
	}
	L.Push(lua.LString(s))
	return 1
}

// --- value conversion + small utils ----------------------------------------

func goToLua(L *lua.LState, v any) lua.LValue {
	switch x := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(x)
	case float64:
		return lua.LNumber(x)
	case int64:
		return lua.LNumber(x)
	case int:
		return lua.LNumber(x)
	case string:
		return lua.LString(x)
	case []any:
		t := L.NewTable()
		for i, e := range x {
			t.RawSetInt(i+1, goToLua(L, e))
		}
		return t
	case map[string]any:
		t := L.NewTable()
		for k, e := range x {
			t.RawSetString(k, goToLua(L, e))
		}
		return t
	default:
		return lua.LString(fmt.Sprintf("%v", x))
	}
}

func lvStr(v lua.LValue) string {
	if s, ok := v.(lua.LString); ok {
		return string(s)
	}
	if n, ok := v.(lua.LNumber); ok {
		return strconv.FormatFloat(float64(n), 'f', -1, 64)
	}
	return ""
}

func lvInt(v lua.LValue) int64 {
	if n, ok := v.(lua.LNumber); ok {
		return int64(n)
	}
	if s, ok := v.(lua.LString); ok {
		n, _ := strconv.ParseInt(string(s), 10, 64)
		return n
	}
	return 0
}

func lvHas(t *lua.LTable, key string) bool {
	return t.RawGetString(key) != lua.LNil
}

func normRole(r string) string {
	switch r {
	case "user", "assistant", "tool":
		return r
	case "human":
		return "user"
	case "ai", "model", "gemini", "bot":
		return "assistant"
	case "":
		return "assistant"
	default:
		return r
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func stem(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// parseJSONPath turns "payload.content[0].text" into ["payload","content",0,"text"]
// for sonic.Get (string keys, int indices).
func parseJSONPath(p string) []any {
	var segs []any
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			segs = append(segs, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(p); {
		switch p[i] {
		case '.':
			flush()
			i++
		case '[':
			flush()
			j := strings.IndexByte(p[i:], ']')
			if j < 0 {
				return segs
			}
			tok := p[i+1 : i+j]
			if n, err := strconv.Atoi(tok); err == nil {
				segs = append(segs, n)
			} else {
				segs = append(segs, strings.Trim(tok, `"'`))
			}
			i += j + 1
		default:
			cur.WriteByte(p[i])
			i++
		}
	}
	flush()
	return segs
}

// --- discovery -------------------------------------------------------------

// discoverLuaAdapters loads every *.lua plugin in dir as an adapter. Malformed
// plugins are skipped with a stderr note rather than failing the whole run.
func discoverLuaAdapters(dir string) []Adapter {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.lua"))
	var out []Adapter
	for _, p := range matches {
		man, err := readLuaManifest(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "recall: skipping plugin %s: %v\n", filepath.Base(p), err)
			continue
		}
		out = append(out, &luaAdapter{path: p, man: man})
	}
	return out
}

// readLuaManifest runs the plugin in a throwaway sandbox just to read its
// declared metadata (id, kind, roots, glob, resume).
func readLuaManifest(path string) (luaManifest, error) {
	L := newLuaState(context.Background())
	defer L.Close()
	mod, err := loadModule(L, path)
	if err != nil {
		return luaManifest{}, err
	}
	man := luaManifest{
		id:        lvStr(mod.RawGetString("id")),
		kind:      lvStr(mod.RawGetString("kind")),
		glob:      lvStr(mod.RawGetString("glob")),
		resume:    lvStr(mod.RawGetString("resume")),
		source:    lvStr(mod.RawGetString("source")),
		table:     lvStr(mod.RawGetString("table")),
		prefix:    lvStr(mod.RawGetString("prefix")),
		related:   lvStr(mod.RawGetString("related")),
		watermark: lvStr(mod.RawGetString("watermark")),
	}
	if rt, ok := mod.RawGetString("roots").(*lua.LTable); ok {
		for i := 1; i <= rt.Len(); i++ {
			if s := lvStr(rt.RawGetInt(i)); s != "" {
				man.roots = append(man.roots, s)
			}
		}
	}
	if man.id == "" {
		return man, fmt.Errorf("missing id")
	}
	switch man.kind {
	case "line", "file":
		if man.glob == "" {
			return man, fmt.Errorf("plugin %s: missing glob", man.id)
		}
	case "kv":
		if man.source == "" {
			return man, fmt.Errorf("plugin %s: kv kind requires source", man.id)
		}
		if !validSQLIdent(man.table) {
			return man, fmt.Errorf("plugin %s: kv kind requires a safe table identifier", man.id)
		}
		if man.prefix == "" {
			return man, fmt.Errorf("plugin %s: kv kind requires prefix", man.id)
		}
		if man.watermark != "" && !validSQLIdent(man.watermark) {
			return man, fmt.Errorf("plugin %s: kv watermark must be a safe column identifier", man.id)
		}
	default:
		return man, fmt.Errorf("plugin %s: kind must be \"line\", \"file\", or \"kv\"", man.id)
	}
	return man, nil
}

// --- `recall plugin` CLI: the authoring loop -------------------------------

func runPlugin(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: recall plugin <list|install|test> ...")
	}
	switch args[0] {
	case "list":
		return runPluginList()
	case "install":
		return runPluginInstall(args[1:])
	case "test":
		return runPluginTest(args[1:])
	default:
		return fmt.Errorf("unknown plugin subcommand %q (want: list, install, test)", args[0])
	}
}

func runPluginList() error {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".recall", "plugins")
	installed := map[string]bool{}
	for _, a := range discoverLuaAdapters(dir) {
		la := a.(*luaAdapter)
		installed[la.man.id] = true
		mark := "✗"
		if la.Available() {
			mark = "✓"
		}
		paths := la.expandRoots()
		if la.man.kind == "kv" {
			paths = []string{la.expandKVSource()}
		}
		fmt.Printf("installed  %s %-12s %-5s %s\n", mark, la.man.id, la.man.kind, strings.Join(paths, ", "))
	}

	// Bundled plugins not yet installed: offer them so users know they exist.
	var available []string
	for _, name := range embeddedPluginNames() {
		if !installed[name] {
			available = append(available, name)
		}
	}
	if len(available) > 0 {
		fmt.Printf("available  %s  (recall plugin install <name>)\n", strings.Join(available, ", "))
	}
	if len(installed) == 0 && len(available) == 0 {
		fmt.Printf("no plugins (bundled or in %s)\n", dir)
	}
	return nil
}

// runPluginInstall copies a bundled plugin into ~/.recall/plugins. Refuses to
// clobber an existing install (which may be user-edited) without --force.
func runPluginInstall(args []string) error {
	var force bool
	var names []string
	for _, a := range args {
		switch a {
		case "--force", "-f":
			force = true
		default:
			names = append(names, a)
		}
	}
	if len(names) == 0 {
		return fmt.Errorf("usage: recall plugin install <name>... [--force]  (bundled: %s)",
			strings.Join(embeddedPluginNames(), ", "))
	}
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".recall", "plugins")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range names {
		data, err := readEmbeddedPlugin(name)
		if err != nil {
			return fmt.Errorf("no bundled plugin %q (bundled: %s)", name,
				strings.Join(embeddedPluginNames(), ", "))
		}
		dst := filepath.Join(dir, name+".lua")
		if _, err := os.Stat(dst); err == nil && !force {
			return fmt.Errorf("%s already installed at %s (use --force to overwrite)", name, dst)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
		fmt.Printf("installed %s → %s\n", name, dst)
	}
	return nil
}

// runPluginTest dry-runs a plugin so authors see exactly what records it
// produces. With a sample file it parses just that file (bypassing roots/glob);
// otherwise it scans the plugin's declared roots.
func runPluginTest(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: recall plugin test <plugin.lua> [sample-file]")
	}
	path := args[0]
	man, err := readLuaManifest(path)
	if err != nil {
		return err
	}
	ad := &luaAdapter{path: path, man: man, batchSessions: 1}

	var sessions []Session
	var msgs []Message
	if len(args) >= 2 {
		sessions, msgs, err = ad.testSample(args[1])
	} else {
		sessions, msgs, _, err = ad.Scan(context.Background(), "")
	}
	if err != nil {
		return err
	}

	fmt.Printf("plugin %s (%s) → %d session(s), %d message(s)\n", man.id, man.kind, len(sessions), len(msgs))
	for _, s := range sessions {
		fmt.Printf("\nsession %s\n  project=%s\n  title=%s\n  msgs=%d  started_ms=%d\n",
			s.SourceID, s.Project, s.Title, s.MsgCount, s.StartedAt)
	}
	fmt.Println()
	for i, m := range msgs {
		if i >= 12 {
			fmt.Printf("  … and %d more\n", len(msgs)-12)
			break
		}
		t := m.Text
		if len(t) > 100 {
			t = t[:100]
		}
		fmt.Printf("  [%d] %-9s %s\n", m.Idx, m.Role, strings.ReplaceAll(t, "\n", " "))
	}
	return nil
}

// testSample parses one file through the plugin, regardless of roots/glob — the
// fast path for iterating on a transform.
func (a *luaAdapter) testSample(sample string) ([]Session, []Message, error) {
	L, mod, err := a.newPlugin(context.Background())
	if err != nil {
		return nil, nil, err
	}
	defer L.Close()

	if a.man.kind == "kv" {
		return nil, nil, fmt.Errorf("plugin %s: kv kind has no sample file (run `recall plugin test %s` without an argument)", a.man.id, a.path)
	}
	switch a.man.kind {
	case "line":
		var mtimeMs int64
		if fi, e := os.Stat(sample); e == nil {
			mtimeMs = fi.ModTime().UnixMilli()
		}
		p, err := a.callLine(L, mod, sample, 0, 0, "", mtimeMs, false)
		if err != nil {
			return nil, nil, err
		}
		var sessions []Session
		if p.sessID != "" {
			sessions = []Session{p.session(a.man.id, p.sessID)}
		}
		return sessions, p.msgs, nil
	default: // "file"
		data, err := os.ReadFile(sample)
		if err != nil {
			return nil, nil, err
		}
		s, m, err := a.callFile(L, mod, sample, data, 0, false)
		if err != nil {
			return nil, nil, err
		}
		return []Session{s}, m, nil
	}
}

// --- kv kind: index a sqlite KV table -------------------------------------
//
// The manifest names a sqlite file, a table, a header key prefix, and an
// optional related-key range template. The host scans header rows in key
// order, runs the related range scan per id, and hands the blobs to
// session(id, header_value, related_rows, st). Lua sees only strings.
//
// With a watermark column set, the scan resumes incrementally (see
// kvScanStream); without one it full-rescans every pass.

func (a *luaAdapter) kvScanStream(ctx context.Context, prev string, emit EmitFunc) error {
	src := a.expandKVSource()
	if _, err := os.Stat(src); err != nil {
		return nil // missing source = empty scan, like a missing root
	}
	db, err := sql.Open("sqlite", sourceSQLiteDSN(src))
	if err != nil {
		return err
	}
	defer db.Close()

	L, mod, err := a.newPlugin(ctx)
	if err != nil {
		return err
	}
	defer L.Close()

	// Advance the checkpoint to MAX(watermark) only at clean pass-end (intermediate
	// flushes keep the old value), so a mid-pass crash re-runs from the prior
	// watermark instead of skipping un-emitted sessions; the upsert absorbs the redo.
	hasWM := a.man.watermark != ""
	prevWM := parseKVCkpt(prev)
	ckptVal := prev
	if !hasWM {
		ckptVal = ""
	}
	be := newBatchEmitter(emit, func() string { return ckptVal }, a.batchSessions)

	var curMax int64
	if hasWM {
		if curMax, err = a.kvMaxWatermark(ctx, db); err != nil {
			return err
		}
	}

	process := func(id, val string) error {
		related, err := a.kvRelatedRows(ctx, db, id)
		if err != nil {
			return err
		}
		fileCtx, cancel := context.WithTimeout(ctx, luaFileTimeout)
		L.SetContext(fileCtx)
		// Don't truncate at scan: index.go caps text at write, and truncating here
		// would permanently drop characters from the stored excerpt.
		sess, msgs, e := a.callSession(L, mod, id, val, related, false)
		cancel()
		L.SetContext(ctx)
		if e != nil || sess.SourceID == "" {
			return nil
		}
		return be.add(sess, msgs)
	}

	if hasWM && prevWM > 0 {
		// Incremental: re-emit only sessions whose header or related rows
		// advanced past the last watermark.
		ids, err := a.kvTouchedIDs(ctx, db, prevWM)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			val, ok, err := a.kvHeaderValue(ctx, db, id)
			if err != nil {
				return err
			}
			if !ok {
				continue // header deleted; nothing to re-index
			}
			if err := process(id, val); err != nil {
				return err
			}
		}
	} else {
		// Full pass: every header row in key order.
		hdrLo, hdrHi := keyRange(a.man.prefix)
		rows, err := db.QueryContext(ctx,
			`SELECT key, value FROM `+a.man.table+` WHERE key >= ? AND key < ? ORDER BY key`,
			hdrLo, hdrHi)
		if err != nil {
			return err
		}
		type hdr struct{ id, val string }
		var headers []hdr
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				rows.Close()
				return err
			}
			var k string
			var v []byte
			if err := rows.Scan(&k, &v); err != nil {
				rows.Close()
				return err
			}
			headers = append(headers, hdr{id: k[len(a.man.prefix):], val: string(v)})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		for _, h := range headers {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := process(h.id, h.val); err != nil {
				return err
			}
		}
	}

	// Pass completed cleanly: advance the checkpoint so the final flush
	// persists it. (No-op for the no-watermark full-rescan mode.)
	if hasWM {
		ckptVal = encodeKVCkpt(curMax)
	}
	return be.flush()
}

// kvMaxWatermark reads MAX(watermark) from the table; 0 when the table is
// empty. Bound as int64 so SQLite compares numerically, not lexically.
func (a *luaAdapter) kvMaxWatermark(ctx context.Context, db *sql.DB) (int64, error) {
	var n sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(`+a.man.watermark+`) FROM `+a.man.table).Scan(&n); err != nil {
		return 0, err
	}
	if !n.Valid {
		return 0, nil
	}
	return n.Int64, nil
}

// kvHeaderValue fetches one header row's value by id.
func (a *luaAdapter) kvHeaderValue(ctx context.Context, db *sql.DB, id string) (string, bool, error) {
	var v []byte
	err := db.QueryRowContext(ctx,
		`SELECT value FROM `+a.man.table+` WHERE key = ?`, a.man.prefix+id).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(v), true, nil
}

// kvTouchedIDs returns the header ids whose header row or related rows have a
// watermark greater than prev, scanning the prefix/related key ranges.
func (a *luaAdapter) kvTouchedIDs(ctx context.Context, db *sql.DB, prev int64) ([]string, error) {
	hdrLo, hdrHi := keyRange(a.man.prefix)
	relPre, relSuf := a.relatedPrefixSuffix()

	var rows *sql.Rows
	var err error
	if relPre != "" {
		relLo, relHi := keyRange(relPre)
		rows, err = db.QueryContext(ctx,
			`SELECT key FROM `+a.man.table+` WHERE `+a.man.watermark+` > ? AND `+
				`((key >= ? AND key < ?) OR (key >= ? AND key < ?))`,
			prev, hdrLo, hdrHi, relLo, relHi)
	} else {
		rows, err = db.QueryContext(ctx,
			`SELECT key FROM `+a.man.table+` WHERE `+a.man.watermark+` > ? AND key >= ? AND key < ?`,
			prev, hdrLo, hdrHi)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		switch {
		case strings.HasPrefix(k, a.man.prefix):
			seen[k[len(a.man.prefix):]] = true
		case relPre != "" && strings.HasPrefix(k, relPre):
			if id, ok := idFromRelatedKey(k, relPre, relSuf); ok {
				seen[id] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out) // deterministic order
	return out, nil
}

// relatedPrefixSuffix splits the related template around {id}. For
// "bubbleId:{id}:" it returns ("bubbleId:", ":"). Empty template => empty
// prefix (no related scan).
func (a *luaAdapter) relatedPrefixSuffix() (string, string) {
	if a.man.related == "" {
		return "", ""
	}
	if i := strings.Index(a.man.related, "{id}"); i >= 0 {
		return a.man.related[:i], a.man.related[i+len("{id}"):]
	}
	return a.man.related, ""
}

// idFromRelatedKey recovers the header id embedded in a related key, given the
// template's prefix/suffix around {id}. "bubbleId:c1:b2" with ("bubbleId:",
// ":") yields "c1".
//
// The id is everything between relPre and the FIRST relSuf, so it is correct
// only when the id itself cannot contain relSuf (e.g. UUIDs vs a ":" suffix).
// An empty id (key == relPre+relSuf) is rejected.
func idFromRelatedKey(key, relPre, relSuf string) (string, bool) {
	if !strings.HasPrefix(key, relPre) {
		return "", false
	}
	rest := key[len(relPre):]
	if relSuf == "" {
		if rest == "" {
			return "", false
		}
		return rest, true
	}
	if i := strings.Index(rest, relSuf); i > 0 {
		return rest[:i], true
	}
	return "", false
}

func parseKVCkpt(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

func encodeKVCkpt(n int64) string { return strconv.FormatInt(n, 10) }

func (a *luaAdapter) kvFetch(ctx context.Context, sourceID string) ([]Message, error) {
	src := a.expandKVSource()
	db, err := sql.Open("sqlite", sourceSQLiteDSN(src))
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var hdr []byte
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM `+a.man.table+` WHERE key = ?`,
		a.man.prefix+sourceID).Scan(&hdr); err != nil {
		return nil, fmt.Errorf("%s %s: %w", a.man.id, sourceID, err)
	}
	related, err := a.kvRelatedRows(ctx, db, sourceID)
	if err != nil {
		return nil, err
	}
	L, mod, err := a.newPlugin(ctx)
	if err != nil {
		return nil, err
	}
	defer L.Close()
	_, msgs, err := a.callSession(L, mod, sourceID, string(hdr), related, false)
	return msgs, err
}

type kvRow struct{ key, value string }

// kvRelatedRows runs the configured related-range scan for one header id and
// returns rows in key order. Empty `related` template yields no
// rows — handy for pure 1-row-per-session sources.
func (a *luaAdapter) kvRelatedRows(ctx context.Context, db *sql.DB, id string) ([]kvRow, error) {
	if a.man.related == "" {
		return nil, nil
	}
	lo := strings.ReplaceAll(a.man.related, "{id}", id)
	_, hi := keyRange(lo)
	rows, err := db.QueryContext(ctx,
		`SELECT key, value FROM `+a.man.table+` WHERE key >= ? AND key < ? ORDER BY key`,
		lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []kvRow
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out = append(out, kvRow{key: k, value: string(v)})
	}
	return out, rows.Err()
}

// callSession invokes the plugin's session(id, header_value, related_rows, st)
// transform and normalizes the returned (session, messages) pair, mirroring
// callFile's title/timestamp fallback rules so kv-sourced sessions look like
// the other kinds in the index.
func (a *luaAdapter) callSession(L *lua.LState, mod *lua.LTable, id, headerVal string, related []kvRow, truncate bool) (Session, []Message, error) {
	fn, ok := mod.RawGetString("session").(*lua.LFunction)
	if !ok {
		return Session{}, nil, fmt.Errorf("plugin %s: no session() function", a.man.id)
	}
	rt := L.NewTable()
	for i, r := range related {
		row := L.NewTable()
		row.RawSetString("key", lua.LString(r.key))
		row.RawSetString("value", lua.LString(r.value))
		rt.RawSetInt(i+1, row)
	}
	st := L.NewTable()
	st.RawSetString("id", lua.LString(id))

	if err := L.CallByParam(lua.P{Fn: fn, NRet: 2, Protect: true},
		lua.LString(id), lua.LString(headerVal), rt, st); err != nil {
		return Session{}, nil, err
	}
	sv := L.Get(-2)
	mv := L.Get(-1)
	L.Pop(2)

	sessT, ok := sv.(*lua.LTable)
	if !ok {
		return Session{}, nil, nil
	}
	sess := Session{
		Source:    a.man.id,
		SourceID:  firstNonEmpty(lvStr(sessT.RawGetString("id")), id),
		Project:   lvStr(sessT.RawGetString("project")),
		Title:     lvStr(sessT.RawGetString("title")),
		StartedAt: lvInt(sessT.RawGetString("started_at")),
		EndedAt:   lvInt(sessT.RawGetString("ended_at")),
	}
	readUsage(sessT, &sess)
	msgs := a.tableToMessages(mv, sess.SourceID, truncate, &sess.Chars)
	return deriveSessionMeta(sess, msgs), msgs, nil
}

// keyRange returns the half-open [lo, hi) byte range that matches all keys
// starting with `prefix`. hi is prefix with the last byte incremented; for an
// empty prefix we use a high sentinel that sorts after any reasonable key.
func keyRange(prefix string) (string, string) {
	if prefix == "" {
		return "", "\uffff"
	}
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return prefix, string(b[:i+1])
		}
	}
	return prefix, prefix + "\uffff"
}

// validSQLIdent guards the manifest-supplied table name. The manifest is
// trusted code (it IS the plugin), but the table name flows into a string-
// concatenated SELECT — restrict it to identifier chars so a careless plugin
// can't accidentally write SQL through it.
func validSQLIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_':
		default:
			return false
		}
	}
	return true
}
