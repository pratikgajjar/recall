#!/usr/bin/env python3
"""Replay the real agent workload against a recall binary and measure output size.

The workload in agent_workload.json is not synthetic: it is every recall_search
and recall_transcript call real agents made in this machine's pi sessions,
extracted with the arguments they actually passed. The metric is the number of
characters an agent would have to read back — that is what costs tokens.

Usage:  autoresearch/replay.py [--bin ./recall] [--split dev|holdout|all] [--json]
"""
import argparse
import json
import os
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent


def to_argv(call):
    """Map an MCP-shaped call onto the equivalent CLI invocation.

    The MCP server and the CLI share the same core, so CLI output size is a
    faithful proxy for what the MCP tool hands back to the model.
    """
    a, tool = call["args"], call["tool"]
    if tool == "recall_search":
        argv = [a.get("query", "")]
        if a.get("repo"):
            argv += ["--repo", a["repo"]]
        if a.get("source"):
            argv += ["--tag", "source:" + a["source"]]
        if a.get("since"):
            argv += ["--since", a["since"]]
        if a.get("session_id"):
            argv += ["--in", a["session_id"]]
        for t in a.get("tags") or []:
            argv += ["--tag", t]
        argv += ["--limit", str(a.get("limit") or 15)]
        return argv
    # Unsliced calls need no special handling: applyBigSessionCap now lives in
    # the shared transcript path, so the CLI reproduces what MCP handed back.
    argv = ["show", a["session_id"]]
    if a.get("range"):
        argv += ["--range", a["range"]]
    if a.get("role"):
        argv += ["--role", a["role"]]
    if a.get("outline"):
        argv += ["--outline"]
    return argv


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--bin", default=str(ROOT.parent / "recall"))
    ap.add_argument("--split", default="dev", choices=["dev", "holdout", "all"])
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args()

    workload = json.load(open(ROOT / "agent_workload.json"))
    if args.split != "all":
        workload = [c for c in workload if c["split"] == args.split]

    env = dict(os.environ)
    # Frozen corpus (see autoresearch/checks.sh) so runs stay comparable.
    env.setdefault("RECALL_INDEX", os.path.expanduser("~/.recall/bench.sqlite"))
    totals, per_tool, failures, rejected = 0, {}, 0, 0
    # outline is tracked separately: it is the mode that claims to be cheap.
    outline_chars = 0
    for call in workload:
        try:
            # Bytes, not text=True: some transcripts carry invalid UTF-8 and a
            # decode error here would silently drop the call from the total.
            r = subprocess.run([args.bin] + to_argv(call), capture_output=True,
                               timeout=120, env=env)
            n = len(r.stdout.decode("utf-8", "replace"))
            if r.returncode != 0:
                # A rejected call is a real cost (the agent must retry), so its
                # output still counts; it just is not a harness failure.
                rejected += 1
                n += len(r.stderr.decode("utf-8", "replace"))
        except Exception:
            failures += 1
            n = 0
        totals += n
        per_tool[call["tool"]] = per_tool.get(call["tool"], 0) + n
        if call["tool"] == "recall_transcript" and call["args"].get("outline"):
            outline_chars += n

    # There is no tool schema any more: recall registers no tools, so nothing
    # is re-sent every turn. What an agent must read to use it is the skill
    # file, loaded once when the task calls for it. Same question — what does
    # the instruction surface cost — asked of the surface that now exists.
    skill_chars = 0
    skill = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                         "skills", "recall", "SKILL.md")
    try:
        skill_chars = len(open(skill, encoding="utf-8").read())
    except OSError:
        pass

    out = {
        "split": args.split,
        "calls": len(workload),
        "total_chars": totals,
        "total_kch": round(totals / 1000, 1),
        "est_tokens": totals // 4,
        "outline_chars": outline_chars,
        "rejected_calls": rejected,
        "failures": failures,
        "skill_chars": skill_chars,
        **{"chars_" + k.replace("recall_", ""): v for k, v in per_tool.items()},
    }
    if args.json:
        print(json.dumps(out))
    else:
        for k, v in out.items():
            print(f"{k:<22} {v}")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
