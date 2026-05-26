# recall — autoresearch ideas backlog

Promising optimizations to try when the loop has room. Move up to active work
when ripe; delete when stale or done.

## Indexing speed

- **Cursor full scan: stream JSON instead of `json.Unmarshal`.** Each bubble
  blob is parsed in full just to extract `text` / `richText`. Use
  `github.com/valyala/fastjson` or a manual scanner to grab one field; should
  cut Cursor full-scan from ~60s to ~10–15s.
- **Parallel adapters.** Today adapters run sequentially. They share no state
  beyond the index DB and the index supports concurrent writers via a Tx-per-
  adapter pattern. Easy 2–3× on cold builds.
- **Parallel bubble decode.** Inside CursorAdapter, decode bubbles in a worker
  pool (GOMAXPROCS workers reading a channel of raw `[]byte`).
- **SQLite write tuning.** `PRAGMA journal_mode=WAL` is set; also try
  `PRAGMA synchronous=OFF` during ingest, then back to NORMAL.

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
