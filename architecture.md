# recall architecture

recall is a **host**, not a fixed set of integrations. It owns indexing, search,
and MCP; *what* gets indexed is defined by plugins. New sources — a new agent, an
Obsidian vault, a knowledge graph — are added as data and Lua, never by changing
Go and cutting a release.

## The contract

Everything reduces to one contract, defined in `types.go`:

- a **Session** — a unit of grouping (a chat, a note, a document)
- a stream of **Messages** — searchable records (`role`, `ts`, `text`)
- an opaque **checkpoint** string — a resumable cursor recall stores per source

recall handles the rest: incremental/offset-resumable scanning (`checkpoint.go`,
`streaming.go`), batched FTS5 ingest (`index.go`), ranked search, and the MCP
server (`mcp.go`). A source is anything that produces those records. "Chat" is
just the shape where records are messages with `user`/`assistant`/`tool` roles;
a note or a graph node is the same contract with a different role and grouping.

## Two ways to implement a source

1. **Built-in Go adapters** (`cursor.go`, `claude.go`, `codex.go`, `pi.go`) —
   the hot, popular sources. They stay in Go so the primary metric in
   `autoresearch.md` (full-index time, dominated by Cursor's ~245k blobs) never
   pays a plugin cost.

2. **Lua plugins** (`lua.go` + `~/.recall/plugins/*.lua`) — everything else, and
   anything a user wants to add without recompiling. This is the extension
   mechanism.

Both satisfy the same `Adapter` interface, so search, `show`, `open`, MCP, and
`doctor` treat them identically. A plugin may even **override a built-in of the
same id** — drop a `claude.lua` in your plugins dir and it replaces the Go Claude
adapter, so you can iterate on a source without recompiling.

## Lua plugins: I/O in Go, logic in Lua

The split that makes plugins safe and easy:

```
recall (Go)  ── owns ALL I/O: walk files, read bytes, offset-resume, checkpoint, FTS
   │  raw bytes + the recall.* helpers
   ▼
plugin (Lua) ── owns ONLY logic: bytes → {session}, {message…}   pure, no I/O
```

The Lua VM (gopher-lua — **pure Go, no CGO**, so the single static binary
survives) is built with **no `os`, `io`, `package`/`require`, or networking**.
The sandbox is by omission: those capabilities are simply never injected, and
Lua has no built-in networking. A plugin is a pure, deterministic transform — it
cannot read the disk, shell out, or reach the wire. The only host API is a small,
stable helper table, so there is no I/O surface to version over time.

Because I/O lives in Go, plugins inherit incremental indexing for free: recall
passes the previous checkpoint and persists the new one exactly as for built-ins.

### Plugin contract

A plugin is a `.lua` file returning a table:

```lua
return {
  id = "gemini",                 -- source name (must be unique)
  kind = "line",                 -- "line" (JSONL, offset-resumable) | "file" (whole file)
  roots = { "~/.gemini/tmp/*" }, -- ~-expanded; trailing globs allowed
  glob = "session-*.jsonl",      -- matched against each file's base name
  resume = "gemini --resume {id}",

  -- kind="line": called per line with a per-file state table. Stash the session
  -- id/cwd you learn from a meta line in `st`; return a message or nil.
  line = function(line, st) ... return { role=, ts=, text= } end,

  -- kind="file": called once per file. Return a session table and messages.
  file = function(doc) ... return session, messages end,
}
```

`st` (line) is seeded with `path/name/basename/dir`; set `st.id`, `st.project`,
`st.title`, `st.started_at`. `doc` (file) carries `text/path/dir/name/basename/mtime`.

### recall.* helpers (the entire host API)

| Helper | Purpose |
|---|---|
| `recall.get(bytes, "a.b[0].c")` | extract one value by path (sonic AST — heavy decode stays in Go) |
| `recall.json(bytes)` | full decode to a Lua table (escape hatch) |
| `recall.lines(s)` | split into a line array |
| `recall.time(s [, fmt])` | parse → unix ms (`rfc3339`\|`unix_ms`\|`unix_s`\|Go layout) |
| `recall.truncate(s, n)` | cap text length |

Authors write glue, never a JSON or time parser; the heavy work stays in Go.

## Beyond chat

Nothing in the contract is chat-specific. `plugins/obsidian.lua` indexes a
Markdown vault: each note is one record (`role = "note"`, text = body, title =
first heading), searchable alongside transcripts — no Go change. A
knowledge-graph plugin would instead emit one record per node. The host indexes
text; the plugin decides what a record *is*.

## Validation

The built-in JSONL adapters are reproduced exactly by Lua plugins
(`plugins/claude.lua`, `codex.lua`, `pi.lua`), and `lua_test.go` runs both the Go
adapter and the plugin over the **same fixtures**, asserting identical sessions
and messages — including the offset-resume / append path. `TestLuaSandbox` proves
`os`/`io`/`require`/`load` are unavailable. So the plugin model is provably
capable of everything the built-ins do.

Running `recall plugin test plugins/claude.lua` against a *real* transcript also
surfaced a latent bug in the built-in Go adapter: a `tool_result` with array
content (common in real data) failed to unmarshal and silently dropped the whole
message. Both the Go adapter and the Lua plugin now tolerate string-or-array
content (`TestLuaParityClaudeArrayToolResult`).

## Performance

- Built-ins stay in Go; Lua runs only for opt-in plugins, on far smaller data.
- One VM per scan, reused across files; `recall.get` keeps decoding in Go rather
  than marshaling whole blobs into Lua.
- Each file is bounded by a per-file timeout, so a runaway or malicious plugin
  can't hang or DoS indexing — the bad file is skipped and the scan continues
  (`TestLuaRunawayPluginSkipped`).
- `bench.sh` remains the gate on every change.

## Adding a source

Drop a `.lua` file in `~/.recall/plugins/`. `recall doctor` lists it and whether
its roots exist; `recall index` picks it up. No fork, no recompile.

Authoring loop:

```
recall plugin list                       # discovered plugins, kind, roots, availability
recall plugin test <plugin.lua> [sample] # dry-run: print the records it produces
```

`plugin test` against a real file is the tight loop — it parses one file
(bypassing roots/glob) and prints the sessions/messages, so you see exactly what
will be indexed before committing to a full scan.

## A note on SQLite sources (Cursor/Windsurf)

These keep history in `state.vscdb` (SQLite KV blobs), not files. The Lua tier is
for `line`/`file` sources: a per-record Lua call across Cursor's ~245k blobs
would wreck the full-index time that `autoresearch.md` treats as the primary
metric. So **SQLite-backed sources stay host-owned Go adapters** — this is the
"I/O is the host's job" boundary, not a gap. The natural extension is a host
`vscdb` connector kind that does the SQLite iteration in Go and calls a Lua
transform only per *session blob* (not per message), which would add Windsurf/Cody
as pure-Lua plugins without the per-bubble cost.

## What's next

- **SQLite/vscdb connectors** as additional host I/O kinds (Cursor/Windsurf are
  SQLite, not files), with the per-row/blob transform still in Lua.
- `recall plugin new/test` for a tighter authoring loop, and `plugin add <repo>`
  to share plugins.
- Host-mediated fetch for authenticated remote sources (the host does the network
  from a declared, approved manifest entry and hands bytes to a `file` plugin —
  the wire never enters Lua).
