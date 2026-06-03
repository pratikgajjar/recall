---
name: recall
description: "Search the user's own past AI chat history across Cursor, Claude Code, Codex, and pi via the `recall` CLI. Use when the user references earlier work — \"how did we fix…\", \"what did I decide about…\", \"continue the…\", \"didn't we already…\" — that may live in a prior conversation."
---

# recall — search past AI chat history

`recall` indexes the conversations you've already had with Cursor, Claude Code,
Codex, and pi into a local SQLite index, and lets you search them from the
shell. It is read-only and never modifies the source data.

Use it whenever the user points at past work that isn't in the current context.

## Prerequisites

`recall` must be on `PATH` and the index must exist. Check both, and bootstrap
if needed:

```bash
recall doctor          # shows detected sources + index status
recall index           # build/refresh the index (incremental, ~ms once built)
```

If `recall` is not installed:

```bash
curl -fsSL https://raw.githubusercontent.com/pratikgajjar/recall/main/install.sh | sh
# or: go install github.com/pratikgajjar/recall@latest
```

## Core workflow

1. **Search** for the relevant session, then 2. **read it in full**.

```bash
# search; ranked hits with matched excerpts and a session id
recall find "import cycle proto" --limit 5

# machine-readable for parsing
recall find "import cycle" --json --limit 5
```

Each hit prints a `session_id` (e.g. `cursor:94dc…`, `pi:019e…`) and a `msg`
index pointing at the matched message. Read it:

```bash
recall show cursor:94dc8775-5fd3-41e9-93d7-43d7dff795b6   # full transcript
recall last --repo .                                      # most recent session in THIS project
```

### Navigating large sessions

Some sessions are huge (thousands of messages — a 30k-msg agent loop is real).
Use `--outline`, `--range`, and `--role` to slice instead of dumping everything:

```bash
recall show <id> --outline                       # [N] role: first-line  (table of contents)
recall show <id> --range :100                    # first 100 messages
recall show <id> --range -50:                    # last 50 messages
recall show <id> --range 305:315                 # window around a search hit at msg_idx=310
recall show <id> --role user,assistant           # skip tool noise (often ~50% of agent loops)
recall show <id> --outline --role user           # just the user prompts — "what did I ask?"
recall show <id> --range 305:315 --role assistant # combine: window + only assistants
```

Every rendered message carries its own `## msg N/TOTAL role` header, so any
slice is self-locating — you can re-call with a new range without re-reading
what came before. Indices are **absolute** (over the full message list), so a
`recall find` hit at `msg=N` always maps to `--range N-5:N+5` even with a role
filter active.

Roles canonicalise to three buckets: `user`, `assistant`, `tool` (anything
matching `tool*` / `function_call*` is `tool`).

**After a `recall find` hit at `msg=N`, prefer
`recall show <id> --range N-5:N+5` over reading the whole session.**
**For unfamiliar large sessions, start with `--outline --role user` — the user
prompts are a tiny, navigable spine of what the session was about.**

## All commands

```
recall find <query> [flags]    search; ranked hits (also the default verb)
recall show <session-id>       full transcript of one session
recall last [flags]            full transcript of the most recent matching session
recall sessions [flags]        list recent sessions (titles + ids, no bodies)
recall related <session-id>    sessions covering the same topic
recall stats [flags]           session/message counts by source/project
recall doctor                  health check + detected sources
```

## Flags (anywhere on the line)

```
--repo PATH        restrict to a project folder ('.' = current dir)
--source NAME      cursor | claude | codex | pi
--since DURATION   e.g. 24h, 7d, 30d
--limit N          default 30
--json             machine-readable output (snake_case)
```

## Tips

- Scope to the current project with `--repo .` — most "didn't we already…"
  questions are about the repo you're in.
- Use **concrete identifiers** in the query (error strings, symbol names, file
  paths, feature names), not vague phrases — it's full-text search.
- Pipe a transcript straight into the model: `recall last --repo . | …`.
- `--json` returns an array of hits with `session_id`, `project`, `title`,
  `snippet`, `started_at_ms` — parse it when you need to act on results.
- After finding one relevant session, `recall related <id>` widens to
  neighbouring conversations on the same topic.

## When NOT to use

- Don't use it for general web/doc search — it only covers *your own* past AI
  conversations.
- Don't rebuild with `recall index --full` unless the index looks corrupt; the
  default `recall index` is incremental and cheap.
