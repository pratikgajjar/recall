---
name: recall
description: "Search the user's own past AI chat history across Cursor, Claude Code, Codex, and pi via the `recall` CLI. Use when the user references earlier work — \"how did we fix…\", \"what did I decide about…\", \"continue the…\", \"didn't we already…\" — that may live in a prior conversation."
---

# recall — search past AI chat history

Read-only full-text search over your past Cursor / Claude Code / Codex / pi
sessions. Use it whenever the user points at past work not in current context.

If `recall` is missing or the index is stale, run `recall index` (incremental,
~ms once built). To install: `go install github.com/pratikgajjar/recall@latest`.

## Workflow: find, then read

```bash
recall find "import cycle proto" --limit 5   # ranked hits; --json to parse
recall show cursor:94dc8775-…                # full transcript of a hit
recall last --repo .                          # most recent session in THIS repo
```

Each hit gives a `session_id` and a `msg` index at the matched message.

## Big sessions: slice, don't dump

Sessions can be 30k+ messages. `recall show <id>` takes:

```bash
--outline                 # [N] role: first-line — a table of contents
--range 305:315           # window (also :100, -50:); indices are absolute
--role user,assistant     # drop tool noise (often ~50% of an agent loop)
```

After a hit at `msg=N`, prefer `recall show <id> --range N-5:N+5`. For an
unfamiliar session, start with `--outline --role user` — the prompts are its spine.

## Tags: durable bookmarks (git-tag style)

Tags survive `recall index --full` (stored apart from the disposable index).

```bash
recall tag cursor:94dc8775-… deploy-rca   # remember this session
recall tag                                # list all tags + counts
recall tag -d <id> <tag>                  # remove   |  -l [id]  list
recall sessions --tag deploy-rca          # filter by tag
```

`--tag` is the one filter selector (repeatable, AND). `source` is a reserved
**facet** on it — filter with `--tag source:cursor` (no separate `--source`
flag); you can't author a facet as a tag.

## Flags & tips

`--repo PATH` (`.` = cwd) · `--since 7d` · `--limit N` · `--json`

- Query with **concrete identifiers** (errors, symbols, paths) — it's FTS, not semantic.
- `recall related <id>` widens to neighbouring sessions on the same topic.
- Covers *only* your own AI chats — not web/docs. `--full` only if the index looks corrupt.
