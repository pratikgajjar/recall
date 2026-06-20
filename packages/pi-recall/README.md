# pi-recall

> A [pi](https://github.com/badlogic/pi-mono) extension that lets the agent search **your past AI chat history** — across Cursor, Claude Code, Codex, and pi — without you copy-pasting transcripts.

It's a thin wrapper over the [`recall`](https://github.com/pratikgajjar/recall) CLI, which indexes your conversations into a local SQLite FTS5 index. The extension shells out to that binary and exposes the index to the agent as tools.

![recall_search running inside pi](./assets/demo.png)

_The agent calls `recall_search` to find a past conversation, then reads it in full with `recall_transcript` — no copy-paste._

## Install

```bash
pi install npm:@pratikgajjar/pi-recall
```

The prebuilt `recall` binary for your platform ships inside the package, so
`pi install` / `pi update` keep the extension **and** the binary in lockstep —
no separate `go install` or `brew`. The extension resolves the binary in this
order: `--recall-bin` flag -> `RECALL_BIN` env -> bundled binary -> `recall` on `PATH`.

Then build the index once:

```bash
recall index        # one-time, ~1 minute on real data
recall doctor       # confirm sources are detected
```

For local development, load the extension ad-hoc:

```bash
pi -e ./packages/pi-recall/src/index.ts
```

## Tools

| Tool | What it does |
| --- | --- |
| `recall_search` | Full-text search over past sessions. Returns ranked hits with matched excerpts and a session id. |
| `recall_transcript` | Read a session in full — by `session_id`, or omit it for the most recent session (filterable by repo/source/since). |
| `recall_sessions` | List recent sessions (titles + ids, no bodies). |
| `recall_related` | Given a session id, find other sessions on the same topic. |
| `recall_tag` / `recall_untag` | Attach/remove durable tags on a session — bookmark sessions worth remembering. Tags survive index rebuilds. |
| `recall_tags` | List all tags (with counts), or the tags on one session. |

All search/list tools accept `repo` (pass `"."` for the current project),
`source` (`cursor` \| `claude` \| `codex` \| `pi`), `since` (e.g. `7d`), and
`tags` (filter to sessions carrying ALL given tags; AND). A tag may be a user
tag or a reserved facet like `source:cursor` — `tags: ["source:cursor"]` is
equivalent to `source: "cursor"`.

### Navigating large sessions

Some sessions are huge (thousands of messages). `recall_transcript` takes
three extra params to slice instead of dumping everything:

- `range` — Python-style slice over the message list: `":100"` first 100,
  `"-50:"` last 50, `"305:315"` window. Negative indices count from the end.
- `outline` — one line per message: `[N] role: first-line`. A cheap table of
  contents you can scan before slicing in.
- `role` — comma-separated allowlist: `"user,assistant"` (skip tool noise),
  `"user"` (just the prompts), `"tool"`. Tool-related labels (`toolResult`,
  `toolCall`, `function_call`, …) all collapse to `tool`. In long agent loops,
  ~50% of messages are tool noise; in some, 98% — a 30k-msg session shrinks to
  ~600 with `role="user"`.

Every rendered message carries its own `## msg N/TOTAL role` header so any
slice is self-locating. **Default-cap:** if a session has more than 200
messages and you ask for neither `range` nor `outline`, the tool returns the
outline (plus a one-line note explaining how to drill in) instead of a
context-blowing wall of text. After a `recall_search` hit at `msg_idx=N`,
prefer `range: "N-5:N+5"` over reading the whole session.

### Recommended agent prompt

Drop into your project's `AGENTS.md` / `CLAUDE.md`:

```markdown
When the user refers to earlier work ("how did we fix…", "continue the…"),
use the recall tools to find and read the relevant past AI session first.
```

## Staying fresh

Because the extension is a long-lived process, it keeps the index warm in the
background so searches always reflect your latest conversations — you never pay
an index rebuild on the query path:

- On **session start** it runs an incremental `recall index` to catch up on
  anything that changed since your last pi session.
- After **each agent turn** it debounces a background refresh, so the session
  you're in right now is searchable moments later.

The incremental index is append-only (it reads just the new lines of changed
session files), so each refresh is typically tens of milliseconds. Disable it
with `--recall-auto-index=false` or `RECALL_AUTO_INDEX=0` and refresh manually
via `/recall-index`.

## Commands

- `/recall-health` — runs `recall doctor` (CLI status + detected sources).
- `/recall-index` — rebuilds the index (`recall index`; pass `--full` for a full rebuild).

## Configuration

| | |
| --- | --- |
| `--recall-bin PATH` flag / `RECALL_BIN` env | Path to the `recall` binary. Default: `recall` on `PATH`. |
| `--recall-auto-index=false` flag / `RECALL_AUTO_INDEX=0` env | Turn off the background index refresh (on by default). |

## License

MIT.
