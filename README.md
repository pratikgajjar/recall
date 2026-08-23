# recall

> Your AI chat history, searchable. Across Cursor, Claude Code, Codex, and pi.
> Really fast. Read-only. No copies.

`recall` indexes the conversations you've already had with Cursor, Claude Code,
OpenAI Codex CLI, and pi — straight from their native storage. It does not move,
copy, or modify your data. It builds a tiny searchable index over excerpts and
metadata, and lets you grep the lot from your terminal.

![recall running inside pi](packages/pi-recall/assets/demo.png)

_Above: the [pi extension](packages/pi-recall) lets an agent run `recall`
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
    id=cursor:a3f21c08-0000-4a1b-9c2d-5e6f70819abc  msg=1  role=assistant
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

## Use it in your agent

recall is a CLI, and agents use it the way you would. There is no server and no
tool schema: one surface, one set of flags, one thing to keep correct.

**As a skill** — for any agent with a skills system and shell access (Claude
Code Agent Skills, pi skills):

```bash
npx skills add pratikgajjar/recall          # via the skills CLI (skills.sh)
# or manually:  cp -r skills/recall ~/.claude/skills/recall
# or from the binary, which carries its own copy:
recall skill install
```

**pi** — `pi install npm:@pratikgajjar/pi-recall`
([packages/pi-recall](packages/pi-recall)). The extension installs the skill and
keeps the index warm in the background. It registers no tools.

**Anything else with shell access** — point it at `recall --help`. The skill
file is just the short version of that, written for an agent.

The skill is also embedded in the binary. `recall doctor` flags installed copies
that have fallen behind it, and `recall skill install` refreshes them — a stale
skill keeps recommending the expensive path long after the binary stopped.

Once connected, ask the agent things like _"use recall to find how we fixed the
import cycle"_ and it will search and read the relevant past session.

For huge sessions, every integration takes `range` (Python-style slice like
`"305:315"` or `"-50:"`) and `outline` (one line per message), so an LLM can
navigate and read slices instead of dumping a 25 MB transcript into context.
Sessions over 200 messages default to outline when no slice is requested.

## Commands

```
recall <query>                     search; prints ranked hits (alias: 'find')
recall last [--repo P]             full transcript of the most recent matching session
recall show <session-id>           full transcript of one session
recall sessions [--repo P]         list recent sessions
recall related <session-id>        sessions on the same topic
recall open <session-id>           reopen in the source tool (cursor://, claude --resume, …)
recall tag                         list all tags + counts (git-tag style)
recall tag <session-id> <tag>…     attach durable tags (survive reindex)
recall tag -d <session-id> <tag>…  remove tags
recall stats [flags]               sessions/messages/tokens/cost by source, project, model
recall index [--full] [--prune]    (re)build the index; --prune drops sessions
                                   whose source was deleted (needs --full)
recall plugin list                 show bundled + installed Lua plugins
recall plugin install <name>       install a bundled plugin into ~/.recall/plugins
recall doctor                      health check
```

Flags can appear anywhere on the line:

```
--repo PATH      restrict to a project folder
--after WHEN     started after: a duration (7d), epoch-ms, or YYYY-MM-DD
--before WHEN    started before (pages back through history)
--since WHEN     alias for --after
--limit N        default 30
--tag SELECTOR   filter, repeatable (AND). A user tag, or a reserved facet:
                 source:cursor|claude|codex|pi
--json           machine-readable output
--context N      print N messages around each hit (like grep -C), so a search
                 answers "find it and show me" without a follow-up `show`
```

`stats` also takes `--projects N` / `--models N` (rows per table, `0` = all)
and `--csv <prefix>` (see below).

`find`, `sessions`, and `show` print terse, copy-pasteable **`next:`/`prev:`**
page commands at the bottom (text mode) so you — or an agent — can traverse all
matches. Across sessions it pages by time (`--before`/`--after`); within a
session it pages by message window (`--range`). `--json` output stays clean; use
`started_at_ms` to page programmatically.

### Tags

Tag any session to find it again later — tags are **durable**: they live in a
separate table the indexer never rebuilds, so they survive `recall index --full`.

```bash
recall tag cursor:94dc8775-… deploy-rca auth-design   # remember this session
recall sessions --tag deploy-rca                      # find tagged sessions
recall sessions --tag deploy-rca --tag source:cursor  # tag + facet, AND
```

`source` is exposed as a reserved **facet** through the same `--tag` selector
(`--tag source:cursor`) rather than a separate flag — one filter vocabulary,
k8s-label style. Reserved facets are derived from the session, so you can filter
by them but can't author them as tags. Agents can tag with `recall tag` the
tool to bookmark sessions worth remembering.

### Tokens and cost

`recall stats` reports what your sessions actually cost, rolled up by source,
project, and model:

```
$ recall stats --projects 2 --models 4
SOURCE  SESSIONS  MESSAGES  TOKENS  COST        PROJECT
pi      686       313807    31.3B   $17190.19   (all)
        20        69379     5.7B    $2701.57      ~/code/ledger
        187       68149     6.7B    $2273.09      ~/code/api-server
                                                  … 73 more (--projects 0)
codex   1088      37493     771.4M  -           (all)

MODEL                SOURCE  SESSIONS  TOKENS  CACHED  COST
claude-opus-4-8      pi      192       8.2B    96%     $6687.34
claude-opus-4-7      pi      122       7.2B    96%     $2732.05
gpt-5.6-sol          codex   18        415.5M  93%     -
… 68 more
```

Nothing here is computed from a price list recall carries — there isn't one.
Every number is read back out of what the tool itself recorded:

| Source | Model | Tokens | Cost |
|---|---|---|---|
| pi | `message.model` | per-message `usage` summed over turns | `usage.cost.total`, priced by pi |
| codex | `turn_context.model` | `token_count` running total | — (subscription; codex logs quota %, not dollars) |
| cursor | `modelConfig.modelName` | `contextTokensUsed` | `usageData.costInCents` |
| claude, cursor-agent, obsidian | — | estimated | — |

Sources that record no usage fall back to a character-count estimate, always
printed with a leading `~` so it is never mistaken for a measurement. A `-`
means the source genuinely does not record that number. Sessions from a source
with no model name still appear, grouped under `(unknown)`, so the token totals
always add up.

### CSV export

```bash
recall stats --csv usage-      # → usage-projects.csv, usage-models.csv
```

Raw unformatted numbers (`8152978475`, not `8.2B`; no `$`, no `~`) so a
spreadsheet can do arithmetic on them; the `~` becomes an `estimated` column you
can filter on. All filters apply (`--repo`, `--since`, `--tag`), and row caps do
not — an export is everything.

```bash
# cost per 1M tokens, measured rows only
recall stats --csv u- && python3 -c "
import csv
for r in csv.DictReader(open('u-models.csv')):
    if r['estimated']=='false' and float(r['cost_usd'])>0:
        print(r['model'], round(float(r['cost_usd'])/(int(r['tokens'])/1e6), 3))"
```

## What it reads

| Tool | Storage | Notes |
|---|---|---|
| Cursor | `~/Library/Application Support/Cursor/User/globalStorage/state.vscdb` and per-workspace `state.vscdb` | Walks `composerData:*` + `bubbleId:*` blobs; joins to workspace folder via `workspaceStorage/*/workspace.json` |
| Claude Code | `~/.claude/projects/<sanitized-cwd>/*.jsonl` | One file per session, append-only JSONL |
| Codex CLI | `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` | First line is `session_meta`; rest are `response_item` payloads |
| pi | `~/.pi/agent/sessions/<sanitized-cwd>/*.jsonl` | First line is the `session` event; rest are `message` events |
| Cursor Agent CLI | `~/.cursor/projects/<slug>/agent-transcripts/<id>/<id>.jsonl` | `{role, message:{content:[parts]}}` per line; no per-event timestamps, so sessions are dated by file mtime. Title prefers `~/.cursor/chats/<workspace>/<id>/meta.json` (sidebar name); first user prompt is the fallback |

The index lives at `~/.recall/index.sqlite`. It contains:

- `sessions(id, source, source_id, project, title, started_at, msg_count, …)`
- `messages_fts(session_pk, idx, role, ts, text)` — SQLite FTS5 over excerpts (trimmed to ~1.5 KB per message)

Nuke `~/.recall/` and re-run `recall index` any time. The index is disposable.

## Index anything: Lua plugins

The sources above are built in. Anything else — a notes vault, another
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

## Design

- **Read-only.** Sources stay the source of truth.
- **No materialization.** No markdown copies, no canonical schema migrations.
- **One binary.** Single Go executable, ~10 MB, no CGO.
- **SQLite + FTS5** for search. `bm25` ranking with a small recency lift.
- **Adapter trait.** Add a new tool = implement one interface (`Adapter` in `types.go`).

Inspired by [`fff`](https://github.com/dmtrKovalenko/fff)'s pattern: thin index,
live source reads.

## Status

v0.1. Incremental indexing (append-only) and the pi extension
all work. A filesystem watcher and a TUI are next.

## Licence

MIT.
