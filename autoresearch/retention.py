#!/usr/bin/env python3
"""Did the token savings cost any signal?

The utility guard is structural: it checks that markers and ids are still
present. It cannot tell whether the *content an agent needed* survived. This
compares, call for call, what the pre-optimisation binary returned against what
the current one returns, over the real workload.

The distinction that matters:

  lost       user/assistant prose present in the old output and absent from the
             new one, with no pointer to it. This is the number that must be 0.
  paged      content the new output deliberately defers behind an explicit
             continuation ("Continue with range=", "elided", outline note).
             Recoverable in one more call, so not lost.
  tool       tool-result bytes dropped by design; recoverable via tool_chars=0.

Outline calls are excluded: old and new are different renderings by intent, and
navigability is what the guard checks there.

Usage: autoresearch/retention.py [--old /tmp/recall-base] [--new ./recall] [--split dev]
"""
import argparse
import json
import os
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(ROOT))
from replay import to_argv  # noqa: E402

MSG_RE = re.compile(r"^## msg (\d+)/\d+ (\w+)\s*$", re.M)
DEFER_MARKERS = ("Continue with range=", "elided", "defaulting to outline",
                 "outline truncated at msg", "stopped at msg")


def messages(out):
    """Split rendered output into {(idx, role): body}."""
    parts = list(MSG_RE.finditer(out))
    res = {}
    for i, m in enumerate(parts):
        end = parts[i + 1].start() if i + 1 < len(parts) else len(out)
        res[(int(m.group(1)), m.group(2))] = out[m.end():end].strip()
    return res


def norm(s):
    return " ".join(s.split())


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--old", default="/tmp/recall-base")
    ap.add_argument("--new", default=str(ROOT.parent / "recall"))
    ap.add_argument("--split", default="dev")
    ap.add_argument("--json", action="store_true")
    a = ap.parse_args()

    env = dict(os.environ)
    env.setdefault("RECALL_INDEX", os.path.expanduser("~/.recall/bench.sqlite"))

    workload = [c for c in json.load(open(ROOT / "agent_workload.json"))
                if c["tool"] == "recall_transcript" and not c["args"].get("outline")
                and (a.split == "all" or c["split"] == a.split)]

    kept = lost = paged = tool_dropped = 0
    lost_examples = []
    for call in workload:
        argv = to_argv(call)
        old = subprocess.run([a.old] + argv, capture_output=True, env=env).stdout.decode("utf-8", "replace")
        new = subprocess.run([a.new] + argv, capture_output=True, env=env).stdout.decode("utf-8", "replace")
        om, nm = messages(old), messages(new)
        defers = any(d in new for d in DEFER_MARKERS)

        for key, obody in om.items():
            idx, role = key
            is_tool = role.lower().startswith("tool")
            nbody = nm.get(key)
            if nbody is None:
                # Absent entirely. Deferred behind a continuation, or gone?
                if is_tool:
                    tool_dropped += len(obody)
                elif defers:
                    paged += len(obody)
                else:
                    lost += len(obody)
                    if len(lost_examples) < 5:
                        lost_examples.append((call["args"], idx, role, norm(obody)[:90]))
                continue
            if is_tool:
                tool_dropped += max(0, len(obody) - len(nbody))
                continue
            on, nn = norm(obody), norm(nbody)
            if on == nn or on in nn:
                kept += len(obody)
            elif nn and (on.startswith(nn[:200]) or any(d in nbody for d in DEFER_MARKERS)):
                paged += len(obody) - len(nbody)
                kept += len(nbody)
            else:
                lost += len(obody) - len(nbody) if len(obody) > len(nbody) else 0
                kept += min(len(obody), len(nbody))
                if len(obody) > len(nbody) and len(lost_examples) < 5:
                    lost_examples.append((call["args"], idx, role, norm(obody)[:90]))

    total_prose = kept + lost + paged
    out = {
        "calls": len(workload),
        "prose_kept": kept,
        "prose_paged": paged,
        "prose_lost": lost,
        "tool_chars_dropped": tool_dropped,
        "prose_retention_pct": round(100 * kept / total_prose, 2) if total_prose else 100.0,
    }
    if a.json:
        print(json.dumps(out))
    else:
        for k, v in out.items():
            print(f"{k:<22} {v}")
        if lost_examples:
            print("\nprose with no pointer to it (must be empty):")
            for args, idx, role, txt in lost_examples:
                print(f"  msg {idx} {role} | {args.get('range')} | {txt}")
    return 1 if lost else 0


if __name__ == "__main__":
    sys.exit(main())
