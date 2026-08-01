#!/usr/bin/env python3
"""Build a benchmark workload from YOUR OWN agent history.

The workload is every recall_search / recall_transcript call your agents have
actually made, with the arguments they passed. It is deliberately NOT committed:
those queries are your private work — ticket ids, service names, whatever you
were debugging — and this repo is public.

Run this once before benchmarking:

    autoresearch/extract_workload.py            # -> autoresearch/agent_workload.json
    autoresearch/replay.py --split dev --json

Sessions are split dev/holdout 75/25 by a hash of the call, so the split is
stable across runs and a change cannot be tuned against the holdout.
"""
import glob
import hashlib
import json
import os
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parent


def calls_from(root):
    for path in glob.glob(os.path.join(root, "**", "*.jsonl"), recursive=True):
        pending = {}
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
                            pending[p.get("id")] = (p["name"], p.get("arguments") or {})
                elif m.get("role") == "toolResult" and m.get("toolCallId") in pending:
                    name, args = pending.pop(m["toolCallId"])
                    c = m.get("content")
                    text = c if isinstance(c, str) else "".join(
                        x.get("text", "") for x in c if isinstance(x, dict)
                    ) if isinstance(c, list) else ""
                    yield {"tool": name, "args": args, "observed_chars": len(text)}


def main():
    root = sys.argv[1] if len(sys.argv) > 1 else os.path.expanduser("~/.pi/agent/sessions")
    out = [c for c in calls_from(root) if c["tool"] in ("recall_search", "recall_transcript")]
    for c in out:
        h = hashlib.sha256(json.dumps(c["args"], sort_keys=True).encode()).hexdigest()
        c["split"] = "holdout" if int(h[:2], 16) < 64 else "dev"
    dst = ROOT / "agent_workload.json"
    json.dump(out, open(dst, "w"), indent=1, sort_keys=True)
    dev = sum(1 for c in out if c["split"] == "dev")
    print(f"{dst}: {len(out)} calls (dev={dev} holdout={len(out)-dev})")
    if not out:
        print("no recall calls found — point this at your agent session directory",
              file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
