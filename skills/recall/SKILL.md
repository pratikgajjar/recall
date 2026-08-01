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

## Big sessions: search inside them, don't page them

Sessions can be 30k+ messages. To find a **known topic** inside one, search the
session — do not outline it:

```bash
recall find "connection pool timeout" --in cursor:94dc8775-… --context 10  # ONE call
```

`--context N` prints the surrounding messages with each hit, like `grep -C`, so
"find it and show me" does not need a follow-up `show --range`. Measured against
what agents historically went on to read: `5` covers ~59% of it (~4k chars),
`10` covers ~100% whenever the search landed in the right place (~7.5k). Use 5
to check a detail, 10 to read the exchange. Without it:

```bash
recall find "connection pool timeout" --in cursor:94dc8775-…   # ~400 chars, gives msg=N
recall show cursor:94dc8775-… --range N-5:N+5            # read just that window
```

Measured over real navigations in this repo's own history: searching inside a
session costs ~400 characters against ~4,000 to outline it — about 10x less for
the same landing spot. `--in .` scopes to the current session, which is how you
recover something said before a compaction.

Outline is for a session you know **nothing** about:

```bash
--outline                 # table of contents; tool runs collapse to one line
--range 305:315           # window (also :100, -50:); indices are absolute
--role user,assistant     # drop tool noise (often ~50% of an agent loop)
--tool-chars 0            # un-clip tool output (default clips at 600 chars)
--max-chars 0             # un-cap the read (default pages at 20k chars)
```

Output is bounded by default, and always tells you how to get the rest: a long
outline degrades to user turns + tool runs, oversized reads stop with a
`Continue with range='X:Y'`, and long tool results end in an elision marker.
Asking for `--range 0:500` is not a shortcut — it is half a session (~170k
characters) and you will get the first page of it.

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

`--repo PATH` (`.` = cwd) · `--after 7d` / `--before WHEN` (alias `--since`) · `--limit N` · `--json`

`find`, `sessions`, and `show` print terse **`next:`/`prev:`** page commands at the bottom — follow them to traverse everything. Across sessions pages by time (`--before`/`--after`); within `show` by message window (`--range`). `WHEN` is a duration (`7d`), an epoch-ms (paste `started_at_ms` from `--json`), or `YYYY-MM-DD`.

- Query with **concrete identifiers** (errors, symbols, paths) — it's FTS, not semantic.
- `recall related <id>` widens to neighbouring sessions on the same topic.
- Covers *only* your own AI chats — not web/docs. `--full` only if the index looks corrupt.
