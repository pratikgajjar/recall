# recall — autoresearch

This file holds the optimization objective and rules for the autoresearch loop
that lives **inside the recall repo itself**, separate from any other repo's
autoresearch. The pi harness's autoresearch tools point at pi's cwd, so we do
not use them here. Experiments are run with `Bash` + `time`, results recorded
manually in `autoresearch/results.jsonl`, and commits made in lockstep.

## Objective

Make `recall` indispensable on its own merits:

> Indexing real chat history must feel **free**. Search must feel **instant**.

## Primary metric

**`full_index_seconds`** — wall-clock seconds for `recall index --full` from
a cold start (`rm -rf ~/.recall` first). Current ≈80s on real data (1,514
Cursor + 27 Claude + 1,058 Codex sessions; 96k messages; ~4 GB of blobs).

Direction: **lower is better.**

Why this metric: incremental indexing already hit 10ms — essentially free
on the hot loop. The remaining friction is the first-time setup cost, which
also dominates the developer feedback loop when iterating on adapters.
Cursor full scan is ~58s of the 80s — the JSON-decode pass through 245k
bubble blobs is the largest single cost.

### Earlier primary (kept as secondary)

**`incremental_index_ms`** — re-run with no source changes. Hit 10ms in
commit ca802e8. We monitor it; do not regress.

## Secondary metrics (watched, not optimized for)

- `incremental_index_ms` — must stay <50ms.
- `incremental_one_new_ms` — re-run after one new chat appeared in one source.
- `query_ms` — `recall "import cycle" --limit 5` p50, includes cold-start
  overhead of the Go binary. Acceptable up to 100ms.
- `index_size_mb` — disk footprint. Acceptable up to ~150 MB on this corpus.
- `binary_size_mb` — `go build` output. Acceptable up to 15 MB.

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

## Privacy

This repo is **public**. The benchmark workload is every recall call your agents
actually made — real queries carrying ticket ids, service names and whatever you
were debugging. It is gitignored and must stay that way. Regenerate it locally:

    autoresearch/extract_workload.py

Before pushing, sweep for identifiable names, ticket ids, internal hosts and
secrets across every tracked file — including `autoresearch/results.jsonl`, where it is
easy to quote a real query into an experiment description, and doc examples,
where it is easy to reach for a real vendor or project name. Use invented names
in examples.

## Rules

- Run on real local data (Cursor 1,500+ chats, Claude 25, Codex 1,058).
- **Never cheat the benchmark**: no skipping work conditionally on the
  presence of bench env vars; no hardcoded shortcuts that only kick in for
  the test query.
- A "keep" result must produce a smoke-test pass: full `recall index --full`
  still produces the same session count and FTS still finds known hits
  (`recall "import cycle"` and `recall "race condition"`).
- One change per commit, one row per `autoresearch/results.jsonl` line. Use status
  values `keep`, `discard`, `crash`.
- `autoresearch/ideas.md` collects optimizations we want to try later.
  Move them up to the loop when they're ripe; prune when they go stale.
