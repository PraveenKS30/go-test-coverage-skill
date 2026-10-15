#!/usr/bin/env bash
# Fan-out: one fresh `claude -p` per package, driven by THIS script (not by a
# model). Same pattern as Claude Code's best-practices "Fan out across files":
#
#   for item in $(queue); do claude -p "<prompt>" --allowedTools ... --permission-mode dontAsk; done
#
# gocov does the deterministic parts (queue, prompt, model and spend cap per
# call, verification, revert, goal check); this loop only orchestrates. Models,
# caps, attempts and timeouts come from the config (models.*, budget.*,
# max_attempts) through `gocov next-call`, by the same rules as `gocov run`.
#
# Prerequisite: you have already run, and reviewed the output of,
#   gocov report -config CFG   and   gocov plan -config CFG
# and `claude -p` can authenticate (standalone CLI logged in, or ANTHROPIC_API_KEY).
#
# Usage:
#   fanout.sh -c cfg.json -r REPO [options]
#     -c FILE     gocov config (required)
#     -r NAME     repo name from the config (required)
#     -n N        pilot: only the first N queued items
#     -v          run `gocov review -headless` at the end
#     --dry-run   print the claude command and prompt for the first item; call nothing
set -uo pipefail

CFG=""; REPO=""; PILOT=0; REVIEW=0; DRY=0; DRYF=""
while [ $# -gt 0 ]; do
  case "$1" in
    -c) CFG="$2"; shift 2;; -r) REPO="$2"; shift 2;; -n) PILOT="$2"; shift 2;;
    -v) REVIEW=1; shift;; --dry-run) DRY=1; DRYF=-dry-run; shift;;
    *) echo "unknown option: $1" >&2; exit 2;;
  esac
done
[ -n "$CFG" ] && [ -n "$REPO" ] || { sed -n '2,22p' "$0"; exit 2; }

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

# next_call ID ATTEMPT: sets CALL="MODEL USD SECS WORKDIR" and NC=0, or NC=4
# (budget/time limit reached: stop) / NC=5 (attempts used) with the reason in CALL.
next_call() { CALL="$(g next-call -item "$1" -attempt "$2" -spent "$SPENT" -elapsed "$SECONDS" $DRYF)"; NC=$?; [ "$NC" -eq 0 ]; }

# claude_call MODEL USD SECS WORKDIR IN OUT: one capped call in the module dir,
# killed after SECS seconds so a hung call cannot block the run. A kill leaves
# OUT.killed behind (a killed call reports no cost; the caller charges its cap).
claude_call() {
  rm -f "$6.killed"
  ( cd "$4" && exec claude -p --output-format json --model "$1" --permission-mode dontAsk \
      --max-budget-usd "$2" --no-session-persistence --allowedTools "$TOOLS" < "$5" > "$6" ) &
  local pid=$!
  # the watchdog kills its own sleep when it is stopped, so none is left behind
  ( trap 'kill "$s" 2>/dev/null; exit 0' TERM; sleep "$3" & s=$!; wait "$s"; : > "$6.killed"; kill "$pid" 2>/dev/null ) & local dog=$!
  wait "$pid"; local rc=$?
  kill "$dog" 2>/dev/null
  return "$rc"
}
cost_of() { local c; c="$(grep -o '"total_cost_usd": *[0-9.eE+-]*' "$1" 2>/dev/null | head -1 | cut -d: -f2 | tr -d ' ')"; echo "${c:-0}"; }
add() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.4f", a+b}'; }

WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
SPENT=0; DONE=0; BLOCKED=0; N=0; VERIFIED=0; CALL=""; NC=0

if [ "$DRY" -eq 0 ]; then
  "$GOCOV" preflight -config "$CFG" || exit 1   # fail in seconds, not after planning, if claude -p cannot authenticate
fi

IDS="$(g queue)" || exit 1
for ID in $IDS; do
  if g goal >/dev/null; then
    [ "$DRY" -eq 1 ] && { echo "goal already reached by estimate - nothing to dry-run"; exit 0; }
    echo "goal reached by estimate - verifying with a fresh full run..."
    g verify; if g goal >/dev/null; then VERIFIED=1; break; fi
    echo "verified below goal - the estimate was optimistic, continuing"
  fi
  [ "$PILOT" -gt 0 ] && [ "$N" -ge "$PILOT" ] && { echo "pilot of $PILOT item(s) done"; break; }
  if ! next_call "$ID" 1; then
    [ "$NC" -eq 4 ] && { echo "$CALL"; break; }   # remaining items stay queued
    echo "gocov next-call failed ($NC): $CALL"; exit 1
  fi
  N=$((N+1))

  PROMPT_FILE="$(g begin -item "$ID" -q $DRYF)"; RC=$?   # with --dry-run: saves nothing, leaves the tree alone
  if [ "$RC" -eq 3 ]; then echo "$PROMPT_FILE"; BLOCKED=$((BLOCKED+1)); continue; fi   # package does not build: blocked by gocov
  [ "$RC" -ne 0 ] && { echo "gocov begin failed ($RC) - stopping"; exit "$RC"; }
  FEEDBACK_FILE="${PROMPT_FILE%.prompt.md}.feedback.md"

  if [ "$DRY" -eq 1 ]; then
    read -r M USD SECS WD <<< "$CALL"
    echo "--- DRY RUN: $ID (in $WD, killed after ${SECS}s)"
    echo "claude -p --output-format json --model $M --permission-mode dontAsk --max-budget-usd $USD --no-session-persistence --allowedTools \"$TOOLS\" < $PROMPT_FILE"
    echo; cat "$PROMPT_FILE"; exit 0
  fi

  # attempt 1 uses the CALL checked before begin: asking again could hit the
  # time limit during begin's measurement and block an item never attempted
  OK=0; ATT=1
  while [ "$ATT" -eq 1 ] || next_call "$ID" "$ATT"; do
    read -r M USD SECS WD <<< "$CALL"
    cp "$PROMPT_FILE" "$WORK/in.md"
    [ -s "$FEEDBACK_FILE" ] && { printf '\n\nPREVIOUS ATTEMPT FEEDBACK - fix this first:\n' >> "$WORK/in.md"; cat "$FEEDBACK_FILE" >> "$WORK/in.md"; }
    echo "[$REPO] $ID attempt $ATT (model $M, cap \$$USD, timeout ${SECS}s)"
    claude_call "$M" "$USD" "$SECS" "$WD" "$WORK/in.md" "$WORK/out.json"
    if [ -e "$WORK/out.json.killed" ]; then
      C="$USD"; echo "[$REPO]   claude killed at the ${SECS}s timeout: charging its cap \$$USD (a killed call reports no cost)"
    else
      C="$(cost_of "$WORK/out.json")"
    fi
    SPENT="$(add "$SPENT" "$C")"
    if grep -q '"is_error": *true' "$WORK/out.json" && [ "$(awk -v c="$C" 'BEGIN{print (c==0)}')" = 1 ]; then
      echo "claude -p could not run: $(grep -o '"result": *"[^"]*"' "$WORK/out.json" | head -1)"
      echo "(no model call was made; check login / API key, then re-run - the item stays queued)"; exit 1
    fi
    g finish -item "$ID" -cost "$C"; RC=$?
    if [ "$RC" -eq 0 ]; then OK=1; DONE=$((DONE+1)); break; fi
    [ "$RC" -ne 3 ] && { echo "gocov finish failed ($RC)"; exit "$RC"; }
    ATT=$((ATT+1))
  done
  if [ "$OK" -eq 0 ]; then
    [ "$NC" -eq 4 ] || [ "$NC" -eq 5 ] || { echo "gocov next-call failed ($NC): $CALL"; exit 1; }
    echo "$CALL"; g finish -item "$ID" -abandon; BLOCKED=$((BLOCKED+1))
    [ "$NC" -eq 4 ] && break   # budget gone mid-item: like gocov run, block it and stop
  fi
  echo "[$REPO] spent so far: \$$SPENT"
done
[ "$DRY" -eq 1 ] && { echo "dry run: no item to show - nothing changed"; exit 0; }   # never verify/review on a dry run

echo; echo "== $DONE done, $BLOCKED blocked, \$$SPENT spent."
if [ "$VERIFIED" -eq 0 ]; then echo "Verifying with a fresh full run..."; g verify; fi
if [ "$REVIEW" -eq 1 ]; then g review -headless; else echo "Next: gocov review (adversarial review of the new tests)"; fi
