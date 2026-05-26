# recall — autoresearch

This file holds the optimization objective and rules for the autoresearch loop
that lives **inside the recall repo itself**, separate from any other repo's
autoresearch. The pi harness's autoresearch tools point at pi's cwd, so we do
not use them here. Experiments are run with `Bash` + `time`, results recorded
manually in `autoresearch.jsonl`, and commits made in lockstep.

## Objective

Make `recall` indispensable on its own merits:

> Indexing real chat history must feel **free**. Search must feel **instant**.

## Primary metric

**`incremental_index_ms`** — wall-clock milliseconds for `recall index` to
complete a re-run when no source data has changed since the last index.

Direction: **lower is better.**

Why this metric: the v0.1 hot loop is `cd <project> && recall index && recall <q>`.
If `recall index` is fast on every shell prompt, we never need a daemon, a
file watcher, or an MCP server's freshness ceremony. It just works.

## Secondary metrics (watched, not optimized for)

- `full_index_seconds` — cold rebuild after `rm -rf ~/.recall`. Current ~80s.
- `incremental_one_new_ms` — re-run after one new chat appeared in one source.
- `query_ms` — `recall "import cycle" --limit 5` p50. Already sub-10ms.
- `index_size_mb` — disk footprint of `~/.recall/index.sqlite`. Current ~70 MB.
- `binary_size_mb` — output of `go build`. Currently ~10 MB.

A primary improvement is kept even if a secondary regresses, unless the
secondary regresses catastrophically (e.g. binary doubles, query >100ms).

## Benchmark protocol

The single source of truth is `bench.sh` in this directory. It:

1. Builds the binary (`go build -o recall .`).
2. Runs the experiment in a known state (warm index, no source changes).
3. Times it with `/usr/bin/time -lp` (or `gtime` if available) for repeatable
   wall + cpu + RSS readings.
4. Emits one JSONL line on stdout with `commit`, all metrics, and notes.

The script is fast (<1 min) and idempotent. Re-run as often as you like.

## Rules

- Run on real local data (Cursor 1,500+ chats, Claude 25, Codex 1,058).
- **Never cheat the benchmark**: no skipping work conditionally on the
  presence of bench env vars; no hardcoded shortcuts that only kick in for
  the test query.
- A "keep" result must produce a smoke-test pass: full `recall index --full`
  still produces the same session count and FTS still finds known hits
  (`recall "import cycle"` and `recall "race condition"`).
- One change per commit, one row per `autoresearch.jsonl` line. Use status
  values `keep`, `discard`, `crash`.
- `autoresearch.ideas.md` collects optimizations we want to try later.
  Move them up to the loop when they're ripe; prune when they go stale.
