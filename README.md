# recall

> Your AI chat history, searchable. Across Cursor, Claude Code, Codex, and pi.
> Really fast. Read-only. No copies.

`recall` indexes the conversations you've already had with Cursor, Claude Code,
OpenAI Codex CLI, and pi — straight from their native storage. It does not move,
copy, or modify your data. It builds a tiny searchable index over excerpts and
metadata, and lets you grep the lot from your terminal.

![recall_search running inside pi](packages/pi-recall/assets/demo.png)

_Above: the [pi extension](packages/pi-recall) lets an agent call `recall_search`
to find a past conversation and read it back — no copy-paste._

```
$ recall doctor
recall 0.1.0

sources:
  ✓ cursor  ~/Library/Application Support/Cursor/User/globalStorage/state.vscdb
  ✓ claude  ~/.claude/projects
  ✓ codex   ~/.codex/sessions

index: ~/.recall/index.sqlite (69.5 MiB)
  cursor  1514 sessions
  claude    25 sessions
  codex   1058 sessions
  total   2597 sessions

$ recall "import cycle" --limit 3
2025-09-02 18:36  cursor  ~/code/acme-api    Fix import cycle in proto files
    id=cursor:94dc8775-5fd3-41e9-93d7-43d7dff795b6  msg=1  role=assistant
    I'll help you fix the «import» «cycle» in the request logging…

$ recall last --repo ~/code/acme-api | claude -p "continue this"
```

## Install

```bash
# Homebrew
brew install pratikgajjar/tap/recall

# one-line installer (downloads the right prebuilt binary)
curl -fsSL https://raw.githubusercontent.com/pratikgajjar/recall/main/install.sh | sh

# …or with Go
go install github.com/pratikgajjar/recall@latest

# …or grab a binary from https://github.com/pratikgajjar/recall/releases
```

Then build the index once:

```bash
recall index           # one-time, ~1 minute on real data
recall doctor
recall <query>
```

Pure Go, no CGO. Builds a single static binary.

## Use it in your agent (MCP)

`recall mcp` is a [Model Context Protocol](https://modelcontextprotocol.io)
server over stdio, so any MCP-capable harness can search your past chats. It
exposes `recall_search`, `recall_transcript`, `recall_sessions`, and
`recall_related`, and keeps the index warm in the background.

**Claude Code**

```bash
claude mcp add recall -- recall mcp
```

**Codex** — add to `~/.codex/config.toml`:

```toml
[mcp_servers.recall]
command = "recall"
args = ["mcp"]
```

**Cursor / Cline / Windsurf** — add to the client's `mcp.json`
(e.g. `~/.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "recall": { "command": "recall", "args": ["mcp"] }
  }
}
```

**pi** — use the native extension instead (no MCP needed):
`pi install npm:@pratikgajjar/pi-recall` ([packages/pi-recall](packages/pi-recall)).

**As a skill** — for any agent with a skills system + shell access (Claude Code
Agent Skills, pi skills), install the [`recall` skill](skills/recall) instead of
a server. It teaches the agent to shell out to the CLI directly:

```bash
npx skills add pratikgajjar/recall          # via the skills CLI (skills.sh)
# or manually:  cp -r skills/recall ~/.claude/skills/recall
```

Once connected, ask the agent things like _"use recall to find how we fixed the
import cycle"_ and it will search and read the relevant past session.

For huge sessions, every integration takes `range` (Python-style slice like
`"305:315"` or `"-50:"`) and `outline` (one line per message), so an LLM can
navigate and read slices instead of dumping a 25 MB transcript into context.
Sessions over 200 messages default to outline when no slice is requested.

## Commands

```
recall index                       (re)build the index from all sources
recall <query>                     search; prints ranked hits
recall find <query> [--repo P]     same, with filters
recall last [--repo P]             dump the most recent session as transcript
recall show <session-id>           dump one session as transcript
recall sessions [--repo P]         list recent sessions
recall related <session-id>        sessions on the same topic
recall mcp                         run an MCP server (Claude Code, Codex, Cursor, …)
recall plugin list                show bundled + installed Lua plugins
recall plugin install <name>      install a bundled plugin into ~/.recall/plugins
recall plugin test <file>         dry-run a plugin: print the records it produces
recall doctor                     health check
```

Flags can appear anywhere on the line:

```
--repo PATH      restrict to a project folder
--source NAME    cursor | claude | codex
--since DURATION e.g. 24h, 7d, 30d
--limit N        default 30
--json           machine-readable output
```

## What it reads

| Tool | Storage | Notes |
|---|---|---|
| Cursor | `~/Library/Application Support/Cursor/User/globalStorage/state.vscdb` and per-workspace `state.vscdb` | Walks `composerData:*` + `bubbleId:*` blobs; joins to workspace folder via `workspaceStorage/*/workspace.json` |
| Claude Code | `~/.claude/projects/<sanitized-cwd>/*.jsonl` | One file per session, append-only JSONL |
| Codex CLI | `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` | First line is `session_meta`; rest are `response_item` payloads |
| pi | `~/.pi/agent/sessions/<sanitized-cwd>/*.jsonl` | First line is the `session` event; rest are `message` events |
| Cursor Agent CLI | `~/.cursor/projects/<slug>/agent-transcripts/<id>/<id>.jsonl` | Lua plugin (`cursor-agent`); `{role, message:{content:[parts]}}` per line; no per-event timestamps, so sessions are dated by file mtime |

The index lives at `~/.recall/index.sqlite`. It contains:

- `sessions(id, source, source_id, project, title, started_at, msg_count, …)`
- `messages_fts(session_pk, idx, role, ts, text)` — SQLite FTS5 over excerpts (trimmed to ~1.5 KB per message)

Nuke `~/.recall/` and re-run `recall index` any time. The index is disposable.

## Index anything: Lua plugins

The four sources above are built in. Anything else — a notes vault, another
agent's history, any JSONL or SQLite-backed log — is indexable by a small Lua
plugin, no recompile. A plugin is a sandboxed pure transform (no `os`/`io`/net):
recall owns all I/O, the plugin just maps bytes to records.

```bash
recall plugin list                 # bundled + installed
recall plugin install obsidian     # → ~/.recall/plugins/obsidian.lua
recall index                       # now indexes your vault too
```

Bundled plugins install on demand (nothing auto-runs until you ask); anything in
`~/.recall/plugins/*.lua` is auto-discovered on the next `index`, and a broken
plugin is skipped without blocking the others. Kinds: `line` (JSONL), `file`
(whole document), `kv` (SQLite, incremental via a `watermark`).

See [`plugins/`](plugins/) for examples and
[`architecture.md`](architecture.md) for the full contract.


- **Read-only.** Sources stay the source of truth.
- **No materialization.** No markdown copies, no canonical schema migrations.
- **One binary.** Single Go executable, ~10 MB, no CGO.
- **SQLite + FTS5** for search. `bm25` ranking with a small recency lift.
- **Adapter trait.** Add a new tool = implement one interface (`Adapter` in `types.go`).

Inspired by [`fff`](https://github.com/dmtrKovalenko/fff)'s pattern: thin index,
live source reads, MCP-as-peer.

## Status

v0.1. Incremental indexing (append-only), an MCP server, and the pi extension
all work. A filesystem watcher and a TUI are next.

## Licence

MIT.
