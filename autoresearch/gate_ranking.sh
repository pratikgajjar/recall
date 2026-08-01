#!/usr/bin/env bash
# Ranking gate — mandatory for any change that alters indexed text.
#
# recall_score cannot see a ranking regression: no term in it measures whether
# the right session still beats its competitors for a short, ambiguous query.
# Raising excerptMax to 24000 scored +4.5 while MRR fell 8.8% and rank@3 went
# 22 -> 18. A score that accepts that change is worse than no score.
#
# This builds two indexes from the same disk, the same minute, one with the
# control binary and one with the working tree's, and compares retrieval on real
# queries. Building both is the point: the corpus grows constantly, so only a
# same-data comparison isolates what the code did.
#
#   autoresearch/gate_ranking.sh              # control = last commit
#   autoresearch/gate_ranking.sh /tmp/old     # control = a binary you name
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

TOL=${TOL:-3}            # tolerated MRR loss, percent relative
CTRL_BIN=${1:-}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

if [ -z "$CTRL_BIN" ]; then
  # A detached worktree at HEAD, not `git stash` — stashing the tree you are
  # working in to build a control is how uncommitted work gets lost.
  echo "building control binary from HEAD..."
  git worktree add --quiet --detach "$work/head" HEAD || { echo "worktree failed"; exit 1; }
  ( cd "$work/head" && go build -o "$work/control" . ) || { echo "control build failed"; exit 1; }
  git worktree remove --force "$work/head"
  CTRL_BIN="$work/control"
fi

go build -o "$work/treat" . || { echo "treatment build failed"; exit 1; }

echo "indexing control..."
RECALL_INDEX="$work/control.sqlite" "$CTRL_BIN" index --full >/dev/null 2>&1
echo "indexing treatment..."
RECALL_INDEX="$work/treat.sqlite" "$work/treat" index --full >/dev/null 2>&1

for side in control treat; do
  n=$(sqlite3 "$work/$side.sqlite" "select count(*) from sessions" 2>/dev/null || echo 0)
  if [ "${n:-0}" -lt 100 ]; then
    echo "  ABORT  $side index has ${n:-0} sessions — it did not build."
    echo "         A gate that passes because its control is empty is worse than no gate."
    exit 2
  fi
done

# rank_live invokes ./recall; both indexes must be read by the same reader so
# the only difference measured is what got written into them.
out=$(autoresearch/rank_live.py --index "$work/control.sqlite" \
                                --compare "$work/treat.sqlite" --json)
echo "$out" | python3 -c '
import json,sys,os
d=json.load(sys.stdin); a,b=d["a"],d["b"]
tol=float(os.environ.get("TOL","3"))
print("  %-12s %9s %9s %8s" % ("", "control", "change", "delta"))
for k in ("found","rank_at_1","rank_at_3","mrr"):
    print(f"  {k:<12} {a[k]:>9} {b[k]:>9} {b[k]-a[k]:>+8.4g}")
drop = 0.0 if a["mrr"] == 0 else 100*(a["mrr"]-b["mrr"])/a["mrr"]
print()
if drop > tol:
    print(f"  FAIL  MRR fell {drop:.1f}% (tolerance {tol:.0f}%) — indexed text got worse to search")
    sys.exit(1)
if drop < -tol:
    print(f"  PASS  MRR rose {-drop:.1f}%")
else:
    print(f"  PASS  MRR within {tol:.0f}% ({drop:+.1f}%)")
'
