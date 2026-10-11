#!/usr/bin/env python3
"""Render the README screenshot from real recall output.

Usage: render.py <recall-binary> <out.png>

The script writes the synthetic corpus (corpus.py) to a temporary HOME,
indexes it, runs the commands below, and screenshots the output in headless
Chromium. The text in the image is the exact command output; only colour is
added.
"""

import html
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

CHROMIUM = os.environ.get("CHROMIUM", "/Applications/Chromium.app/Contents/MacOS/Chromium")
COMMANDS = [
    ["checkout", "--limit", "3"],
    ["flaky checkout test", "--context", "1", "--limit", "1"],
]
SOURCE_COLOURS = {"pi": "#7ee787", "claude": "#f0a46c", "codex": "#79c0ff"}


def run(binary: str, home: Path, args: list[str]) -> str:
    env = dict(os.environ, HOME=str(home))
    out = subprocess.run([binary, *args], env=env, cwd=home,
                         capture_output=True, text=True, check=False)
    return out.stdout.rstrip("\n")


def colour(line: str) -> str:
    s = html.escape(line)
    s = re.sub(r"«(.+?)»", r'<span class="m">\1</span>', s)
    if m := re.match(r"^(## )?(\d{4}-\d\d-\d\d \d\d:\d\d)(\s+)(\w[\w-]*)(.*)$", s):
        hdr, ts, gap, src, rest = m.groups()
        c = SOURCE_COLOURS.get(src, "#c9d1d9")
        b = ' class="b"' if hdr else ""
        return (f'<span class="d">{hdr or ""}{ts}</span>{gap}'
                f'<span style="color:{c}">{src}</span><span{b}>{rest}</span>')
    if re.match(r"^\s*id=", s) or s.startswith("next:"):
        return f'<span class="d">{s}</span>'
    if s.startswith("## "):
        return f'<span class="r">{s}</span>'
    return s


def page(blocks: list[tuple[list[str], str]]) -> str:
    body = []
    for args, out in blocks:
        shown = " ".join(f'"{a}"' if " " in a else a for a in args)
        body.append(f'<span class="p">~/code/shop-api $</span> <span class="c">recall {html.escape(shown)}</span>')
        body.extend(colour(l) for l in out.splitlines())
        body.append("")
    text = "\n".join(body).rstrip()
    return f"""<!doctype html><meta charset="utf-8"><style>
html,body{{margin:0;background:#0d1117}}
.w{{margin:28px;border-radius:10px;background:#161b22;border:1px solid #30363d;
   box-shadow:0 18px 50px rgba(0,0,0,.55);overflow:hidden}}
.t{{height:34px;background:#21262d;display:flex;align-items:center;padding:0 14px;gap:8px}}
.t i{{width:12px;height:12px;border-radius:50%;display:block}}
.t span{{color:#8b949e;font:13px -apple-system,system-ui,sans-serif;margin-left:10px}}
pre{{margin:0;padding:18px 22px 22px;color:#c9d1d9;
    font:14.5px/1.55 "JetBrains Mono","SF Mono",Menlo,monospace;white-space:pre-wrap}}
.p{{color:#8b949e}} .c{{color:#e6edf3;font-weight:600}}
.d{{color:#6e7681}} .m{{color:#e3b341;font-weight:600}}
.r{{color:#56d4dd}} .b{{color:#e6edf3;font-weight:600}}
</style><div class="w"><div class="t"><i style="background:#ff5f57"></i>
<i style="background:#febc2e"></i><i style="background:#28c840"></i>
<span>recall: one search across pi, Claude Code and Codex</span></div><pre>{text}</pre></div>"""


def main() -> None:
    binary, out_png = os.path.abspath(sys.argv[1]), os.path.abspath(sys.argv[2])
    here = Path(__file__).resolve().parent
    with tempfile.TemporaryDirectory() as tmp:
        home = Path(tmp).resolve() / "home"
        home.mkdir()
        subprocess.run([sys.executable, here / "corpus.py", home], check=True)
        env = dict(os.environ, HOME=str(home))
        subprocess.run([binary, "index"], env=env, check=True, capture_output=True)
        blocks = [(a, run(binary, home, a)) for a in COMMANDS]
        doc = Path(tmp) / "demo.html"
        doc.write_text(page(blocks))
        # Count wrapped lines: about 112 monospace columns fit the 1140 px window.
        cols = 112
        lines = sum(max(1, -(-len(l) // cols)) for _, out in blocks for l in out.splitlines())
        lines += 2 * len(blocks) - 1
        height = 56 + 34 + 40 + int(lines * 14.5 * 1.55) + 4
        subprocess.run([CHROMIUM, "--headless=new", "--hide-scrollbars",
                        "--force-device-scale-factor=2", f"--window-size=1140,{height}",
                        f"--screenshot={out_png}", doc.as_uri()],
                       check=True, capture_output=True)
    print(out_png)


if __name__ == "__main__":
    main()
