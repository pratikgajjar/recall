# recall — autoresearch

This file holds the optimization objective and rules for the autoresearch loop
that lives **inside the recall repo itself**, separate from any other repo's
autoresearch. The pi harness's autoresearch tools point at pi's cwd, so we do
not use them here. Experiments are run with `Bash` + `time`, results recorded
in `autoresearch.jsonl` at the repo root, which the pi harness writes and which
is deliberately not committed: it is the record of one machine's run, not
something a reader needs. What survives a run is in this file (the rubric and
the rules) and in `autoresearch/ideas.md` (what was tried, what it measured, and
why it was rejected).

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

### The ranking gate can veto the score

`recall_score` is necessary, not sufficient. No term in it measures whether the
right session still beats its competitors for a short, ambiguous query — the
thing agents actually type. Raising `excerptMax` to 24000 scored **+4.5**
(reliability 77.2 -> 91.9, because 44% more prose became searchable) while MRR
fell **8.8%** and rank@3 went 22 -> 18. The score would have accepted it.

So any change that alters indexed text must pass:

    autoresearch/gate_ranking.sh          # control = HEAD, treatment = worktree

It builds two indexes from the same disk in the same minute and compares
retrieval on real queries; only a same-data comparison isolates the code, since
the corpus grows constantly. A relative MRR loss over 3% fails. **A change that
fails this gate is discarded no matter what `recall_score` says.**

Report `mrr` as a secondary metric on every experiment that touches indexing.

## Secondary metrics (watched, not optimized for)

Every sub-score above is reported on every run. The axis scores are what get
compared; a change that lifts `recall_score` while dropping any axis by more
than a point needs its trade stated explicitly in the result description.

## Benchmark protocol

`autoresearch/score.py` is the single entry point. It builds nothing — build
first — and reports every sub-score plus the raw measurements behind them.

    go build -o recall .
    autoresearch/score.py                 # full run, ~3 min
    autoresearch/score.py --quick         # reuse the cached fresh index
    autoresearch/score.py --json          # for the loop

Then, depending on what changed:

    autoresearch/gate_ranking.sh          # MANDATORY if indexed text or query
                                          # execution changed
    autoresearch/checks.sh                # utility guards, ~15 assertions
    autoresearch/all.sh                   # the descriptive benchmarks together

The workload the replay needs is private and gitignored. Regenerate it once per
machine:

    autoresearch/extract_workload.py

Measurements are only comparable if they are taken the same way. Two habits
matter more than they look:

- **Sample the corpus stably.** The audit orders files by a hash of their path,
  not by shuffling a growing list. Shuffling made a sample a function of how
  many files exist: seven new sessions replaced 68% of it and moved the score
  two points with no code change.
- **Take the faster of two cold indexes.** Readings ranged 28s to 49s on this
  machine purely from contention with other agents, which is not a property of
  recall.

## Privacy

This repo is **public**. The benchmark workload is every recall call your agents
actually made — real queries carrying ticket ids, service names and whatever you
were debugging. It is gitignored and must stay that way. Regenerate it locally:

    autoresearch/extract_workload.py

Before pushing, sweep for identifiable names, ticket ids, internal hosts and
secrets across every tracked file — it is easy to quote a real query into a
commit message or a note, and easy to reach for a real vendor name in
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
- One change per commit, one logged result per change. Use status
  values `keep`, `discard`, `crash`.
- `autoresearch/ideas.md` collects optimizations we want to try later.
  Move them up to the loop when they're ripe; prune when they go stale.
