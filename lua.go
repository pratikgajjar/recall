package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	kind   string // "line" (JSONL, offset-resumable) | "file" (whole-file)
	roots  []string
	glob   string // matched against each file's base name (filepath.Match)
	resume string // OpenURL template, {id} substituted
}

func (a *luaAdapter) ID() string { return a.man.id }

func (a *luaAdapter) Available() bool {
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

func (a *luaAdapter) ScanStream(ctx context.Context, prev string, emit EmitFunc) error {
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
		p, err := a.callLine(L, mod, path, prevSt.Offset, prevSt.Idx, prevSt.SID, true)
		if err == nil && len(p.msgs) > 0 {
			nextMap[path] = fileState{Size: size, MTime: mtime, Offset: p.endOffset, Idx: p.nextIdx, SID: prevSt.SID}
			return be.add(Session{
				Source: a.man.id, SourceID: prevSt.SID, Append: true,
				EndedAt: p.endedAt, MsgCount: len(p.msgs),
			}, p.msgs)
		}
	}

	p, err := a.callLine(L, mod, path, 0, 0, "", true)
	nextMap[path] = fileState{Size: size, MTime: mtime, Offset: p.endOffset, Idx: p.nextIdx, SID: p.sessID}
	if err != nil || p.sessID == "" || len(p.msgs) == 0 {
		return nil
	}
	title := p.title
	if title == "" {
		title = titleFromPrompt(p.firstUser)
	}
	return be.add(Session{
		Source: a.man.id, SourceID: p.sessID,
		Project: p.project, Title: title,
		StartedAt: p.startedAt, EndedAt: p.endedAt,
		MsgCount: len(p.msgs),
	}, p.msgs)
}

func (a *luaAdapter) Fetch(ctx context.Context, sourceID string) ([]Message, error) {
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
			p, e := a.callLine(L, mod, path, 0, 0, "", false)
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
	msgs := a.tableToMessages(mv, sess.SourceID, truncate)
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
	return sess, msgs, nil
}

// callLine invokes the plugin's line(line, state) -> msg|nil for each line,
// carrying a per-file state table so the plugin can stash the session id/cwd it
// learns from a meta line.
func (a *luaAdapter) callLine(L *lua.LState, mod *lua.LTable, path string, startOffset int64, startIdx int, knownSID string, truncate bool) (luaLineParse, error) {
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
		if truncate && len(text) > excerptMax {
			text = text[:excerptMax]
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
	for i := range res.msgs {
		res.msgs[i].SourceID = res.sessID
	}
	return res, err
}

func (a *luaAdapter) tableToMessages(v lua.LValue, sid string, truncate bool) []Message {
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
		if truncate && len(text) > excerptMax {
			text = text[:excerptMax]
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
		id:     lvStr(mod.RawGetString("id")),
		kind:   lvStr(mod.RawGetString("kind")),
		glob:   lvStr(mod.RawGetString("glob")),
		resume: lvStr(mod.RawGetString("resume")),
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
	if man.glob == "" {
		return man, fmt.Errorf("plugin %s: missing glob", man.id)
	}
	if man.kind != "line" && man.kind != "file" {
		return man, fmt.Errorf("plugin %s: kind must be \"line\" or \"file\"", man.id)
	}
	return man, nil
}
