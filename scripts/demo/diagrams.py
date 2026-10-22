#!/usr/bin/env python3
"""Write the animated README diagrams.

Usage: diagrams.py <out-dir>

Writes flow.svg (how recall works) and plugins.svg (index anything). The SVGs
use SMIL animation, so they play inside a GitHub README <img> with no script.
One cycle is CYCLE seconds. Every animation starts at 0 and places its step
with keyTimes, so the steps stay in order on each repeat.
"""

import os
import sys

CYCLE = 8
BG, CARD, LINE, TEXT, DIM, HOT = "#0d1117", "#161b22", "#30363d", "#e6edf3", "#8b949e", "#e3b341"
MONO = "ui-monospace,SFMono-Regular,Menlo,Consolas,monospace"
SANS = "-apple-system,BlinkMacSystemFont,Segoe UI,Helvetica,Arial,sans-serif"

SOURCES = [
    ("pi", "~/.pi/agent/sessions", "#7ee787"),
    ("Claude Code", "~/.claude/projects", "#f0a46c"),
    ("Codex", "~/.codex/sessions", "#79c0ff"),
    ("Cursor", "state.vscdb, read-only", "#d2a8ff"),
    ("Lua plugins", "~/.recall/plugins", "#8b949e"),
]


def kt(*t: float) -> str:
    return ";".join(f"{x:.3f}" for x in t)


def dot(path: str, colour: str, s: float, e: float, r: float = 5) -> str:
    """A dot that moves along path from cycle fraction s to e."""
    return f"""<circle class="dot" r="{r}" fill="{colour}" opacity="0">
  <animateMotion dur="{CYCLE}s" repeatCount="indefinite" path="{path}"
    calcMode="linear" keyPoints="0;0;1;1" keyTimes="{kt(0, s, e, 1)}"/>
  <animate attributeName="opacity" dur="{CYCLE}s" repeatCount="indefinite"
    values="0;0;1;1;0;0" keyTimes="{kt(0, s - .001, s, e, e + .001, 1)}"/>
</circle>"""


def pulse(s: float, e: float) -> str:
    """Highlight a box border from s to e."""
    return f"""<animate attributeName="stroke" dur="{CYCLE}s" repeatCount="indefinite"
    values="{LINE};{LINE};{HOT};{HOT};{LINE};{LINE}" keyTimes="{kt(0, s, s + .01, e, e + .01, 1)}"/>"""


def box(x, y, w, h, title, lines, s=None, e=None, title_colour=TEXT) -> str:
    anim = pulse(s, e) if s is not None else ""
    out = [f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="8" fill="{CARD}" stroke="{LINE}" stroke-width="1.5">{anim}</rect>',
           f'<text x="{x + 14}" y="{y + 24}" fill="{title_colour}" font-family="{MONO}" font-size="14" font-weight="700">{title}</text>']
    for i, l in enumerate(lines):
        out.append(f'<text x="{x + 14}" y="{y + 44 + 18 * i}" fill="{DIM}" font-family="{SANS}" font-size="12.5">{l}</text>')
    return "\n".join(out)


def caption(text: str, s: float, e: float, y: int = 452) -> str:
    return f"""<text x="480" y="{y}" text-anchor="middle" fill="{TEXT}" font-family="{SANS}" font-size="15" opacity="0">{text}
  <animate attributeName="opacity" dur="{CYCLE}s" repeatCount="indefinite"
    values="0;0;1;1;0;0" keyTimes="{kt(0, s, s + .02, e - .02, e, 1)}"/>
</text>"""


def flow() -> str:
    parts = []
    # Sources.
    paths_in = []
    for i, (name, where, colour) in enumerate(SOURCES):
        y = 64 + 58 * i
        parts.append(box(24, y, 196, 46, name, [], title_colour=colour))
        parts.append(f'<text x="38" y="{y + 39}" fill="{DIM}" font-family="{MONO}" font-size="11">{where}</text>')
        p = f"M220,{y + 23} C262,{y + 23} 258,210 300,210"
        paths_in.append((p, colour))
        parts.append(f'<path d="{p}" fill="none" stroke="{LINE}" stroke-width="1.5"/>')

    # recall index, the SQLite file, and the two read commands.
    parts.append(box(300, 148, 200, 124, "recall index",
                     ["reads only new lines", "one writer at a time", "compacts stale rows"], .30, .42))
    parts.append(f'<path d="M500,210 L560,210" fill="none" stroke="{LINE}" stroke-width="1.5"/>')
    parts.append(f"""<path d="M560,166 v88 a80,14 0 0 0 160,0 v-88" fill="{CARD}" stroke="{LINE}" stroke-width="1.5">{pulse(.46, .56)}</path>
<ellipse cx="640" cy="166" rx="80" ry="14" fill="{CARD}" stroke="{LINE}" stroke-width="1.5">{pulse(.46, .56)}</ellipse>
<text x="640" y="206" text-anchor="middle" fill="{TEXT}" font-family="{MONO}" font-size="14" font-weight="700">index.sqlite</text>
<text x="640" y="226" text-anchor="middle" fill="{DIM}" font-family="{SANS}" font-size="12.5">FTS5 + BM25</text>
<text x="640" y="244" text-anchor="middle" fill="{DIM}" font-family="{SANS}" font-size="12.5">text for search only</text>""")
    to_find, to_show = "M720,198 C752,198 748,136 780,136", "M720,222 C752,222 748,284 780,284"
    for p in (to_find, to_show):
        parts.append(f'<path d="{p}" fill="none" stroke="{LINE}" stroke-width="1.5"/>')
    parts.append(box(780, 108, 156, 58, "recall find", ["ranked hits, ~40 ms"], .58, .70))
    parts.append(box(780, 256, 156, 58, "recall show", ["full session text"], .72, .98))
    parts.append(f'<text x="858" y="92" text-anchor="middle" fill="{DIM}" font-family="{SANS}" font-size="12.5">you or your agent</text>')

    # show reads the session again from its source: the index holds no copy.
    back = "M858,314 L858,392 L122,392 L122,342"
    parts.append(f'<path d="{back}" fill="none" stroke="{LINE}" stroke-width="1.5" stroke-dasharray="5 5"/>')
    parts.append(f'<text x="490" y="384" text-anchor="middle" fill="{DIM}" font-family="{SANS}" font-size="12.5">show reads the session again from its source. The index keeps only search text.</text>')

    # Moving dots, in step order.
    for i, (p, colour) in enumerate(paths_in):
        s = .02 + .05 * i
        parts.append(dot(p, colour, s, s + .14))
    parts.append(dot("M500,210 L560,210", HOT, .42, .47))
    parts.append(dot(to_find, HOT, .56, .63))
    parts.append(dot(to_show, HOT, .70, .76))
    parts.append(dot(back, HOT, .76, .96))

    parts.append(caption("1. recall index reads only the new lines of each session file", 0, .40))
    parts.append(caption("2. one writer adds them to a local SQLite full-text index", .40, .56))
    parts.append(caption("3. recall find ranks the hits in about 40 ms", .56, .72))
    parts.append(caption("4. recall show reads the full session from its source", .72, 1))

    return svg(476, "How recall works",
               "recall index reads new lines from pi, Claude Code, Codex, Cursor and Lua plugin sources"
               " into a local SQLite FTS5 index. recall find searches the index. recall show reads the"
               " full session again from its source.", parts)


def svg(height: int, title: str, desc: str, parts: list[str]) -> str:
    return f"""<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 960 {height}" width="960" height="{height}" role="img" aria-labelledby="t d">
<title id="t">{title}</title>
<desc id="d">{desc}</desc>
<style>@media (prefers-reduced-motion: reduce) {{ .dot {{ display: none }} }}</style>
<rect width="960" height="{height}" rx="12" fill="{BG}"/>
<text x="24" y="38" fill="{TEXT}" font-family="{SANS}" font-size="17" font-weight="600">{title}</text>
{chr(10).join(parts)}
</svg>
"""


def record(path: str, s: float, e: float) -> str:
    """A small record card that moves along path from s to e."""
    return f"""<g class="dot" opacity="0">
  <rect x="-13" y="-8" width="26" height="16" rx="3" fill="{HOT}"/>
  <path d="M-8,-3 h16 M-8,2 h11" stroke="{BG}" stroke-width="1.5"/>
  <animateMotion dur="{CYCLE}s" repeatCount="indefinite" path="{path}"
    calcMode="linear" keyPoints="0;0;1;1" keyTimes="{kt(0, s, e, 1)}"/>
  <animate attributeName="opacity" dur="{CYCLE}s" repeatCount="indefinite"
    values="0;0;1;1;0;0" keyTimes="{kt(0, s - .001, s, e, e + .001, 1)}"/>
</g>"""


KINDS = [
    ("JSONL log", 'kind = "line"', "chats, event logs", "#7ee787"),
    ("Markdown, text", 'kind = "file"', "notes, memory, skills", "#79c0ff"),
    ("SQLite key/value", 'kind = "kv"', "Cursor-style stores", "#d2a8ff"),
]
FEATURES = ["incremental scan", "BM25 search", "show, related", "tags, facets", "stats, --json"]


def plugins() -> str:
    parts = []
    code = ["file = function(doc)", "  return session, {", '    { role = "note",', "      text = doc.text } }", "end"]
    # Sources, one for each plugin kind.
    for i, (name, kind, use, colour) in enumerate(KINDS):
        y = 64 + 86 * i
        parts.append(box(24, y, 200, 70, name, [], title_colour=colour))
        parts.append(f'<text x="38" y="{y + 44}" fill="{TEXT}" font-family="{MONO}" font-size="12">{kind}</text>')
        parts.append(f'<text x="38" y="{y + 61}" fill="{DIM}" font-family="{SANS}" font-size="12">{use}</text>')
        p = f"M224,{y + 35} C258,{y + 35} 256,186 290,186"
        parts.append(f'<path d="{p}" fill="none" stroke="{LINE}" stroke-width="1.5"/>')
        s = .02 + .06 * i
        parts.append(dot(p, colour, s, s + .14))

    # The plugin: a pure function from bytes to records.
    parts.append(box(290, 88, 220, 196, "plugin.lua", [], .26, .42))
    for i, l in enumerate(code):
        parts.append(f'<text x="304" y="{134 + 18 * i}" fill="{TEXT}" font-family="{MONO}" font-size="12" xml:space="preserve">{l}</text>')
    for i, l in enumerate(["pure transform: no file,", "process or network access"]):
        parts.append(f'<text x="304" y="{240 + 18 * i}" fill="{DIM}" font-family="{SANS}" font-size="12.5">{l}</text>')

    # The contract that every source produces.
    parts.append(f'<path d="M510,186 L540,186" fill="none" stroke="{LINE}" stroke-width="1.5"/>')
    parts.append(box(540, 126, 190, 120, "one contract",
                     ["Session: id, title, time", "Message: role, ts, text", "checkpoint: resume point"],
                     .50, .62))
    parts.append(record("M510,186 L540,186", .42, .50))

    # What recall gives every source.
    for i, f in enumerate(FEATURES):
        y = 64 + 50 * i
        s = .66 + .05 * i
        parts.append(box(766, y, 170, 40, f, [], s, .96))
        p = f"M730,186 C750,186 746,{y + 20} 766,{y + 20}"
        parts.append(f'<path d="{p}" fill="none" stroke="{LINE}" stroke-width="1.5"/>')
        parts.append(dot(p, HOT, .60, s, 4))

    parts.append(f'<text x="24" y="342" fill="{DIM}" font-family="{SANS}" font-size="12.5">recall does all of the I/O: it walks files, reads only new lines, and stores the checkpoint.</text>')
    parts.append(f'<text x="24" y="362" fill="{DIM}" font-family="{SANS}" font-size="12.5">The built-in Go adapters for pi, Claude Code, Codex and Cursor make the same contract. '
                 f'recall memory and recall skill are Lua plugins.</text>')
    parts.append(caption("1. recall reads the bytes and keeps the checkpoint", 0, .42, 412))
    parts.append(caption("2. the plugin changes the bytes into sessions and messages", .42, .62, 412))
    parts.append(caption("3. every source gets the same BM25 index and the same commands", .62, 1, 412))
    return svg(436, "Index anything: one contract, BM25 search for each source",
               "A Lua plugin of kind line, file or kv changes JSONL, Markdown or SQLite bytes into sessions"
               " and messages. recall does all I/O and gives every source incremental scans, BM25 search,"
               " show, related, tags, facets, stats and JSON output.", parts)


def main() -> None:
    out = sys.argv[1]
    for name, make in (("flow.svg", flow), ("plugins.svg", plugins)):
        with open(os.path.join(out, name), "w") as f:
            f.write(make())


if __name__ == "__main__":
    main()
