# recall — autoresearch ideas backlog

Promising optimizations to try when the loop has room. Move up to active work
when ripe; delete when stale or done.

## Indexing speed

- ~~Stream JSON for cursor bubbles.~~ DONE — sonic ConfigFastest + typed
  structs, 60s → 37s for cursor phase. (commit 485661e)
- ~~Naive parallel adapters.~~ TRIED, REVERTED — buffering all 4 adapters'
  results in RAM (~150 MB from pi alone) before serial ingest blew wall time
  from 82s → 215s due to GC pressure. (would need true streaming pipeline)
- **Streaming Scan→Ingest pipeline.** Make `Scan` return a channel of
  `(Session, []Message)` so we don't buffer the entire corpus before writing.
  Then parallel adapters become viable. Bigger refactor; deferred.
- **Parallel bubble decode within cursor.** Inside CursorAdapter, decode
  bubbles in a worker pool (GOMAXPROCS workers reading a channel of raw
  `[]byte`). Cursor is now ~50s on cold I/O; ~25-30s of that is JSON decode.
- **SQLite write tuning during ingest.** Try `PRAGMA synchronous=OFF` +
  `PRAGMA temp_store=MEMORY` + larger `cache_size` during bulk ingest, then
  back to NORMAL. Low risk; the index is disposable anyway.
- **Skip cursor bubbles with no top-level `text` early.** Sonic still has to
  decode 4 GB of `toolFormerData`. If we could pre-filter blobs that don't
  contain `"text":` near the start, decode could skip them entirely.

## Index size

- **Drop assistant tool-call boilerplate from FTS** (`<thinking>` blocks,
  long file listings, raw shell logs >N lines). Index a normalized
  representation but keep the raw text addressable for Fetch.
- **Trigram tokenizer** for partial-token matches; might bloat index, measure.
- **External-content FTS5** (`content=…`) referencing a side table; reduces
  duplication when we eventually store full bodies for offline `show`.

## Search quality

- **Frecency** — track `recall open <id>` and `recall show <id>` calls in an
  LMDB-style ranking sidecar; boost recently-opened sessions in results.
- **Project default.** When run inside a git repo, default `--repo` to the
  repo root (not just the cwd). Walk up to find `.git`.
- **Smart-case + fuzzy fallback** (mirror fff): if zero exact hits, retry as a
  fuzzy match over titles.
- **Highlight role badges** in TUI: 👤/🤖/🛠️ for user/assistant/tool snippets.

## Surfaces

- **MCP server** (`recall serve`): expose `recall-find`, `recall-recent`,
  `recall-replay`, `recall-related` so Cursor/Claude/Codex can ask without
  shell-out. Same core lib.
- **TUI** (ratatui-style with `tview`/`bubbletea-go`): live search like `fff`,
  preview pane lazy-fetches transcripts.
- **fsnotify watcher** (`recall watch`): keep the index warm continuously.
  Only useful if we drop the shell-prompt re-run model.

## Adapters

- **Aider** — `~/.aider.chat.history.md` and `.aider.input.history`.
- **Continue.dev** — `~/.continue/` JSON sessions.
- **Zed AI** — TBD, check storage layout.
- **Open WebUI / generic ChatGPT exports** — JSON archive ingest.

## Cleanliness

- Replace ad-hoc `unsanitize()` with cwd-from-event everywhere; drop the
  reverse fn once all adapters carry cwd in their payload.
- `recall doctor --verbose` prints checkpoint state per source so an
  incremental-stuck issue is one command to diagnose.
- Crash test: corrupted SQLite, partial JSONL, missing workspace.json.
