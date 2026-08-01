# Plan

Ordered by what the rubric says is broken, not by what is interesting to build.
Baseline **recall_score 70.8** (reliability 77.4 | performance 73.0 | cost 62.1).

## 1. `searchable_prose` — 0/100 (55.7%, floor is 80%)

**44% of all human and model prose has no route to search.** `excerptMax = 1500`
truncates text at index time: 4,992 of 100,969 prose parts are cut, and those
few parts carry nearly half the characters in the corpus, because long messages
are where the substance is — the design discussion, the explanation, the
decision. It is in the transcript and invisible to search.

The obvious fix is a bigger cap, but it is not free and must not be waved
through:
- index size and cold-index time grow (performance axis)
- bm25 changes when documents get longer, and long documents score differently
  under the same query — **the ranking A/B is mandatory**, this is exactly the
  class of change that halved MRR when tool arguments were indexed
- a very long message that is one FTS row may crowd out shorter, better ones

Options, cheapest first: raise the cap; split long parts into overlapping
windows so each stays short (keeps bm25 document lengths sane); index the head
and tail rather than the head alone. Measure all three; do not assume.

## 2. `search_latency` — 0/100 (p95 1,493 ms, p50 15.5 ms)

A 100x tail. The median search is instant and the slow 5% take a second and a
half. Nobody has ever looked at which queries these are — likely common terms
matching an enormous number of rows, where the cost is ranking the matches
rather than finding them. Find the slow queries first, then decide; do not
optimise before the cause is known.

## 3. `ranking` — 55.3/100 (MRR 0.553) and `retrieval_hit` — 71.6/100

28% of the time the session the agent actually chose never appears in results
at all. That is the expensive failure: it costs a whole extra round of searching
and the agent may simply give up. This is a retrieval-quality problem, not a
presentation one, and it is now the largest cost lever left — bigger than any
remaining trim to output format.

## 4. `token_cost` — 59.3/100 (888 kch)

Presentation work has been thorough and is close to exhausted; several recent
rounds moved it under 1%. Expect little here until retrieval improves, at which
point fewer wasted calls will move it more than formatting ever could.

## Standing rules

- Reliability regressions are not tradeable. A change that adds speed or saves
  tokens by indexing less is a loss even if `recall_score` rises.
- Any change touching indexed text runs the ranking A/B before it is kept.
- Never tune against the holdout split; report it, do not optimise it.
