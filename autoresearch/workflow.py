#!/usr/bin/env python3
"""Compare two ways an agent can locate a passage inside a known session.

Both strategies end with the same range read, so the difference is purely the
cost of *finding* where to read:

  A (observed)  recall_transcript(outline)          -> recall_transcript(range)
  B (available) recall_search(query, session_id=S)  -> recall_transcript(range)
  C (new)       recall_search(query, session_id=S, context=N)   -- one call

The pairs come from real sessions: every place an agent actually ran an outline
and then sliced a range on the same session. The query for B is derived from the
content the agent ended up reading, and B only counts as a win if search puts a
hit *inside* that same window — locating the wrong place cheaply is not a win.
"""
import collections
import json
import os
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent
BIN = str(ROOT.parent / "recall")
ENV = dict(os.environ)
ENV.setdefault("RECALL_INDEX", os.path.expanduser("~/.recall/bench.sqlite"))

STOP = set("""the a an and or but if then than that this these those is are was were be been being
of to in on at by for with from as it its it's you your we our they their he she i me my
what which who whom whose when where why how all any both each few more most other some such
no nor not only own same so too very can will just don should now do does did doing have has
had having would could may might must let lets like get got make made use used using need
here there about into over under again further once because while during before after above
below up down out off then once""".split())


def run(args):
    r = subprocess.run([BIN] + args, capture_output=True, env=ENV)
    return r.stdout.decode("utf-8", "replace")


def terms_from(text, n=4):
    """Pick distinctive words from the passage the agent read - a stand-in for
    the question they were holding in their head at the time."""
    words = re.findall(r"[a-zA-Z][a-zA-Z_\-]{3,}", text.lower())
    freq = collections.Counter(w for w in words if w not in STOP)
    return [w for w, _ in freq.most_common(n)]


def window(rng, total):
    """Resolve a Python-style slice to (lo, hi) the way recall does."""
    lo_s, _, hi_s = rng.partition(":")
    def val(s, d):
        s = s.strip().strip("\"'")
        if not s:
            return d
        v = int(s)
        return v if v >= 0 else max(0, total + v)
    return val(lo_s, 0), val(hi_s, total)


def build_pairs(dst, sessions=None):
    """Recover every place an agent outlined a session then sliced a range in it.

    That pattern is the navigation cost this benchmark exists to measure: two
    calls to reach one passage.
    """
    import glob
    root = sessions or os.path.expanduser("~/.pi/agent/sessions")
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
                    for part in m["content"]:
                        if isinstance(part, dict) and part.get("type") == "toolCall" \
                                and part.get("name") == "recall_transcript":
                            calls.append(part.get("arguments") or {})
        # an outline followed by a range on the same session
        for i, a in enumerate(calls):
            if not a.get("outline"):
                continue
            for b in calls[i + 1:i + 4]:
                if b.get("session_id") == a.get("session_id") and b.get("range"):
                    key = (a.get("session_id"), b["range"])
                    if key not in seen:
                        seen.add(key)
                        pairs.append({"session_id": a["session_id"], "range": b["range"]})
                    break
    json.dump(pairs, open(dst, "w"), indent=1, sort_keys=True)
    return pairs


def main():
    # Derived data belongs beside the script, not in /tmp, where it evaporates
    # and takes the benchmark with it. Regenerate when missing.
    default = os.path.join(os.path.dirname(os.path.abspath(__file__)), "nav_pairs.json")
    path = sys.argv[1] if len(sys.argv) > 1 and not sys.argv[1].startswith("-") else default
    if not os.path.exists(path):
        build_pairs(path)
    pairs = json.load(open(path))
    tot_a = tot_b = tot_c = 0
    located = usable = 0
    rows = []
    for p in pairs:
        sid, rng = p["session_id"], p["range"]
        body = run(["show", sid, "--range", rng])
        if not body.strip():
            continue
        usable += 1
        outline = run(["show", sid, "--outline"])

        q = " ".join(terms_from(body))
        found = run(["search", q, "--in", sid, "--limit", "5"]) if q else ""

        # Did search land inside the window the agent actually read?
        m = re.search(r"msgs=(\d+)", body)
        total = int(m.group(1)) if m else 0
        lo, hi = window(rng, total)
        hit = any(lo <= int(x) < hi for x in re.findall(r"msg=(\d+)", found))
        located += hit

        # C answers "find it and show me" in a single call.
        ctx_out = run(["find", q, "--in", sid, "--context", "5", "--limit", "1"]) if q else ""
        tot_a += len(outline) + len(body)
        tot_b += len(found) + len(body)
        tot_c += len(ctx_out)
        rows.append((sid[:28], rng, len(outline), len(found), len(ctx_out), hit))

    print(f"{'session':<30}{'range':>10}{'outline':>10}{'search':>9}{'ctx(1call)':>12}  located")
    for sid, rng, o, f, c, hit in rows:
        print(f"{sid:<30}{rng:>10}{o:>10,}{f:>9,}{c:>12,}  {'yes' if hit else 'no'}")
    print()
    print(f"pairs                    {usable}")
    print(f"A outline->range         {tot_a:,} chars")
    print(f"B search --in ->range    {tot_b:,} chars")
    if tot_b:
        print(f"ratio                    {tot_a / tot_b:.2f}x cheaper")
    print(f"C search --context (1 call) {tot_c:,} chars   {tot_a / tot_c:.2f}x cheaper than A" if tot_c else "")
    print(f"located in window        {located}/{usable}")
    print(json.dumps({"strategy_a_chars": tot_a, "strategy_b_chars": tot_b,
                      "strategy_c_chars": tot_c, "located": located, "pairs": usable}))


if __name__ == "__main__":
    main()
