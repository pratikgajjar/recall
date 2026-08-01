#!/usr/bin/env python3
"""Reliability audit: is everything on disk actually in the index, and findable?

This is deliberately written WITHOUT using recall's own parsing code. It reads
the raw session files itself and compares against what the index holds. An audit
that shares code with the thing it audits cannot detect the bug where that code
drops data — which is exactly the failure that has happened here twice: codex
dropped 2,336 of 2,336 lines behind a decode error, and claude tool calls were
unattributed for three rounds because the local corpus did not exercise them.

Three questions, in increasing order of strength:

  1. coverage_sessions  Does every session file on disk appear in the index?
  2. coverage_messages  Does the index hold the user+assistant prose that is on
                        disk? Tool results are an editorial choice, but a human
                        turn or a model reply going missing is data loss.
  3. findability        Take a distinctive real sentence out of a session on
                        disk and search for it. Does that session come back?
                        This is end-to-end: ingestion, text normalisation, FTS
                        and ranking all have to be right, and it cannot be
                        satisfied by making the counters agree.

Usage:
    autoresearch/audit.py --json
    autoresearch/audit.py --sample 300      # findability sample size
"""
import argparse
import collections
import glob
import json
import os
import random
import re
import sqlite3
import subprocess
import sys

HOME = os.path.expanduser("~")
HERE_DIR = os.path.dirname(os.path.abspath(__file__))
BIN = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "recall")

JSONL_SOURCES = {
    "pi": "~/.pi/agent/sessions",
    "claude": "~/.claude/projects",
    "codex": "~/.codex/sessions",
}


UUID_RE = re.compile(
    r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}", re.I)


def session_id_from(path, head):
    """The id a session declares about itself, not the one its filename implies.

    These disagree: a resumed or forked session keeps writing to the file it
    started in, so the name can carry an id that belongs to an earlier session.
    The declared id is authoritative — reading the filename instead reports
    perfectly-indexed sessions as missing.
    """
    for line in head:
        try:
            d = json.loads(line)
        except Exception:
            continue
        got = d.get("id") or d.get("sessionId") or (d.get("payload") or {}).get("id")
        if isinstance(got, str) and UUID_RE.fullmatch(got):
            return got
    base = os.path.splitext(os.path.basename(path))[0]
    m = UUID_RE.findall(base)
    return m[-1] if m else base


def excerpt_cap():
    """How much of a message recall can actually index, read from its source.

    The opening goes in as-is up to excerptMax; everything after it is windowed
    into the discounted `tail` column, up to maxChunks windows. So the reachable
    total is the product, not excerptMax. Hardcoding either would let this drift
    from the code it audits, which is the one thing this file must not do.

    Verified empirically, not assumed: a phrase drawn from past 3,000 characters
    of a long message retrieves its own session 20 times in 25, against 12 in 25
    when the tail was truncated away.
    """
    try:
        src = open(os.path.join(os.path.dirname(HERE_DIR), "types.go")).read()
        cap = re.search(r"excerptMax\s*=\s*(\d+)", src)
        chunks = re.search(r"maxChunks\s*=\s*(\d+)", src)
        if cap and chunks:
            return int(cap.group(1)) * (int(chunks.group(1)) + 1)
        if cap:
            return int(cap.group(1))
    except OSError:
        pass
    return 1500


def prose_parts(c):
    """Content field -> the text segments recall will index as separate rows."""
    if isinstance(c, str):
        return [c]
    if isinstance(c, list):
        out = []
        for p in c:
            if isinstance(p, str):
                out.append(p)
            elif isinstance(p, dict) and isinstance(p.get("text"), str):
                out.append(p["text"])
        return out
    return []


def prose_from_content(c):
    """Flatten a message content field to text, tolerating every shape."""
    return "\n".join(prose_parts(c))


def disk_sessions(cap):
    """Every session on disk -> {source_id: [prose strings]}, counted raw.

    Only user and assistant turns. Those are the irreducible content: whatever
    recall chooses to do with tool output, losing a human question or a model
    answer is data loss.
    """
    out = {}
    total_chars = kept_chars = truncated_parts = total_parts = 0
    for source, root in JSONL_SOURCES.items():
        for path in glob.glob(os.path.expanduser(root) + "/**/*.jsonl", recursive=True):
            prose = []
            try:
                lines = open(path, errors="replace").read().splitlines()
            except OSError:
                continue
            sid = session_id_from(path, lines[:6])
            if True:
                for line in lines:
                    if '"role"' not in line and '"payload"' not in line:
                        continue
                    try:
                        d = json.loads(line)
                    except Exception:
                        continue
                    m = d.get("message")
                    if isinstance(m, dict) and m.get("role") in ("user", "assistant"):
                        for seg in prose_parts(m.get("content")):
                            if not seg.strip():
                                continue
                            total_parts += 1
                            total_chars += len(seg)
                            kept_chars += min(len(seg), cap)
                            if len(seg) > cap:
                                truncated_parts += 1
                            prose.append(seg)
                        continue
                    # codex: the conversation is in event_msg. The parallel
                    # response_item/message/user stream is the injected
                    # AGENTS.md preamble, which recall deliberately skips and
                    # should skip — it is identical across hundreds of sessions
                    # and would drown real prose in search.
                    p = d.get("payload")
                    if isinstance(p, dict) and p.get("type") in ("user_message", "agent_message"):
                        t = p.get("message")
                        if isinstance(t, str) and t.strip():
                            total_parts += 1
                            total_chars += len(t)
                            kept_chars += min(len(t), cap)
                            if len(t) > cap:
                                truncated_parts += 1
                            prose.append(t)
            if prose:
                out[(source, sid)] = prose
    return out, {
        "prose_chars": total_chars,
        "searchable_chars": kept_chars,
        "searchable_prose_pct": round(100.0 * kept_chars / total_chars, 2) if total_chars else 100.0,
        "parts_truncated": truncated_parts,
        "parts_total": total_parts,
    }


def index_state(db_path):
    """What the index believes: sessions, per-session message counts, dupes."""
    con = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    sessions = {}
    for source, sid, n in con.execute(
            "SELECT source, source_id, COALESCE(msg_count,0) FROM sessions"):
        sessions[(source, sid)] = n
    # Messages live only in the FTS table; session_pk holds the session id, so
    # the source is its prefix.
    roles = collections.Counter()
    for pk, role, n in con.execute(
            "SELECT session_pk, role, COUNT(*) FROM messages_fts GROUP BY 1,2"):
        roles[(str(pk).split(":", 1)[0], role)] += n
    # Prose characters actually indexed, per session. A global count cannot
    # detect loss: the index legitimately holds more messages than there is
    # prose on disk, because a turn that only calls a tool still gets a row.
    # Only a per-session comparison catches "session present, half of it gone".
    # Count the tail column too where the schema has one: a windowed row holds
    # its text there and nothing in `text`, so summing only `text` would report
    # every long message as mostly lost.
    cols = [r[1] for r in con.execute("PRAGMA table_info(messages_fts)")]
    expr = "SUM(LENGTH(text) + LENGTH(tail))" if "tail" in cols else "SUM(LENGTH(text))"
    chars = {}
    for pk, n in con.execute(
            f"""SELECT session_pk, {expr} FROM messages_fts
                WHERE role IN ('user','assistant') GROUP BY 1"""):
        chars[str(pk)] = n or 0
    # A message duplicated in the FTS table is billed twice and read twice.
    # A message legitimately occupies several rows now — one for its opening and
    # one per tail window — so identity is the content, not (session, idx).
    dupe_cols = "session_pk, idx, text, tail" if "tail" in cols else "session_pk, idx, text"
    dupes = con.execute(
        f"""SELECT COALESCE(SUM(c - 1), 0) FROM (SELECT {dupe_cols}, COUNT(*) c
            FROM messages_fts GROUP BY 1,2,3{',4' if 'tail' in cols else ''} HAVING c > 1)""").fetchone()[0]
    total = con.execute("SELECT COUNT(*) FROM messages_fts").fetchone()[0]
    con.close()
    return sessions, roles, dupes, total, chars


def candidate_phrases(text, limit=5):
    """Phrases from real prose that could plausibly identify a session."""
    out = []
    for line in text.splitlines():
        line = line.strip()
        if len(line) < 40 or len(line) > 200:
            continue
        if any(ch in line for ch in "{}<>|/\\`$#"):
            continue
        words = re.findall(r"[A-Za-z][A-Za-z'-]{2,}", line)
        if len(words) >= 6:
            out.append(" ".join(words[:8]))
        if len(out) >= limit:
            break
    return out


def doc_freq(con, phrase):
    """How many sessions contain this phrase, straight from FTS."""
    q = '"' + phrase.replace('"', "") + '"'
    try:
        return con.execute(
            "SELECT COUNT(DISTINCT session_pk) FROM messages_fts WHERE messages_fts MATCH ?",
            (q,)).fetchone()[0]
    except sqlite3.OperationalError:
        return -1


# A phrase in more sessions than this identifies none of them: it is a system
# prompt or a tool banner, not something anyone would search to find one session.
BOILERPLATE_DF = 30


def findability(disk, db_path, sample, seed=7):
    """Search for a rare real sentence; expect its own session back.

    End-to-end: ingestion, text normalisation, FTS and ranking all have to be
    right, and it cannot be satisfied by making counters agree. Sessions whose
    prose is entirely boilerplate are reported separately rather than counted as
    failures — nothing can retrieve a session by text it shares with 200 others,
    and hiding that in the score would just make it noisy.
    """
    rng = random.Random(seed)
    keys = list(disk)
    rng.shuffle(keys)
    con = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    env = dict(os.environ)
    env["RECALL_INDEX"] = db_path

    found = miss = boiler = err = 0
    misses = []
    for key in keys:
        if found + miss >= sample:
            break
        source, sid = key
        cands = []
        for text in disk[key]:
            cands.extend(candidate_phrases(text))
            if len(cands) >= 5:
                break
        if not cands:
            continue
        scored = [(doc_freq(con, c), c) for c in cands[:5]]
        scored = [(d, c) for d, c in scored if d > 0]
        if not scored:
            continue
        df, phrase = min(scored)
        if df > BOILERPLATE_DF:
            boiler += 1
            continue
        want = f"{source}:{sid}"
        try:
            r = subprocess.run([BIN, phrase, "--json", "--limit", "20"],
                               capture_output=True, env=env, timeout=30)
            ids = [h.get("session_id") for h in json.loads(r.stdout or "[]")]
        except Exception:
            err += 1
            continue
        if want in ids:
            found += 1
        else:
            miss += 1
            if len(misses) < 5:
                misses.append({"want": want, "df": df, "query": phrase[:60]})
    con.close()
    n = found + miss
    return {
        "sampled": n,
        "found": found,
        "found_pct": round(100.0 * found / n, 1) if n else 0.0,
        "boilerplate_only": boiler,
        "errors": err,
        "examples_missed": misses,
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--json", action="store_true")
    ap.add_argument("--sample", type=int, default=200)
    ap.add_argument("--index", default=os.environ.get(
        "RECALL_INDEX", os.path.join(HOME, ".recall", "bench.sqlite")))
    a = ap.parse_args()

    cap = excerpt_cap()
    disk, capinfo = disk_sessions(cap)
    idx_sessions, idx_roles, dupes, total_msgs, idx_chars = index_state(a.index)

    # A session on disk with real prose must exist in the index.
    missing = [k for k in disk if k not in idx_sessions]
    # ...and one in the index whose file is gone is a stale row: searchable but
    # unreadable, which wastes a call to discover.
    stale = [k for k in idx_sessions if k not in disk and k[0] in JSONL_SOURCES]

    n_disk = len(disk)
    cov_sessions = 100.0 * (n_disk - len(missing)) / n_disk if n_disk else 100.0

    disk_prose = sum(len(v) for v in disk.values())
    idx_prose = sum(n for (src, role), n in idx_roles.items()
                    if role in ("user", "assistant") and src in JSONL_SOURCES)

    # Per-session: does the index hold the prose recall INTENDS to index for
    # this session — that is, after its own truncation cap? Comparing against
    # raw disk chars would just re-measure the cap, which is reported on its own
    # as searchable_prose_pct. This isolates the other failure: an adapter
    # dropping messages it should have kept.
    LOSS_RATIO = 0.8
    checked = lossy = 0
    worst = []
    for (source, sid), prose in disk.items():
        want = sum(min(len(t), cap) for t in prose)
        if want < 500:      # too small to judge; a title alone can satisfy it
            continue
        got = idx_chars.get(f"{source}:{sid}", 0)
        checked += 1
        if got < LOSS_RATIO * want:
            lossy += 1
            worst.append((got / want, f"{source}:{sid}", want, got))
    worst.sort()
    cov_messages = 100.0 * (checked - lossy) / checked if checked else 100.0

    find = findability(disk, a.index, a.sample)

    out = {
        "sessions_on_disk": n_disk,
        "sessions_missing": len(missing),
        "coverage_sessions_pct": round(cov_sessions, 2),
        "prose_on_disk": disk_prose,
        "prose_in_index": idx_prose,
        "sessions_checked_for_loss": checked,
        "sessions_lossy": lossy,
        "coverage_messages_pct": round(cov_messages, 2),
        "worst_loss": [{"session": w[1], "disk_chars": w[2], "indexed_chars": w[3],
                        "ratio": round(w[0], 3)} for w in worst[:5]],
        "duplicate_messages": dupes,
        "stale_sessions": len(stale),
        "total_messages": total_msgs,
        "integrity_pct": round(100.0 * (1 - (dupes + len(stale)) / max(total_msgs, 1)), 3),
        "excerpt_cap": cap,
        "searchable_prose_pct": capinfo["searchable_prose_pct"],
        "parts_truncated": capinfo["parts_truncated"],
        "parts_total": capinfo["parts_total"],
        "prose_chars": capinfo["prose_chars"],
        "findability_pct": find["found_pct"],
        "findability_detail": find,
        "examples_missing_sessions": [f"{s}:{i}" for s, i in missing[:5]],
    }
    if a.json:
        print(json.dumps(out))
    else:
        print(f"sessions on disk     {out['sessions_on_disk']}")
        print(f"  missing from index {out['sessions_missing']}  "
              f"-> coverage {out['coverage_sessions_pct']}%")
        print(f"per-session prose     {out['sessions_lossy']}/{out['sessions_checked_for_loss']} "
              f"sessions below {int(LOSS_RATIO*100)}% of their on-disk prose "
              f"-> {out['coverage_messages_pct']}% intact")
        for w in out["worst_loss"]:
            print(f"    {w['session']} {w['indexed_chars']}/{w['disk_chars']} chars "
                  f"({w['ratio']:.0%})")
        print(f"duplicates {out['duplicate_messages']}  stale {out['stale_sessions']}"
              f"  -> integrity {out['integrity_pct']}%")
        print(f"searchable prose     {out['searchable_prose_pct']}% of "
              f"{out['prose_chars']:,} chars survive excerptMax={cap} "
              f"({out['parts_truncated']:,}/{out['parts_total']:,} parts cut)")
        print(f"findability          {out['findability_pct']}% "
              f"({find['found']}/{find['sampled']} real sentences found their session)")
        print(f"  (skipped {find['boilerplate_only']} sessions whose prose is all boilerplate)")
        for m in find["examples_missed"]:
            print(f"    missed {m['want']} (df={m['df']}): {m['query']!r}")
        for s in out["examples_missing_sessions"]:
            print(f"    not indexed: {s}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
