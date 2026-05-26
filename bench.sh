#!/usr/bin/env bash
# recall — benchmark harness
#
# Measures the autoresearch primary metric (incremental_index_ms) plus a
# handful of secondaries. Emits one JSONL line on stdout suitable for
# appending to autoresearch.jsonl.
#
# Usage:
#   ./bench.sh                       # run, print one JSONL line
#   ./bench.sh --full                # also rebuild from scratch (slow)
#   ./bench.sh --notes "what changed" --status keep --discard-reason ""
#
# Requires: bash, jq, /usr/bin/time, go.

set -euo pipefail
cd "$(dirname "$0")"

NOTES=""
STATUS="keep"
WITH_FULL=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --notes) NOTES="$2"; shift 2;;
    --status) STATUS="$2"; shift 2;;
    --full) WITH_FULL=1; shift;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

# 1. Build (not timed as part of the metric; lets us measure binary_size_mb).
go build -o recall .
binary_bytes=$(stat -f%z recall 2>/dev/null || stat -c%s recall)
binary_mb=$(echo "scale=2; $binary_bytes/1024/1024" | bc)

commit=$(git rev-parse --short HEAD 2>/dev/null || echo "uncommitted")

# 2. Warm-up. Ensure index exists; do a full reindex once if missing.
if [[ ! -f "$HOME/.recall/index.sqlite" ]]; then
  ./recall index >/dev/null
fi

# 3. Primary metric: incremental index re-run, no source changes.
# Run 3 times, keep the median to dampen disk-cache noise.
times=()
for _ in 1 2 3; do
  t=$( { /usr/bin/time -p ./recall index >/dev/null; } 2>&1 | awk '/^real/ {print $2}' )
  # `time -p` prints seconds with millisecond precision; convert to ms.
  ms=$(echo "$t * 1000 / 1" | bc)
  times+=("$ms")
done
incr_ms=$(printf '%s\n' "${times[@]}" | sort -n | sed -n '2p')

# 4. query latency (5 sample queries).
qtotal=0
for q in "import cycle" "race condition" "retry backoff" "memory leak" "rate limiter"; do
  t=$( { /usr/bin/time -p ./recall "$q" --limit 5 >/dev/null; } 2>&1 | awk '/^real/ {print $2}' )
  ms=$(echo "$t * 1000 / 1" | bc)
  qtotal=$((qtotal + ms))
done
query_ms=$((qtotal / 5))

# 5. Index size.
idx_bytes=$(stat -f%z "$HOME/.recall/index.sqlite" 2>/dev/null || stat -c%s "$HOME/.recall/index.sqlite")
idx_mb=$(echo "scale=2; $idx_bytes/1024/1024" | bc)

# 6. Optional cold rebuild (slow).
full_s=""
if [[ "$WITH_FULL" -eq 1 ]]; then
  rm -rf "$HOME/.recall"
  t=$( { /usr/bin/time -p ./recall index >/dev/null; } 2>&1 | awk '/^real/ {print $2}' )
  full_s="$t"
fi

# Counts after run (validates the index didn't silently drop sessions).
counts=$(./recall doctor 2>/dev/null | awk '/sessions$/ {print $1"="$2}' | tr '\n' ',' | sed 's/,$//')

jq -nc \
  --arg ts "$(date -u +%FT%TZ)" \
  --arg commit "$commit" \
  --arg notes "$NOTES" \
  --arg status "$STATUS" \
  --arg counts "$counts" \
  --argjson incremental_index_ms "$incr_ms" \
  --argjson query_ms "$query_ms" \
  --argjson index_size_mb "$idx_mb" \
  --argjson binary_size_mb "$binary_mb" \
  --arg full_index_seconds "${full_s:-}" \
  '{ts:$ts, commit:$commit, status:$status, notes:$notes,
    primary:"incremental_index_ms",
    incremental_index_ms:$incremental_index_ms,
    query_ms:$query_ms,
    index_size_mb:$index_size_mb,
    binary_size_mb:$binary_size_mb,
    full_index_seconds:$full_index_seconds,
    counts:$counts}'
