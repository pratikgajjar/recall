#!/usr/bin/env bash
# Every claim this loop makes, re-verified in one command.
#
#   autoresearch/all.sh            # ~4 min against the frozen corpus
#   autoresearch/all.sh --quick    # skip the slow replays, keep the guards
#
# The frozen corpus (~/.recall/bench.sqlite) is deliberate: the live index is
# shared with other running agents and older builds re-stamp its schema, which
# would make runs incomparable. Rebuild it with:
#   cp ~/.recall/index.sqlite ~/.recall/bench.sqlite && \
#     RECALL_INDEX=~/.recall/bench.sqlite ./recall index
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

export RECALL_INDEX="${RECALL_INDEX:-$HOME/.recall/bench.sqlite}"
quick=false
[ "${1:-}" = "--quick" ] && quick=true

hdr() { printf '\n\033[1m%s\033[0m\n' "$1"; }
fail=0

hdr "build + unit tests"
go build -o recall . || exit 1
go test ./... 2>&1 | tail -2

hdr "utility guard (output must stay worth reading)"
./autoresearch/checks.sh 2>&1 | tail -n +2
[ "${PIPESTATUS[0]:-0}" -ne 0 ] && fail=1

hdr "token cost over the real agent workload"
if $quick; then
  echo "  (skipped --quick)"
else
  echo -n "  dev     "; autoresearch/replay.py --split dev --json
  echo -n "  holdout "; autoresearch/replay.py --split holdout --json
fi

hdr "no signal lost (needs the pre-optimisation binary)"
if [ -x /tmp/recall-base ] && ! $quick; then
  autoresearch/retention.py --json
else
  echo "  skipped: build it with"
  echo "    git worktree add /tmp/wt-base 8be17e8 && (cd /tmp/wt-base && go build -o /tmp/recall-base .)"
fi

hdr "cost per navigation task"
$quick && echo "  (skipped --quick)" || autoresearch/workflow.py 2>/dev/null | tail -6

hdr "search ranking quality (from recorded agent behaviour)"
autoresearch/ranking.py --json

hdr "per-turn schema"
python3 - <<'PY'
import json, subprocess
req = json.dumps({"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}})
p = subprocess.run(["./recall","mcp"], input=req+"\n", capture_output=True, text=True, timeout=30)
for line in p.stdout.splitlines():
    try: d = json.loads(line)
    except: continue
    if d.get("id") == 1 and "result" in d:
        n = len(json.dumps(d["result"]["tools"]))
        print(f'  {len(d["result"]["tools"])} tools, {n:,} chars (~{n//4:,} tokens) re-sent every turn')
        break
PY

hdr "installed skill copies"
./recall doctor 2>/dev/null | sed -n '/^skill:/,$p' | sed 's/^/  /' || true
./recall doctor 2>/dev/null | grep -q '^skill:' || echo "  all in step with the binary"

exit $fail
