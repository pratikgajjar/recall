#!/usr/bin/env python3
"""The rubric: reliability, performance and cost, equally weighted.

Optimising one number is how a tool ends up fast, cheap and wrong. recall has
three jobs and they trade against each other constantly — indexing less makes it
faster and cheaper and worse; showing more makes it more useful and dearer. So
all three are scored on the same 0-100 scale and averaged with equal weight, and
every sub-score is reported so a gain can never hide a loss.

    reliability   is the information in there, and can it be found?
    performance   how long does the human or agent wait?
    cost          how many tokens does an answer take?

Anchors are absolute, not "yesterday's number". Each sub-score names a floor
(0 points) and a target (100 points) chosen from what the tool should plausibly
achieve, so the score says how good recall *is*, not merely whether it moved.
Hitting 100 everywhere should be hard and mean something.

    autoresearch/score.py            # full run
    autoresearch/score.py --quick    # skip the cold-index rebuild
    autoresearch/score.py --json
"""
import argparse
import json
import os
import random
import shutil
import statistics
import subprocess
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)
BIN = os.path.join(REPO, "recall")
BENCH_INDEX = os.path.expanduser("~/.recall/bench.sqlite")


def norm(value, floor, target):
    """Map a measurement onto 0-100 between an absolute floor and target."""
    if target == floor:
        return 100.0
    x = (value - floor) / (target - floor)
    return round(100.0 * max(0.0, min(1.0, x)), 1)


def run_json(argv, timeout=1800):
    r = subprocess.run(argv, capture_output=True, text=True, timeout=timeout, cwd=REPO)
    out = [l for l in r.stdout.splitlines() if l.startswith("{")]
    if not out:
        raise RuntimeError(f"{argv[0]}: no JSON\n{r.stdout[-400:]}{r.stderr[-400:]}")
    return [json.loads(l) for l in out]


# ---------------------------------------------------------------- reliability

def reliability(index_path, sample):
    a = run_json([sys.executable, os.path.join(HERE, "audit.py"),
                  "--json", "--sample", str(sample), "--index", index_path])[0]
    subs = {
        # A session on disk that is not in the index is invisible: no query can
        # ever reach it. Anything below 99% is a broken adapter.
        "coverage_sessions": norm(a["coverage_sessions_pct"], 90.0, 100.0),
        # A session present but missing most of its prose is worse than absent:
        # it looks searched.
        "coverage_prose": norm(a["coverage_messages_pct"], 60.0, 100.0),
        # Duplicate rows bill twice; stale rows cost a call to discover.
        "integrity": norm(a["integrity_pct"], 98.0, 100.0),
        # The end-to-end question: a real sentence retrieves its own session.
        "findability": norm(a["findability_pct"], 80.0, 100.0),
        # Prose that survives the index-time truncation cap. Text beyond it is
        # in the transcript but has no route to search.
        "searchable_prose": norm(a.get("searchable_prose_pct", 100.0), 80.0, 100.0),
    }
    return subs, a


# ---------------------------------------------------------------- performance

def time_calls(argvs, index_path, timeout=60):
    env = dict(os.environ)
    env["RECALL_INDEX"] = index_path
    ms = []
    for argv in argvs:
        t0 = time.perf_counter()
        subprocess.run(argv, capture_output=True, env=env, timeout=timeout, cwd=REPO)
        ms.append((time.perf_counter() - t0) * 1000)
    return ms


def pct(xs, p):
    if not xs:
        return 0.0
    xs = sorted(xs)
    k = min(len(xs) - 1, int(round((p / 100.0) * (len(xs) - 1))))
    return round(xs[k], 1)


FRESH_INDEX = os.path.expanduser("~/.recall/audit.sqlite")


def build_fresh_index(quick):
    """Index today's disk from scratch, and time it.

    Reliability has to be judged against an index built from the disk as it is
    now. Auditing the frozen benchmark corpus instead reports every session
    created since the freeze as missing, and inherits defects already fixed —
    that corpus still carries ~2,300 duplicate rows from a bug that no longer
    exists. Cost and ranking keep using the frozen corpus, because those numbers
    only mean something if the corpus holds still.
    """
    have = os.path.exists(FRESH_INDEX)
    if quick and have:
        return FRESH_INDEX, None, None
    env = dict(os.environ)
    env["RECALL_INDEX"] = FRESH_INDEX
    for suffix in ("", "-wal", "-shm"):
        try:
            os.remove(FRESH_INDEX + suffix)
        except OSError:
            pass
    t0 = time.perf_counter()
    subprocess.run([BIN, "index", "--full"], capture_output=True,
                   env=env, timeout=3600, cwd=REPO)
    cold = round(time.perf_counter() - t0, 2)
    # The common path: nothing changed since last time.
    t0 = time.perf_counter()
    subprocess.run([BIN, "index"], capture_output=True,
                   env=env, timeout=3600, cwd=REPO)
    incr = round(time.perf_counter() - t0, 2)
    return FRESH_INDEX, cold, incr


def performance(index_path, cold_s, incr_s):
    wl_path = os.path.join(HERE, "agent_workload.json")
    queries, sids = [], []
    if os.path.exists(wl_path):
        for c in json.load(open(wl_path)):
            if c["tool"] == "recall_search" and c["args"].get("query"):
                queries.append(c["args"]["query"])
            sid = c["args"].get("session_id")
            if sid:
                sids.append(sid)
    rng = random.Random(11)
    rng.shuffle(queries)
    rng.shuffle(sids)
    queries, sids = queries[:40], sids[:20]

    search_ms = time_calls([[BIN, q, "--json", "--limit", "10"] for q in queries], index_path)
    tx_ms = time_calls([[BIN, "show", s, "--outline"] for s in sids], index_path)

    raw = {
        "search_p50_ms": pct(search_ms, 50),
        "search_p95_ms": pct(search_ms, 95),
        "transcript_p95_ms": pct(tx_ms, 95),
        "cold_index_s": cold_s,
        "incremental_index_s": incr_s,
    }
    subs = {
        # An agent issues several searches per task; a second each is a wasted
        # minute. 50ms is "feels instant", 1s is "noticeably waiting".
        "search_latency": norm(raw["search_p95_ms"], 1000.0, 50.0),
        # Reading a session is one call but a big one.
        "transcript_latency": norm(raw["transcript_p95_ms"], 2000.0, 100.0),
    }
    if cold_s is not None:
        # A first index is a one-off, but it is the first thing anyone
        # experiences. 155s was the original; ~40s is the measured I/O floor.
        subs["cold_index"] = norm(cold_s, 180.0, 30.0)
        # Re-indexing runs constantly; it should be nearly free when idle.
        subs["incremental_index"] = norm(incr_s, 30.0, 2.0)
    return subs, raw


# ----------------------------------------------------------------------- cost

def cost(index_path):
    rep = run_json([os.path.join(HERE, "replay.py"), "--split", "dev", "--json"])[0]
    rank = run_json([os.path.join(HERE, "ranking.py"), "--json"])[0]
    raw = {
        "workload_kch": rep["total_kch"],
        "est_tokens": rep["est_tokens"],
        "rank_at_1_pct": rank.get("rank_at_1_pct", 0.0),
        "mrr": rank.get("mrr", 0.0),
        "miss_pct": rank.get("chose_outside_results_pct", 100.0),
    }
    subs = {
        # Characters the workload costs an agent. 400k is roughly the floor if
        # every answer were perfectly targeted; 1600k is the unbudgeted original.
        "token_cost": norm(raw["workload_kch"], 1600.0, 400.0),
        # Ranking is a cost metric: a session ranked 8th is read after seven
        # wrong ones. MRR folds in how far down the right answer sits.
        "ranking": norm(raw["mrr"], 0.0, 1.0),
        # Never returning the right session at all is the expensive failure.
        "retrieval_hit": norm(100.0 - raw["miss_pct"], 0.0, 100.0),
    }
    return subs, raw


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--json", action="store_true")
    ap.add_argument("--quick", action="store_true",
                    help="skip the cold-index rebuild (~45s)")
    ap.add_argument("--sample", type=int, default=150)
    ap.add_argument("--index", default=BENCH_INDEX)
    a = ap.parse_args()

    axes, raws = {}, {}
    fresh, cold_s, incr_s = build_fresh_index(a.quick)
    # Reliability: against an index of the disk as it is right now.
    axes["reliability"], raws["reliability"] = reliability(fresh, a.sample)
    # Latency and cost: against the frozen corpus, so rounds compare.
    axes["performance"], raws["performance"] = performance(a.index, cold_s, incr_s)
    axes["cost"], raws["cost"] = cost(a.index)

    scores = {k: round(statistics.mean(v.values()), 1) for k, v in axes.items()}
    total = round(statistics.mean(scores.values()), 1)

    out = {"recall_score": total, "axes": scores,
           "sub_scores": axes, "measurements": raws}
    if a.json:
        print(json.dumps(out))
    else:
        print(f"\n  recall_score  {total}/100"
              f"   (reliability {scores['reliability']} | "
              f"performance {scores['performance']} | cost {scores['cost']})\n")
        for axis in ("reliability", "performance", "cost"):
            print(f"  {axis}  {scores[axis]}")
            for k, v in sorted(axes[axis].items(), key=lambda x: x[1]):
                bar = "#" * int(v / 5)
                print(f"      {k:<20} {v:>5.1f}  {bar}")
            print()
    return 0


if __name__ == "__main__":
    sys.exit(main())
