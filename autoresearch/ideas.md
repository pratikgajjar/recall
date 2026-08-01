# recall — autoresearch ideas backlog

Promising optimizations to try when the loop has room. Move up to active work
when ripe; delete when stale or done.

## Indexing speed

**Writing is 76% of a cold index, not parsing (round 10).** A `--scan-only`
probe splits it: parse 36.2s of 155s total. pi 50.2s -> 3.1s parsing,
cursor-agent 33.3s -> 0.3s, codex 19.7s -> 0.7s, cursor 45.7s -> 32.1s. Cursor
is the only source where parsing dominates.

- ~~Parallel adapters (streaming made viable).~~ **TRIED, REVERTED, -27%.** All
  six adapters do stream now, so the stated prerequisite was met, but they only
  queue on the single SQLite writer: 155s -> 197s, and cursor's phase went
  45.7s -> 194.8s purely waiting on the ingest mutex. Parallelising parse cannot
  help while writes are three quarters of the work.
- ~~Target ingest.~~ **DONE, 155.03s -> 44.73s (3.47x).** It was not the insert:
  inserting all 436k messages costs 3.3s. It was the *delete*. `messages_fts`
  declares `session_pk UNINDEXED`, so `DELETE ... WHERE session_pk = ?` cannot
  seek and scans the entire FTS table once per session — 110.6s of the 155s run,
  spent on a cold build deleting rows that do not exist. Guarded by a
  primary-key lookup on `sessions`. Parsing now dominates again (cursor 32.9s of
  44.7s), exactly as the round-10 split predicted.
- **The delete is still O(n) when it does fire.** Re-running `--full` over a
  populated index takes ~400s because every delete scans 436k rows. Equally true
  before this change, and `--prune` requires `--full`, so it is worth fixing.
  Two candidates: (a) a side table mapping session_pk -> fts rowid so the delete
  becomes `WHERE rowid IN (...)` and seeks; (b) clear each available source's
  FTS rows once at the start of a full run (12 scans, not 3,583). (b) is simpler
  but opens a data-loss window if the scan dies midway, so (a) is preferred.
- **35 duplicate `(session_pk, idx)` FTS rows, pre-existing.** Same count in the
  old-code corpus and the live index, all in two cursor-agent sessions. Not
  caused by the delete-skip; worth a look on its own.
- ~~Parallel bubble decode within cursor.~~ **TRIED, REVERTED, +4.8% only.**
  The premise was wrong. Reading every bubble blob with *no decoding at all*
  takes **29.19s** (248,340 blobs, 4.33 GB) against a 31.0s cursor phase — so
  decode is ~2s and cursor is I/O-bound, not CPU-bound. Eight workers bought
  2.1s for ~60 lines and a full copy of 4.3 GB. Not worth it.
- ~~Skip cursor bubbles with no top-level `text`.~~ Same refutation: the cost is
  SQLite reading the pages, not sonic decoding them. Filtering in SQL still
  reads every blob.

**Floor for this design: ~40s.** 29s cursor read + 7s pi + ~3s the rest + FTS
optimize. At 44.73s the cold index is within ~12% of it, so further work here
needs a different shape — reading less of Cursor's 12.7 GB database at all
(the bubble blobs are mostly `toolFormerData` we never index), not faster
processing of what is read.

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


## Agent token efficiency (loop 2)

**Result: 6.76x on dev, 6.73x on a never-tuned holdout** (5897.1 -> 872.9 kch
over 323 real agent calls). Benchmark: `autoresearch/replay.py` replays every
recall_search/recall_transcript call real agents made, against a frozen corpus
(`~/.recall/bench.sqlite`); `autoresearch/workflow.py` measures cost-per-task.
`autoresearch/checks.sh` guards utility so "return less" cannot win.

Shipped: tool-result clipping (600), outline tool-run collapse, adaptive
outline density + hard cap, 20k read budget with paging, lenient range parsing,
`--context N` (grep -C parity, one call instead of two), per-message dedupe for
session-scoped search, shared-session header hoisting.

### The benchmark is now saturated
The last three genuine improvements moved it 0.0%, because they land on paths
with zero historical calls (`--in`, `--context`). Do not grind it further —
what is left is 85% real message content, and search at 270 chars/hit
(109 header + 71 id + 90 snippet, all load-bearing). The next honest signal
needs *new* agent traffic, post-release.

### Rejected, with the evidence
- ~~Semantic dedupe of overlapping reads.~~ Real (57/83 same-session range
  pairs overlap, 1991 duplicate slots) but **wrong to build**: re-reading a
  window after compaction is a primary use case, and a stateless CLI has no
  per-agent cursor to key it on. Suppressing repeats would break the tool.
- ~~Snippet-only transcript mode.~~ Superseded by `--context N`.
- ~~Compress `## msg N/TOTAL role`.~~ Measured: 24 chars x 1298 msgs = 8% of
  windowed reads but only 1.6% of the total, against churning an interface
  agents and docs key on. Not worth it.
- ~~Trim search output.~~ Profiled; every field is load-bearing. The only free
  win is dropping column padding when stdout is not a TTY (~1.2%), which is not
  worth the branch.

### The per-turn schema (found round 3)
The benchmark counts call *output*. It never counted the MCP tool schema, which
is re-sent on **every agent turn**: across the 79 real sessions that used
recall, 26,661 turns x 6,517 chars = 173.7M chars, **199x all call output
combined**. Price is heavily cache-discounted, but context-window pressure is
not discounted at all.

Cut 6,517 -> 4,126 (-37%) by deleting rationale prose the model cannot act on
and merging recall_untag/recall_tags into `recall_tag action=...` (three tools
for one concept, never called once, 27% of the schema). `schema_chars` is now
tracked by autoresearch/replay.py and pinned by a test that also asserts the
behavioural steer survives any future trim.

Remaining schema is mostly per-tool repetition of repo/source/since, which MCP
requires. Cutting real tools would remove capability, not waste.

### The surface I was optimising was not the one users hit (round 4)
pi agents — the source of **100%** of the measured workload — do not use the MCP
server. The npm extension (`packages/pi-recall`) declares its own tool schemas,
and it never exposed `session_id` or `context` on recall_search. The cheap path
was **unreachable from pi**: the 0/280 session-scoped searches were not agents
ignoring guidance, the parameter did not exist. Its promptGuidelines also
actively recommended `outline=true`.

Lesson, generalised: *measure which surface the traffic actually goes through
before optimising a surface.* Two tool definitions for one product is the same
drift that let `--in` ship broken and `--json --context` silently ignore a flag.

Corrected the 199x claim from round 3 too: that counted re-transmission, which
prompt caching largely nullifies. For ranking work, the honest figure is
context-window occupancy — schema is ~4.1k chars once per session against
~15k of call output (5.5 calls x ~2.7k), so roughly **22%**, not 199x.

### Guidance drift is the recurring bug class (round 5)
Four surfaces steer an agent, and they rot independently:
`mcp.go` schema | `packages/pi-recall` TS schema + promptGuidelines |
`skills/recall/SKILL.md` (copied into ~/.pi, ~/.claude, ~/.agents) |
`packages/pi-recall/README.md` (ships to npm).

All three installed skill copies predated every optimisation and still said
"start with --outline". Nothing updates a file someone once copied. Fixed
structurally: the skill is embedded in the binary, `recall doctor` reports
copies that have fallen behind, `recall skill install` repairs them.

Same class, four instances now: `--in` returning one hit, `--json` swallowing
`--context`, title rows wasting scoped slots, and stale copied guidance. The
pattern is *a surface that accepts input or gives advice, and is never
asserted*. The 12 guard checks and the skill test exist to close it.

### The savings cost no signal (round 6, verified)
`autoresearch/retention.py` diffs current output against the pre-optimisation binary
over the real workload. **prose_lost = 0** — no user/assistant text disappeared
without a pointer to it. The 6.76x is 2.88M chars of tool output (recoverable
with `tool_chars: 0`) plus 527k of prose deferred behind explicit continuations.
53 of 61 windowed reads complete untouched; 6 of the 8 that page had asked for
>=100 messages. A self-contained guard now keeps partial delivery from ever
becoming silent truncation.

### Search ranking is healthy — do not optimise it (round 7)
Ground truth: the session an agent went on to READ after each real search,
ranked within the results it actually saw. 94 pairs: median rank **1**, 46.8%
chose hit #1, 59.6% top-3, MRR 0.548. 28.7% read a session the results did not
contain — found via recall_sessions, an earlier search, or an id already in
hand, not a ranking failure.

**Measurement trap, worth remembering.** The first version re-ran each query
against the frozen corpus and reported 9.4% rank@1 — a catastrophic-looking
number that was pure artefact: the corpus has grown, and the replay cannot
reproduce every filter. Any "quality" metric that re-executes history is
measuring its own drift. `autoresearch/ranking.py` now reads rank out of the recorded
tool results and says so in its docstring.

### Index-quality hunt: three rejections (round 9)
Round 8's dead-row bug suggested more index hygiene might pay. It did not.

- ~~Fix boilerplate titles.~~ 984 of 1,089 codex sessions (90%) share two
  titles, both the harness's role-assignment preamble ("You are working on a
  Linear ticket ..."), which `looksLikeWrapper` does not catch. **Fixing it
  makes titles worse:** the next user message is "Continuation guidance:".
  There is no human question in these sessions to recover — they are fully
  automated runs — and a "You are ..." heuristic would misfire on real prompts
  like "You are right, let's revert". Checked before building.
- ~~Suppress trivial sessions.~~ 242 sessions (6%) have <=2 messages and agents
  chose **0** of them. But they are only 4.5% of hits shown (43/960) ≈ 1.6% of
  workload chars, and msg_count is a crude proxy: a 2-message session can hold
  a decisive answer. Hiding real content for 1.6% is a bad trade, and n=44
  chosen sessions is weak evidence of uselessness.
- ~~Deduplicate near-identical sessions.~~ The 984 codex runs genuinely are
  near-duplicates, but that is the corpus telling the truth about the work, not
  a recall defect. Nothing to fix in the tool.

Ranking is healthy (round 7) and titles cannot be improved, so search result
quality is not a remaining token lever.

### Dead index rows cost a wasted read each (round 8, fixed)
Nothing ever removed a session whose source file was deleted, so the index only
grows stale: **84 of 87 claude rows and 128 codex rows were unreadable** — still
searchable, erroring the moment an agent opened one. `recall index --full
--prune` now drops what a completed full scan never emitted.

Refusals are the important part: `--prune` without `--full` errors, an empty
scan prunes nothing, one source's scan never touches another's rows, and tags
survive.

### Transcript presentation (rounds 14-15)
- **DONE:** `## msg 20/1371 toolResult` -> `## 20 tool ffgrep`. The /TOTAL was
  already in the session header; repeating it cost ~20k chars for nothing. pi
  records `toolName` and `isError` on every result and both were dropped, so a
  reader had to match results to calls by position. Denser *and* more
  informative: 864.9 -> 857.4 kch.
- ~~Compress `[tool:x]` markers.~~ Measured 3.1% of transcript chars, and 93%
  of runs are a single marker, so collapsing runs saves ~1.4% for a reindex.
  Not worth it.
- ~~Strip `[tool:x]` markers from the search index.~~ **TRIED, REVERTED.** The
  noise was real (68% of "bash" hits, 88% of "edit" hits matched only via a
  marker) but a controlled A/B over the 108 real queries showed rank@1 and MRR
  *identical*, with one query regressing from rank 4 to absent. Agents search
  ticket ids and phrases, not tool names. It also drops 94,892 documents (13% of
  messages are nothing but a marker), which shifts every bm25 score. **Lesson: a
  true statistic about a query pattern nobody uses is not a defect.**
- **`--full` leaves ~335 MB of slack.** 81,846 free pages after repeated full
  rebuilds vs 1,739 in a fresh one. Pages are reused so it is not unbounded, but
  a VACUUM after a full rebuild would return the space. Untested.

### Tool arguments: show them, never index them (round 16)
`[tool:bash]` told you a command ran, not what it ran. bash and read are 72% of
all calls and for both the identifying argument was the whole point. Now shown
clipped to 70 chars, identifying field only, so edit/write bodies (1k-4.5k chars
of args) stay out. Costs +3.0% of workload chars — an information-for-tokens
trade, taken deliberately.

**Indexing those arguments is harmful, and the token metric hides it.** With
args in the FTS text the primary *improved* 1.5%, which looked like a free win.
Against ground truth it was a collapse: rank@1 10 -> 1, MRR 0.153 -> 0.072, 8
sessions lost, none improved. A shell command shares enough vocabulary with an
unrelated query to win the AND pass and crowd out the right session. Stripping
the argument in `trimText` (render path uses Fetch, so display is unaffected)
restores ranking exactly: rank@1 10, MRR 0.153, zero lost.

Two lessons, both now guarded by tests:
- **A token win can be a quality loss.** Fewer characters returned meant fewer
  and worse hits. Any search-affecting change needs the ranking A/B, not the
  replay.
- Strip the *argument*, never the marker. Removing whole marker lines empties
  13% of messages and drops them from the index (round 15).

### Presentation is now consistent across sources (round 17)
Round 16 fixed pi only. The other adapters were wrong in opposite directions:
cursor-agent showed 6,778 markers with **no** argument, codex dumped **400
chars of raw JSON** per call, burying the field that identifies it. One shared
clipped summary now governs all of them (moved out of pi.go).

Round 16's index strip also had a hole: it matched `[tool:x]` and
`[tool_use:x]` but not codex's `[call x]`, so 5,322 codex messages still had
command text in FTS. Now 7. Ranking unchanged (rank@1 10, MRR 0.154).

**The Lua parity test earned its keep**: it failed the moment the Go adapter
rendered a call differently from the plugin. The existing fixtures had no tool
call *with an argument*, so the agreement was untested exactly where it broke.
New test covers that.

Remaining known gap, deliberately not chased: argSummary keys cover
Shell/Read/Grep/rg (command/path/pattern) but not every tool — Glob and friends
show a bare marker. Adding keys per tool is overfitting to one corpus; the
current list covers the high-frequency calls.

### User-turn wrappers: measured, not worth touching (round 18)
Injected scaffolding (`# AGENTS.md instructions for ...`, `<environment_context>`)
is 12.6% of codex user chars and 15.5% of cursor-agent's, and two texts alone
appear in ~450 sessions each. It looks like obvious search noise.

It is not. Of 539 hits across the 108 real queries, **zero** land on a wrapper
message, and they are 0.4% of rendered characters in the workload. Same shape as
round 15: a true statistic about a query pattern nobody uses. Left alone.

### codex tool attribution (round 18)
codex records `call_id` on both a call and its output but the adapter discarded
the link, so `## 7 tool` never said which tool produced the output below it.
Correlating gives `## 7 tool exec_command`, which then made two body prefixes
redundant with the header: `[output] ` (4,818 messages, ~43k chars) and
`[call name] `. Both dropped. Ranking unchanged.

### Presentation: complete across all four sources (round 19)
| source | call argument | result attributed |
|---|---|---|
| pi | round 16 | round 14 |
| cursor-agent | round 17 | n/a (results are separate roles) |
| codex | round 17 (was 400 chars raw JSON) | round 18 (call_id) |
| claude | **round 19** | **round 19** (tool_use_id) |

claude went three rounds unnoticed because this machine has 3 claude sessions
and none contain a tool call — **a source can be silently broken when the local
corpus does not exercise it.** Pinned by fixture, with Lua parity on the same
fixture, rather than waiting for data to appear.

Known remaining gap, still deliberate: argSummary keys are
command/path/query/pattern/file_path, which cover Shell, Read, Grep and rg.
Rarer tools render a bare marker. Adding keys tool-by-tool fits this corpus;
a principled generalisation (e.g. "the only string field") is untested.

### Still open
- **Verify the steering actually changes behaviour.** Blocked, not stale: 49
  pi sessions touched since the change show 0 uses of `--in`/`--context`,
  because they run the *released* 0.6.0. Re-extract the workload once v0.7.0
  is out and the extension updated, then compare outline-call share.
- **Audit the paths you recommend.** Session-scoped search silently returned
  only ONE hit (dedupe was per-session) for the whole time the docs told agents
  to prefer it. Cheap recurring check: for each documented workflow, assert the
  first call returns what the doc claims.
- ~~Adaptive `--context`.~~ **Rejected on the measurement I said to take first.**
  Coverage of what agents historically went on to read: ctx=5 -> 59% (4.2k
  chars), ctx=10 -> 72% overall and **100% on every case where the search
  located correctly** (7.5k). Expanding to turn boundaries would solve a problem
  a larger N already solves, at less predictable cost. The residual failures are
  4/14 cases where search never located: 3 are `-60:` "read the end of the
  session", which is not a search task. Guidance now states what each N buys.
- ~~Count the schema in the headline number.~~ **Decided against.** Re-measured
  honestly it is ~22% of per-session footprint, not 199x; resetting a baseline
  with 21 comparable runs to re-weight a 22% component is a bad trade. It stays
  a tracked secondary with its own budget test.
- **Ship it.** Nothing from rounds 2-5 reaches an agent until v0.7.0 is
  released, npm republished and the extension updated. This is now the single
  blocking dependency for every remaining item.
- **Re-extract the workload after v0.7.0 ships.** Everything now hinges on it:
  the steering, `session_id`, `context` and the leaner schema all reach agents
  only once the extension is republished and updated. That re-extraction is the
  real test of this whole round — outline-call share should collapse.
