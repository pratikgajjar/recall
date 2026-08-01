#!/usr/bin/env python3
"""Live retrieval quality: re-run real queries against an index you name.

`ranking.py` reads rank out of the results an agent *actually saw*, recovered
from its logs. That is the faithful measure of what happened, and it is exactly
the wrong tool for an A/B: it is history, so no code change can ever move it.
Two of the three cost sub-scores were that number, sitting frozen.

This runs the same real queries against a live index instead. Absolute values
here are NOT comparable to ranking.py's — the corpus has grown since those
searches ran, and a session the agent chose from ten candidates now competes
with thousands. What is comparable is the same query set against two indexes
built the same minute, which is what an A/B needs.

Ground truth stays the agent's own judgement: for each real search, the session
it went on to read.

    autoresearch/rank_live.py --index /tmp/a.sqlite --json
    autoresearch/rank_live.py --index a.sqlite --compare b.sqlite
"""
import argparse
import glob
import json
import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
BIN = os.path.join(os.path.dirname(HERE), "recall")
PAIRS = os.path.join(HERE, "rank_pairs.json")


def extract_pairs(root):
    """(query, session the agent read next) from real history."""
    pairs, seen = [], set()
    for path in glob.glob(os.path.join(root, "**", "*.jsonl"), recursive=True):
        calls = []
        try:
            fh = open(path, errors="replace")
        except OSError:
            continue
        with fh:
            for line in fh:
                if '"message"' not in line:
                    continue
                try:
                    d = json.loads(line)
                except Exception:
                    continue
                m = d.get("message", {})
                if m.get("role") == "assistant" and isinstance(m.get("content"), list):
                    for p in m["content"]:
                        if isinstance(p, dict) and p.get("type") == "toolCall" \
                                and str(p.get("name", "")).startswith("recall_"):
                            calls.append((p["name"], p.get("arguments") or {}))
        for i, (name, args) in enumerate(calls):
            if name != "recall_search" or not args.get("query"):
                continue
            for n2, a2 in calls[i + 1:i + 4]:
                if n2 == "recall_transcript" and a2.get("session_id"):
                    key = (args["query"], a2["session_id"])
                    if key not in seen:
                        seen.add(key)
                        pairs.append({"query": args["query"],
                                      "chose": a2["session_id"],
                                      "repo": args.get("repo")})
                    break
    return pairs


def measure(pairs, index_path, limit=15, binary=None):
    """Run the real queries. `binary` matters as much as `index_path`: a change
    to how a query is executed lives in the binary, and comparing two indexes
    with one binary cannot see it at all."""
    env = dict(os.environ)
    env["RECALL_INDEX"] = index_path
    exe = binary or BIN
    ranks = []
    for p in pairs:
        argv = [exe, p["query"], "--json", "--limit", str(limit)]
        if p.get("repo"):
            argv += ["--repo", p["repo"]]
        try:
            r = subprocess.run(argv, capture_output=True, env=env, timeout=60)
            ids = [h.get("session_id") for h in json.loads(r.stdout or "[]")]
        except Exception:
            ranks.append(None)
            continue
        ranks.append(ids.index(p["chose"]) + 1 if p["chose"] in ids else None)
    hit = [r for r in ranks if r]
    n = len(ranks)
    return {
        "pairs": n,
        "found": len(hit),
        "found_pct": round(100.0 * len(hit) / n, 1) if n else 0.0,
        "rank_at_1": sum(1 for r in hit if r == 1),
        "rank_at_3": sum(1 for r in hit if r <= 3),
        "mrr": round(sum(1.0 / r for r in hit) / n, 4) if n else 0.0,
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--index", default=os.path.expanduser("~/.recall/bench.sqlite"))
    ap.add_argument("--compare", help="second index; prints an A/B")
    ap.add_argument("--bin", help="binary for --index (defaults to ./recall)")
    ap.add_argument("--compare-bin", help="binary for --compare")
    ap.add_argument("--sessions", default=os.path.expanduser("~/.pi/agent/sessions"))
    ap.add_argument("--refresh", action="store_true", help="re-extract pairs from history")
    ap.add_argument("--json", action="store_true")
    a = ap.parse_args()

    if a.refresh or not os.path.exists(PAIRS):
        pairs = extract_pairs(a.sessions)
        json.dump(pairs, open(PAIRS, "w"), indent=1, sort_keys=True)
    else:
        pairs = json.load(open(PAIRS))
    if not pairs:
        print("no query->read pairs found", file=sys.stderr)
        return 1

    base = measure(pairs, a.index, binary=a.bin)
    if not a.compare:
        print(json.dumps(base) if a.json else
              f"  {a.index}\n  found {base['found']}/{base['pairs']} "
              f"({base['found_pct']}%)  rank@1 {base['rank_at_1']}  "
              f"rank@3 {base['rank_at_3']}  MRR {base['mrr']}")
        return 0

    other = measure(pairs, a.compare, binary=a.compare_bin or a.bin)
    if a.json:
        print(json.dumps({"a": base, "b": other}))
        return 0
    print(f"  {'':22} {'A':>10} {'B':>10} {'delta':>9}")
    for k in ("found", "rank_at_1", "rank_at_3", "mrr"):
        d = other[k] - base[k]
        print(f"  {k:<22} {base[k]:>10} {other[k]:>10} {d:>+9.4g}")
    print(f"\n  A = {a.index}\n  B = {a.compare}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
