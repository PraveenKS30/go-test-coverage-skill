#!/usr/bin/env bash
# Fan-out: one fresh `claude -p` per package, driven by THIS script (not by a
# model). Same pattern as Claude Code's best-practices "Fan out across files":
#
#   for item in $(queue); do claude -p "<prompt>" --allowedTools ... --permission-mode dontAsk; done
#
# gocov does the deterministic parts (queue, prompt, verification, revert, goal
# check); this loop does the orchestration and money control.
#
# Prerequisite: you have already run, and reviewed the output of,
#   gocov report -config CFG   and   gocov plan -config CFG
# and `claude -p` can authenticate (standalone CLI logged in, or ANTHROPIC_API_KEY).
#
# Usage:
#   fanout.sh -c cfg.json -r REPO -d /path/to/repo [options]
#     -c FILE     gocov config (required)
#     -r NAME     repo name from the config (required)
#     -d DIR      repo directory, where claude runs (required)
#     -n N        pilot: only the first N queued items
#     -m MODEL    model for attempt 1            (default sonnet)
#     -M MODEL    model for later attempts       (default opus)
#     -a N        max attempts per item          (default 2)
#     -i USD      per-call spend cap             (default 1.50)
#     -t USD      total spend cap for this run   (default 10)
#     -v          run `gocov review -headless` at the end
#     --dry-run   print the claude command and prompt for the first item; call nothing
set -uo pipefail

CFG=""; REPO=""; DIR=""; PILOT=0; MODEL=sonnet; ESC=opus; ATTEMPTS=2; ITEM_USD=1.50; TOTAL_USD=10; REVIEW=0; DRY=0
while [ $# -gt 0 ]; do
  case "$1" in
    -c) CFG="$2"; shift 2;; -r) REPO="$2"; shift 2;; -d) DIR="$2"; shift 2;;
    -n) PILOT="$2"; shift 2;; -m) MODEL="$2"; shift 2;; -M) ESC="$2"; shift 2;;
    -a) ATTEMPTS="$2"; shift 2;; -i) ITEM_USD="$2"; shift 2;; -t) TOTAL_USD="$2"; shift 2;;
    -v) REVIEW=1; shift;; --dry-run) DRY=1; shift;;
    *) echo "unknown option: $1" >&2; exit 2;;
  esac
done
[ -n "$CFG" ] && [ -n "$REPO" ] && [ -n "$DIR" ] || { sed -n '2,30p' "$0"; exit 2; }

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GOCOV="$HERE/bin/gocov"; [ -x "$GOCOV" ] || GOCOV="$HERE/bin/gocov.exe"
if [ ! -x "$GOCOV" ]; then
  echo "building gocov..."; (cd "$HERE/gocov" && go build -o ../bin/gocov .) || exit 1
  GOCOV="$HERE/bin/gocov"; [ -x "$GOCOV" ] || GOCOV="$HERE/bin/gocov.exe"
fi
g() { "$GOCOV" "$1" -config "$CFG" -repo "$REPO" "${@:2}"; }

# Same permission pattern as the article: pre-approve exactly what is needed and
# deny everything else (dontAsk), because nobody is watching.
TOOLS='Read,Edit,Write,Glob,Grep,Bash(go test *),Bash(go vet *),Bash(go build *),Bash(go list *),Bash(gofmt *),Bash(git diff *),Bash(git status *)'

claude_call() { # $1=model $2=input file $3=output file ; runs inside the repo
  ( cd "$DIR" && claude -p --output-format json --model "$1" --permission-mode dontAsk \
      --max-budget-usd "$ITEM_USD" --no-session-persistence --allowedTools "$TOOLS" < "$2" > "$3" )
}
cost_of() { grep -o '"total_cost_usd":[0-9.eE+-]*' "$1" | head -1 | cut -d: -f2 | awk '{print ($1==""?0:$1)}'; }
add() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.4f", a+b}'; }
gt()  { awk -v a="$1" -v b="$2" 'BEGIN{exit !(a>b)}'; }

WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
SPENT=0; DONE=0; BLOCKED=0; N=0

if [ "$DRY" -eq 0 ]; then
  "$GOCOV" preflight -config "$CFG" || exit 1   # fail in seconds, not after planning, if claude -p cannot authenticate
fi

for ID in $(g queue); do
  if g goal >/dev/null; then echo "goal reached by estimate - skipping the remaining items"; break; fi
  [ "$PILOT" -gt 0 ] && [ "$N" -ge "$PILOT" ] && { echo "pilot of $PILOT item(s) done"; break; }
  if gt "$SPENT" "$TOTAL_USD"; then echo "total budget \$$TOTAL_USD reached (spent \$$SPENT) - stopping; re-run to resume"; break; fi
  N=$((N+1))

  PROMPT_FILE="$(g begin -item "$ID" -q)" || { echo "begin failed for $ID"; continue; }
  FEEDBACK_FILE="${PROMPT_FILE%.prompt.md}.feedback.md"

  if [ "$DRY" -eq 1 ]; then
    echo "--- DRY RUN: $ID"; echo "claude -p --output-format json --model $MODEL --permission-mode dontAsk --max-budget-usd $ITEM_USD --no-session-persistence --allowedTools \"$TOOLS\" < $PROMPT_FILE"
    echo; cat "$PROMPT_FILE"; exit 0
  fi

  OK=0
  for ATT in $(seq 1 "$ATTEMPTS"); do
    M="$MODEL"; [ "$ATT" -gt 1 ] && M="$ESC"
    cp "$PROMPT_FILE" "$WORK/in.md"
    [ -s "$FEEDBACK_FILE" ] && { printf '\n\nPREVIOUS ATTEMPT FEEDBACK - fix this first:\n' >> "$WORK/in.md"; cat "$FEEDBACK_FILE" >> "$WORK/in.md"; }
    echo "[$REPO] $ID attempt $ATT/$ATTEMPTS (model $M)"
    claude_call "$M" "$WORK/in.md" "$WORK/out.json"
    C="$(cost_of "$WORK/out.json")"; SPENT="$(add "$SPENT" "$C")"
    if grep -q '"is_error":true' "$WORK/out.json" && [ "$(awk -v c="$C" 'BEGIN{print (c==0)}')" = 1 ]; then
      echo "claude -p could not run: $(grep -o '"result":"[^"]*"' "$WORK/out.json" | head -1)"
      echo "(no model call was made; check login / API key, then re-run - the item stays queued)"; exit 1
    fi
    g finish -item "$ID" -cost "$C"; RC=$?
    if [ "$RC" -eq 0 ]; then OK=1; DONE=$((DONE+1)); break; fi
    [ "$RC" -ne 3 ] && { echo "gocov finish failed ($RC)"; exit "$RC"; }
  done
  if [ "$OK" -eq 0 ]; then g finish -item "$ID" -abandon; BLOCKED=$((BLOCKED+1)); fi
  echo "[$REPO] spent so far: \$$SPENT"
done

echo; echo "== $DONE done, $BLOCKED blocked, \$$SPENT spent. Verifying with a fresh full run..."
g verify
if [ "$REVIEW" -eq 1 ]; then g review -headless; else echo "Next: gocov review (adversarial review of the new tests)"; fi
