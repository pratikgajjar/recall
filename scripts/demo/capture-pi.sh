#!/bin/sh
# Capture a real pi session that uses recall on the synthetic demo corpus.
#
#   scripts/demo/capture-pi.sh <recall-binary> [model]
#
# Writes scripts/demo/pi-session.ansi. Then render it:
#   python3 scripts/demo/render.py pi scripts/demo/pi-session.ansi packages/pi-recall/assets/demo.png
#
# The demo HOME is /Users/Shared/demo (no user name in the paths). A wrapper
# named `recall` sets HOME to it, so pi's own config and auth are untouched.
# pi loads only the recall skill: no extensions, context files, MCP or session
# file, and a neutral system prompt.
set -eu
BIN=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
MODEL=${2:-openai-codex/gpt-6.1-sol}
REPO=$(cd "$(dirname "$0")/../.." && pwd)
DEMO=/Users/Shared/demo
WRAP=$(mktemp -d)

rm -rf "$DEMO" && mkdir -p "$DEMO/code/shop-api"
python3 "$REPO/scripts/demo/corpus.py" "$DEMO"
printf '#!/bin/sh\nHOME=%s exec %s "$@"\n' "$DEMO" "$BIN" > "$WRAP/recall"
chmod +x "$WRAP/recall"
"$WRAP/recall" index > /dev/null

tmux kill-session -t recalldemo 2>/dev/null || true
tmux new-session -d -s recalldemo -x 104 -y 80 -c "$DEMO/code/shop-api"
tmux send-keys -t recalldemo "clear; PATH=$WRAP:\$PATH pi --no-extensions --no-skills \
--skill $REPO/skills/recall --no-context-files --no-mcp --no-session --no-prompt-templates \
--tools bash,read --system-prompt 'You are a coding assistant. Answer briefly.' \
--model $MODEL --thinking low" Enter
sleep 6
tmux send-keys -t recalldemo -l "How did we fix the flaky checkout test?"
sleep 1
tmux send-keys -t recalldemo Enter
# The footer shows token counts once the turn ends.
i=0
until tmux capture-pane -p -t recalldemo | grep -q '↑[0-9.]*k'; do
  i=$((i + 1)); [ $i -gt 60 ] && { echo "timed out" >&2; exit 1; }
  sleep 4
done
sleep 8
tmux capture-pane -e -p -t recalldemo > "$REPO/scripts/demo/pi-session.ansi"
tmux kill-session -t recalldemo
rm -rf "$WRAP" "$DEMO"
echo "wrote scripts/demo/pi-session.ansi"
