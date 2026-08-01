# recall — autoresearch

This file holds the optimization objective and rules for the autoresearch loop
that lives **inside the recall repo itself**, separate from any other repo's
autoresearch. The pi harness's autoresearch tools point at pi's cwd, so we do
not use them here. Experiments are run with `Bash` + `time`, results recorded
in `autoresearch.jsonl` at the repo root — that file is written by the pi
harness, so it stays there while every other artifact lives in `autoresearch/`.
Results from the previous single-metric era are kept in
`autoresearch/results-archive-workload_kch.jsonl`; they are not comparable to
`recall_score` and are retained only as a record of what was tried.

## Objective

recall has three jobs. Optimising any one of them in isolation is how a tool
ends up fast, cheap and wrong — indexing less makes it quicker and cheaper and
useless; showing more makes it more useful and dearer. So all three are scored
on the same 0-100 scale, **weighted equally**, and every sub-score is published
so a gain on one axis can never quietly pay for a loss on another.

| Axis | The question | Failure it prevents |
|---|---|---|
| **Reliability** | Is everything on disk in the index, and can it be found? | Silent data loss. The worst failure: recall answers confidently from a corpus with holes in it. |
| **Performance** | How long does anyone wait? | A tool too slow to reach for. |
| **Cost** | How many tokens does an answer take? | An agent that can afford only one lookup per task. |

## Primary metric

**`recall_score`** — the equally-weighted mean of the three axis scores, each
itself the mean of its sub-scores. Direction: **higher is better.**

    autoresearch/score.py            # full run, ~4 min
    autoresearch/score.py --quick    # reuse the cached fresh index
    autoresearch/score.py --json     # for the loop

### The rubric

Anchors are **absolute**, not "yesterday's number": each sub-score names a floor
worth 0 and a target worth 100, chosen from what recall should plausibly
achieve. The score therefore says how good recall *is*, not merely whether it
moved. 100 everywhere should be hard and should mean something.

| Sub-score | Measures | 0 pts | 100 pts |
|---|---|---|---|
| **Reliability** ||||
| `coverage_sessions` | sessions on disk present in the index | 90% | 100% |
| `coverage_prose` | sessions holding the prose recall intends to index | 60% | 100% |
| `integrity` | free of duplicate and stale rows | 98% | 100% |
| `findability` | a real sentence retrieves its own session | 80% | 100% |
| `searchable_prose` | prose surviving the index-time truncation cap | 80% | 100% |
| **Performance** ||||
| `search_latency` | search p95 | 1000 ms | 50 ms |
| `transcript_latency` | transcript p95 | 2000 ms | 100 ms |
| `cold_index` | first full index | 180 s | 30 s |
| `incremental_index` | re-index with nothing changed | 30 s | 2 s |
| **Cost** ||||
| `token_cost` | characters the replayed workload costs | 1600 kch | 400 kch |
| `ranking` | MRR of the right session | 0 | 1.0 |
| `retrieval_hit` | the right session appears at all | 0% | 100% |

### Where the instruments point

Reliability is measured against an index built from **the disk as it is now**;
auditing the frozen corpus reports every session created since the freeze as
missing, and inherits defects already fixed. Cost and ranking are measured
against the **frozen corpus** at `~/.recall/bench.sqlite`, because those numbers
only compare across rounds if the corpus holds still.

`autoresearch/audit.py` is written **without recall's own parsing code** and
reads the raw session files itself. An audit that shares code with the thing it
audits cannot detect the bug where that code drops data — which has happened
twice here: codex once dropped 2,336 of 2,336 lines behind a decode error, and
claude tool calls went unattributed for three rounds because the local corpus
never exercised them.

## Secondary metrics (watched, not optimized for)

Every sub-score above is reported on every run. The axis scores are what get
compared; a change that lifts `recall_score` while dropping any axis by more
than a point needs its trade stated explicitly in the result description.

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
secrets across every tracked file — including `autoresearch.jsonl`, where it is
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
- One change per commit, one row per `autoresearch.jsonl` line. Use status
  values `keep`, `discard`, `crash`.
- `autoresearch/ideas.md` collects optimizations we want to try later.
  Move them up to the loop when they're ripe; prune when they go stale.
