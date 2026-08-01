#!/usr/bin/env python3
"""How often did search put the right session first?

Ground truth is the agent's own behaviour: for every real recall_search, the
session it went on to *read* is the one it judged relevant.

Rank is read out of the results the agent ACTUALLY saw, recovered from the
tool-result text in the session logs — not by re-running the query. Re-running
looks easier and is wrong: the corpus has grown since, the replay cannot
reproduce every filter, and the first version of this script reported 9.4%
rank@1 when the faithful answer is 46%. It was measuring its own drift.

Usage: autoresearch/ranking.py [--sessions ~/.pi/agent/sessions] [--json]
"""
import argparse
import glob
import json
import os
import re
import statistics
import sys

ID_RE = re.compile(r"id=([\w-]+:[\w-]+)")


def collect(root):
    """Yield (ranked_ids_seen, chosen_session_id) for each search->read pair."""
    for path in glob.glob(os.path.join(root, "**", "*.jsonl"), recursive=True):
        events = []
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
                            events.append(("call", p["name"], p.get("arguments") or {}, p.get("id")))
                elif m.get("role") == "toolResult":
                    c = m.get("content")
                    txt = c if isinstance(c, str) else "".join(
                        x.get("text", "") for x in c if isinstance(x, dict)
                    ) if isinstance(c, list) else ""
                    events.append(("res", m.get("toolName", ""), txt, m.get("toolCallId")))

        results = {e[3]: e[2] for e in events if e[0] == "res"}
        calls = [e for e in events if e[0] == "call"]
        for i, (_, name, _args, cid) in enumerate(calls):
            if name != "recall_search":
                continue
            ids = ID_RE.findall(results.get(cid, ""))
            if not ids:
                continue
            for _, n2, a2, _ in calls[i + 1:i + 4]:
                if n2 == "recall_transcript" and a2.get("session_id"):
                    yield ids, a2["session_id"]
                    break


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--sessions", default=os.path.expanduser("~/.pi/agent/sessions"))
    ap.add_argument("--json", action="store_true")
    a = ap.parse_args()

    ranks, elsewhere = [], 0
    for ids, chose in collect(a.sessions):
        if chose in ids:
            ranks.append(ids.index(chose) + 1)
        else:
            elsewhere += 1

    total = len(ranks) + elsewhere
    if not total:
        print("no search->read pairs found", file=sys.stderr)
        return 1
    out = {
        "pairs": total,
        "rank_at_1_pct": round(100 * sum(1 for r in ranks if r == 1) / total, 1),
        "rank_at_3_pct": round(100 * sum(1 for r in ranks if r <= 3) / total, 1),
        # Read a session the results did not contain: found via recall_sessions,
        # an earlier search, or an id already in hand. Not a ranking failure.
        "chose_outside_results_pct": round(100 * elsewhere / total, 1),
        "median_rank": statistics.median(ranks) if ranks else None,
        "mrr": round(sum(1 / r for r in ranks) / total, 3),
    }
    print(json.dumps(out) if a.json else
          "\n".join(f"{k:<26} {v}" for k, v in out.items()))
    return 0


if __name__ == "__main__":
    sys.exit(main())
