# recall — what the autoresearch loop measured, and what it found

The loop that produced most of this repo's search and indexing behaviour ran
here for a few weeks. Its scripts are gone: they were mechanical, tied to one
machine's corpus, and anyone rebuilding them would want their own. What could
not be rebuilt by reading the code is below — the rubric that made the loop
honest, the ways it fooled itself, and every experiment that failed with the
numbers that killed it.

Read this before optimising anything here. Several of the obvious ideas have
been tried and are worse; the numbers are in the last section.

---

## The rubric

Three axes, weighted equally, each scored 0-100 against **absolute** anchors —
not against yesterday's number, so the score says how good recall *is* rather
than whether it moved.

| Axis | Question | Failure it prevents |
|---|---|---|
| Reliability | Is everything on disk in the index, and can it be found? | Silent data loss: answering confidently from a corpus with holes. |
| Performance | How long does anyone wait? | A tool too slow to reach for. |
| Cost | How many tokens does an answer take? | An agent that can afford one lookup per task. |

Sub-scores, with the anchors that scored 0 and 100:

| Sub-score | Measures | 0 | 100 |
|---|---|---|---|
| `coverage_sessions` | sessions on disk present in the index | 90% | 100% |
| `coverage_prose` | sessions holding the prose recall means to index | 60% | 100% |
| `integrity` | free of duplicate and stale rows | 98% | 100% |
| `findability` | a real sentence retrieves its own session | 80% | 100% |
| `findability_deep` | a phrase from past 3,000 chars retrieves its session | 50% | 100% |
| `searchable_prose` | prose surviving the index-time truncation cap | 80% | 100% |
| `search_latency` | search p95 | 1000 ms | 50 ms |
| `transcript_latency` | transcript p95 | 2000 ms | 100 ms |
| `cold_index` | first full index | 180 s | 30 s |
| `incremental_index` | re-index with nothing changed | 30 s | 2 s |
| `token_cost` | characters a replayed real workload costs | 1600 kch | 400 kch |
| `skill_cost` | the instruction surface an agent must read | 12000 | 3000 |
| `outline_cost` | cost of the navigation surface | 400 kch | 100 kch |

Final state: **87.3** — reliability 95.6, performance 94.2, cost 72.2. Cost is
the weakest axis and largely irreducible: a range read is ~90% content the
caller asked for, assistant prose alone being 66% of it.

---

## How the loop fooled itself

Every one of these cost a round or more. They are the reason to distrust a
number that moves in the direction you hoped.

**A metric that cannot move is worse than no metric.** Two of the three cost
terms were ranking figures read out of *historical* logs — what agents saw at
the time. No code change could ever move them. They sat frozen inside a score
whose entire job was detecting change, and they made a change that degraded
retrieval look like a +4.5 improvement.

**The score needs a veto.** No term measured whether the right session still
beats its competitors for a short, ambiguous query. A separate gate did: build
two indexes from the same disk in the same minute, one per binary, and compare
retrieval on real query→session pairs. Four changes raised the score while
failing that gate. **A change that fails the gate is discarded regardless of
score.**

**An A/B must vary the thing under test.** The gate first compared two *indexes*
using one binary — invisible to any query-side change, reporting a flat zero
delta no matter how much behaviour changed. It must run each arm's own binary
against its own index.

**A gate that passes when its control is broken is worse than none.** The first
version reported PASS because the control index had failed to build. It now
aborts below a plausible session count.

**Sample the corpus stably.** Shuffling a file list makes the sample a function
of *how many* files exist. Seven new sessions replaced 68% of it and moved the
score two points with no code change. Order by a hash of each path instead: a
new session then displaces at most one.

**Separate the machine from the tool.** Cold-index readings ranged 28s to 49s
purely from contention with other processes, ±13 points on that sub-score —
larger than most real improvements. Take the faster of two runs.

**Instruments are wrong more often than the tool.** Four times an instrument
accused recall of a failure it had not committed: a JSON field read by the wrong
name (reported 0% findability), a fixed-offset slice cutting words in half
("guardrails" → "ardrails", understating deep findability by 12 points), a probe
whose `json.loads` threw on recall's own output, and a boilerplate filter one
metric had and its sibling lacked. **Check disagreements before believing them.**

**Do not adjust a measure that just rejected your change.** Three times a
plausible loosening would have turned a red result green. Where the looser
question was genuinely fairer it was added as a *reported* figure and the strict
one kept as the score.

**A benchmark cannot see what it does not replay.** The workload replays fixed
arguments, so it can measure what an answer costs but never whether an agent
would have *chosen* better. Any change trading measured cost against unmeasured
decision quality is not settled by this rubric.

---

## Findings

## Rejected with evidence: indexing deep message text (3 attempts)

`excerptMax = 1500` leaves 44% of all human and model prose unsearchable. Three
ways of fixing that were built and measured against the same control, all on the
same disk in the same minute:

| variant | deep text indexed | MRR | found | rank@3 |
|---|---|---|---|---|
| control (truncate at 1500) | none | 0.1427 | 28 | 22 |
| raise cap to 24000 | one long document | 0.1302 (-8.8%) | 28 | 18 |
| overlapping windows, max 3 | 4.5k/message | 0.1327 (-7.0%) | 27 | 21 |
| overlapping windows, max 40 | 60k/message | 0.1249 (-12.5%) | 26 | 17 |

It works — a phrase from beyond 3,000 characters found its own session 23/25 of
the time with windowing, against 12/25 truncated. And it costs retrieval on the
short topical queries agents actually type, in proportion to how much is added.
The first 1,500 characters of a message carry the signal; the rest is mostly
noise to a two-word query, and it pushes the right session down.

Two things worth knowing before trying again:

- **The adapters truncate before the indexer sees anything.** `pi.go`,
  `codex.go`, `claude.go` and `cursor_agent.go` all cut at `excerptMax` during
  `Scan`, so the first version of windowing was a silent no-op — the index grew
  by 15k rows (from cursor, which does not pre-cut) and deep findability did not
  move at all. Any future attempt must raise `indexTextMax` in the adapters too.
- **This is a trade, not a defect.** Deep text serves recalling a specific
  detail; head text serves finding the right session. Making them compete in one
  ranking is what fails.

### Next thing to try: let deep text be findable without competing

Put deep windows in a second FTS column and weight it down —
`bm25(messages_fts, 0, 0, 0, 1.0, 0.25)` — so a deep match surfaces when nothing
else matches but never outranks a head match. FTS5 cannot `ALTER TABLE ADD
COLUMN`, so this needs the table rebuilt behind a schema bump to v6. Untested.

## Rejected with evidence: skipping tail windows for tool output

Tool results are 73% of all tail rows and 153MB of indexed text. Dropping their
deep windows halves the index — 770MB to 424MB, 612k rows to 486k — and costs
nothing that the rubric measured at the time:

| | all tails | prose-only tails |
|---|---|---|
| index size | 770 MB | 424 MB |
| deep prose findability | 19/22 | 19/22 |
| **deep tool-output findability** | **19/22** | **10/22** |
| searchable_prose | 94.5% | 94.5% |

`searchable_prose` counts human and model prose only, so it does not move at
all. That is the whole problem: saving 346MB by indexing less looked free.

Rejected because agents search tool output constantly — an error string is
exactly the kind of thing someone half-remembers — and because index size is not
a scored metric, so nothing was gained on the rubric either. Index overhead is
2.1x the text it holds, which is normal for FTS5: the index is large because it
indexes a lot, not because it is wasteful. Adding size as a scored metric was
considered and rejected for creating pressure to index less, which is what the
reliability axis exists to prevent.

`findability_deep` now measures phrases from past 3,000 characters across prose
and tool output both, so this trade is visible next time rather than free.

## Tested, not changed: tailWeight

`tailWeight` discounts matches in the tail column. It is a query-time bm25
argument, so it can be A/B'd against one index without reindexing.

| weight | deep findability (n=90) | MRR vs 0.25 |
|---|---|---|
| 0.25 (current) | 71.1% | — |
| 0.5 | 73.3% | 0.0% |
| 1.0 | 75.6% | **-8.4%** (found -1, rank@3 -3) |

1.0 reproduces the loss the three equal-weight indexing experiments showed,
which is a good sign the measurements are consistent with each other.

0.5 looks free — identical MRR, slightly better deep retrieval — but +2 hits out
of 90 is well inside binomial noise (σ ≈ 4.3). Left at 0.25: moving a constant
for a noise-level gain is fitting the benchmark, not improving the tool. Worth
revisiting only with a deep-retrieval sample large enough to resolve a few
points, which this corpus can support if the probe is made cheaper.

## Rejected with evidence: collapsing identical search hits

A search for "connection pool timeout" returned two hits with the same title,
the same msg_idx and a byte-identical snippet, from two session ids a minute
apart. The corpus is full of this: **23.6% of sessions are exact copies of
another** — an automation that re-ran the same task, each run writing an
identical transcript under a new id. Two such sessions were verified 23/23 rows
identical.

Collapsing hits that share (title, snippet, msg_idx), with over-fetch so
distinct results backfill, looked like a clear win: it claimed to remove 14.9%
of all result rows.

The ranking gate rejected it — MRR -5.6%, found -2, rank@3 -2.

The obvious explanation is that the gate's ground truth points at one particular
copy, so collapsing hides the id it expects. That was checked and is false: only
**1 of 42** sessions agents actually read has a same-title twin. The change was
hiding real answers, not twins.

The signature was the bug. `snippet` is generated from the query match, so two
unrelated sessions quoting the same error line produce identical snippets. With
a true content fingerprint — sha256 over every indexed message of a session —
the redundancy in real results is:

    exact-copy rows in real results: 0 of 442 (0.0%), 0 of 120 searches affected

So the duplication is real in the corpus and never reaches a result page. The
whole 14.9% was coincidental snippet collisions between distinct sessions.

Worth remembering: a cheap similarity signature measured something 15x larger
than the real effect, and in the opposite direction of useful. If this is
revisited — for index size rather than search cost — dedupe at ingest on a
content fingerprint, never on rendered output.

## Rejected with evidence: not discounting tail matches in the AND pass

The tail column is discounted (tailWeight 0.25) so deep text does not outrank a
message whose opening is about the query. The argument for exempting the AND
pass: every row it returns contains *every* term, so a deep match there is as
complete an answer as a shallow one, and the discount looked arbitrary.

Measured: deep findability 69.2% -> 70.8% on 120 phrases (+2, inside noise).
Gate: **MRR -3.8%, rank@3 -3, found -1. FAIL.**

The reasoning was wrong. A complete match in the tail is genuinely less relevant
than a complete match in the opening — position carries information about what a
message is *about*, not merely what it contains. The discount was doing real work
in the AND pass too.

Together with the tailWeight sweep, three separate attempts to make deep text
rank higher have now failed at the same wall. It is not a tuning problem.

## The deep-findability gap was mostly boilerplate, not retrieval

Of 35 deep failures, **29 used a phrase shared by more than 30 sessions** — a
repeated `ssh … docker restart` command, `ctx cancel context WithTimeout` (df
137). No ranking can attribute text like that to one session, and `findability()`
had excluded such phrases from the start. `deep_findability()` did not, so it was
measuring ambiguity as if it were failure.

Applying the same rule moves it 70% -> 77.9%. Only 6 of 35 failures were real
retrieval misses.

## Rejected with evidence: one query shape for filtered and unfiltered search

Unfiltered search uses a pooled shape (rank a top-n pool, apply the recency
tiebreak to it); filtered search materialises every match and sorts. Two shapes
is more code than one, and pooling *is* sound for filtered searches as long as
the filter goes inside the ranked subquery rather than around it — which was
verified: filter-inside and materialise-all return the same 200 rows in 26ms and
27ms respectively.

So the shapes were unified. It was worse on both axes:

| | two paths | unified |
|---|---|---|
| repo-filtered p50 | 130 ms | 139 ms |
| repo-filtered p95 | **295 ms** | **816 ms** |
| unfiltered p50 | 15 ms | 15 ms |
| identical output | — | **142 of 218 searches** |

Two reasons. A filter already narrows the scan, so there is little for a pool to
save; and filtered searches usually return few distinct sessions, which triggers
the pool-growth retry loop — up to three queries where there had been one. The
differing output is the pool truncating the candidate set before the recency
tiebreak sees it, which is a real behaviour change with no upside.

The two shapes exist for a reason: a filter narrows before ranking, no filter
needs ranking to narrow. Reverted.

## findability_deep is close to its meaningful ceiling

Four attempts have now failed to raise deep ranking: a larger excerpt cap,
equal-weight windows at depth 3 and 40, a tailWeight sweep, and exempting the
AND pass from the tail discount. Three of those failed the ranking gate; the
fourth moved 2 hits in 120, inside noise.

Looking at what actually fails is more useful than a fifth attempt. Of 9 misses
in 68 probes, the top-ranked results carry *the same matched text as the wanted
session*:

    query   'raise RuntimeError OpenSearchV GetAllBackend requires SEARCH BAC'
      #1    …raise RuntimeError("CI requires the OpenSearch v3 acc…
      #2    …raise RuntimeError("OpenSearchV3Store requires V3_SEAR…
      #3    …raise RuntimeError("OpenSearchV3GetAllBackend requires…

The same error string, the same pasted paragraph, in several sessions. Recall
returns equally valid answers and the probe demands one particular id. Judged by
whether the *content* was reached, deep findability is 91.2% rather than 86.8%.

That looser number is now reported but deliberately **not** scored. It is the
fairer question, and it is also weaker: it would soften precisely the ranking
regressions the audit exists to catch. Three of the nine misses are this; the
other six are real.

Concretely: the remaining gap is roughly 6 probes in 68, and it is competition
between genuinely similar content, not a defect in how deep text is indexed.
Anyone attacking this again should first show the failures are not this.

---

## Indexing: what was already tried

**Writing is 76% of a cold index, not parsing.** A scan-only probe splits it:
parse 36.2s of 155s. pi 50.2s → 3.1s parsing, cursor-agent 33.3s → 0.3s, codex
19.7s → 0.7s, cursor 45.7s → 32.1s. Cursor is the only source where parsing
dominates.

- **Parallel adapters — tried, reverted, -27%.** All six adapters stream, so the
  stated prerequisite was met, but they queue on the single SQLite writer:
  155s → 197s, with cursor's phase going 45.7s → 194.8s purely waiting on the
  ingest mutex. Parallelising parse cannot help while writes are three quarters
  of the work.
- **Parallel bubble decode within cursor — tried, reverted, +4.8%.** Reading
  4.33 GB of blobs with zero decoding takes 29.19s of a 31.0s cursor phase.
  Cursor is I/O-bound, not CPU-bound.
- **Targeting the delete — done, 155.03s → 44.73s.** It was never the insert:
  inserting all 436k messages costs 3.3s. `messages_fts` declares `session_pk
  UNINDEXED`, so deleting a session's rows scanned the whole table once per
  session. Recording the rowid run each session occupies made it seekable.

**Floor for this design: ~40s** — 29s cursor read, 7s pi, ~3s the rest, plus FTS
commit. Everything below that needs a different storage layout, not tuning.

---

## Still untried

Only the ones that survive the findings above:

- **Deep text in a second FTS column, weighted lower still.** The tail column
  exists at weight 0.25. Everything tried to raise deep ranking has failed; a
  genuinely different mechanism is needed, not another constant.
- **SQLite write tuning during bulk ingest** — `synchronous=OFF`,
  `temp_store=MEMORY`, larger `cache_size`, restored afterwards. The index is
  disposable, so the risk is low.
- **`VACUUM` after `--full`** — a rebuilt index carried 81,846 free pages
  (~335 MB) against 1,739 in a fresh build.
- **Trigram tokenizer** for partial-token matches. Would bloat the index;
  measure before believing it.
- **Frecency** — weight sessions the user actually reopens.
