#!/usr/bin/env bash
# Utility guard for the token-efficiency loop.
#
# The metric being optimised is "characters an agent must read". That metric is
# trivially gamed by returning less useful output, so every run must first prove
# the output is still worth reading. If any check here fails the run is
# discarded regardless of how good the number looked.
set -uo pipefail

BIN="${BIN:-./recall}"
# Frozen corpus: the live index is shared with other running agents (and older
# builds re-stamp its schema), which would make runs incomparable.
export RECALL_INDEX="${RECALL_INDEX:-$HOME/.recall/bench.sqlite}"
fail=0
note() { printf '  %-58s %s\n' "$1" "$2"; }
check() { # check <desc> <cmd...>
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then note "$desc" "ok"; else note "$desc" "FAIL"; fail=1; fi
}
# grepcheck <desc> <pattern> <cmd...> — output must match pattern.
# Output is captured rather than piped: `cmd | grep -q` makes grep exit early,
# SIGPIPEs the producer, and under `pipefail` that reads as a false failure on
# any output large enough not to fit the pipe buffer.
grepcheck() {
  local desc="$1" pat="$2"; shift 2
  local out; out=$("$@" 2>/dev/null)
  if printf '%s' "$out" | grep -qE "$pat"; then note "$desc" "ok"
  else note "$desc" "FAIL (no /$pat/)"; fail=1; fi
}

go build -o "$BIN" . || { echo "build failed"; exit 1; }
echo "utility guard:"

# 1. Search still finds the known hits the repo has always smoke-tested on.
grepcheck "search finds 'race condition'" '[Rr]ace|[Cc]ondition' "$BIN" "race condition" --limit 5
grepcheck "search finds 'schema migration'" 'id=' "$BIN" "schema migration" --limit 5

# Pick a real, large session to exercise transcript modes against.
# ...which means the largest, not merely the most recent. The newest session is
# often two messages long, and every transcript check below then passes or fails
# on whether one trivial session happens to contain the word "the". msg_count is
# not exposed by `sessions --json`, so read it from the index.
SID=$(sqlite3 "$RECALL_INDEX" \
      "SELECT id FROM sessions ORDER BY COALESCE(msg_count,0) DESC LIMIT 1" 2>/dev/null)
if [ -z "$SID" ]; then echo "  no session available for transcript checks"; exit 1; fi

# 2. An outline must remain *navigable*: it has to expose position markers that
#    --range can then slice on, and name the roles present. Cheap is only
#    valuable if you can still find your way.
grepcheck "outline exposes [N] position markers" '\[[0-9]+' "$BIN" show "$SID" --outline
grepcheck "outline names roles"                  'user|assistant' "$BIN" show "$SID" --outline

# 3. A range slice must actually return the messages asked for, with their
#    numbering intact, so outline -> range round-trips.
grepcheck "range slice returns numbered msgs" '^## [0-9]+ '  "$BIN" show "$SID" --range 0:6
grepcheck "range slice carries content"       '.{60}'       "$BIN" show "$SID" --range 0:6

# 4. Role filtering must still work and must not empty the transcript.
grepcheck "role filter keeps content" '.{60}' "$BIN" show "$SID" --range 0:40 --role user,assistant

# 5. Search output must stay actionable: every hit needs an id to follow up on.
# NB: not "index" as the query — that resolves to the `index` subcommand.
grepcheck "search hits carry session ids" 'id=' "$BIN" "indexing" --limit 5

# 6. No regression in what the index holds.
grepcheck "stats reports sessions" '[0-9]{3,}' "$BIN" stats --projects 1 --models 1

# 7. The workflows the docs recommend must actually work. Session-scoped search
#    silently returned a single hit for as long as the docs told agents to
#    prefer it, so each documented path now gets asserted rather than assumed.
hits=$("$BIN" find "the" --in "$SID" --limit 5 2>/dev/null | grep -cE '^  msg=')
if [ "${hits:-0}" -ge 2 ]; then note "session-scoped search returns many hits" "ok ($hits)"
else note "session-scoped search returns many hits" "FAIL (got ${hits:-0}, dedupe collapsing?)"; fail=1; fi

grepcheck "session-scoped search states id once" 'hits\)' "$BIN" find "the" --in "$SID" --limit 5

# 8. Nothing may go missing silently. A read that does not deliver everything
#    asked for must carry a pointer to the rest — autoresearch/retention.py proved
#    prose_lost=0 against the pre-optimisation binary, and this keeps it there
#    without needing that binary around.
trunc=$("$BIN" show "$SID" --range 0:400 2>/dev/null)
delivered=$(printf '%s' "$trunc" | grep -cE '^## [0-9]+ ')
if [ "$delivered" -ge 400 ]; then
  note "large read: delivered in full" "ok ($delivered msgs)"
elif printf '%s' "$trunc" | grep -q "Continue with range="; then
  note "large read: partial delivery names the rest" "ok ($delivered msgs + pointer)"
else
  note "large read: partial delivery names the rest" "FAIL (silent truncation)"; fail=1
fi

# Clipped tool output must announce itself rather than look complete.
if "$BIN" show "$SID" --range 0:400 2>/dev/null | grep -q 'elided'; then
  note "clipped tool output is marked" "ok"
else note "clipped tool output is marked" "skipped (no long tool result in range)"; fi

# --context must answer find-and-read in ONE call: locate AND carry content.
ctx=$("$BIN" find "the" --in "$SID" --context 2 --limit 1 2>/dev/null)
if printf '%s' "$ctx" | grep -q 'msg=' && printf '%s' "$ctx" | grep -qE '^## [0-9]+ '; then
  note "--context returns hit + surrounding messages" "ok"
else note "--context returns hit + surrounding messages" "FAIL"; fail=1; fi

# 9. This repo is public and the workload is derived from private history.
#    A real query quoted into a doc or an experiment log is the likely leak.
#    The deny-list is NOT kept here: a public file naming your projects and
#    vendors leaks exactly what it is meant to protect. Put one extended-regex
#    alternation per line in autoresearch/private-names.txt (gitignored), e.g.
#      acme-corp
#      internal-service-name
#    The generic patterns below (home paths, secrets, emails) always run.
private_re=''
if [ -f autoresearch/private-names.txt ]; then
  private_re=$(grep -vE '^\s*(#|$)' autoresearch/private-names.txt | paste -sd'|' -)
fi
generic_re='/Users/[a-z][a-z0-9._-]+|sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9]{30,}|AKIA[0-9A-Z]{16}'
[ -n "$private_re" ] && scan_re="($private_re)|($generic_re)" || scan_re="$generic_re"
leak=$(git ls-files 2>/dev/null | grep -v '^autoresearch/checks.sh$' | while read -r f; do
  [ -f "$f" ] || continue
  grep -lEi "$scan_re" "$f" 2>/dev/null
done | head -3)
if [ -z "$leak" ]; then note "no private names in tracked files" "ok"
else note "no private names in tracked files" "FAIL ($leak)"; fail=1; fi

if git ls-files --error-unmatch autoresearch/agent_workload.json >/dev/null 2>&1; then
  note "private workload not tracked" "FAIL (it is committed)"; fail=1
else note "private workload not tracked" "ok"; fi

exit $fail
