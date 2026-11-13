#!/bin/sh
# Dispatch one worker; preserve the existing OpenCode harness.
set -eu

die() { echo "dispatch: $*" >&2; exit 2; }
[ "$#" -ge 3 ] && [ "$#" -le 4 ] || die 'usage: dispatch.sh ROLE MODEL BRIEF [SESSION]'
role=$1 model=$2 brief=$3
case $role in scout|implementer|reviewer) ;; *) die "unknown role '$role'";; esac
[ -s "$brief" ] || die "brief '$brief' is missing or empty"
case $model in
  glm-5.3-flash|deepseek-v4.1-flash) model=opencode-go/$model ;;
  opencode-go/glm-5.3-flash|opencode-go/deepseek-v4.1-flash) ;;
  codex/gpt-6-luna|codex/gpt-6.1-sol|codex/gpt-6-astra) ;;
  *) die "model '$model' is outside the Sras roster" ;;
esac

if [ "$role" = implementer ]; then
  dirty=$(git status --porcelain) || die 'cannot inspect the checkout'
  [ -z "$dirty" ] || [ "${DELEGATE_ALLOW_DIRTY:-0}" = 1 ] ||
    die 'implementer needs a clean checkout; use an isolated worktree'
fi

case $model in
  opencode-go/*)
    harness=$HOME/.agents/skills/delegate/dispatch.sh
    [ -f "$harness" ] || die "missing OpenCode harness: $harness"
    exec sh "$harness" "$role" "$model" "$brief" "${4:-}"
    ;;
esac

[ -z "${4:-}" ] || die 'Codex dispatch starts fresh; include previous feedback in the brief'
command -v codex >/dev/null 2>&1 || die 'Codex CLI is not installed'
model=${model#codex/}
effort=high
[ "$model" != gpt-6-luna ] || effort=medium
sandbox=read-only
[ "$role" != implementer ] || sandbox=workspace-write

# ponytail: one fresh process per brief; add resume support only when needed.
instructions='Follow the supplied brief and AGENTS.md. Do not select another roadmap task, delegate, commit, stage, push, or edit global configuration. Report unresolved decisions as BLOCKED.'
case $role in
  scout) instructions="$instructions Read only. Return ANSWER and EVIDENCE with path:line." ;;
  reviewer) instructions="$instructions Read only. Return VERDICT: PASS or FINDINGS; each finding needs path:line and a concrete failing case." ;;
  implementer) instructions="$instructions Edit only named files. Return STATUS: DONE, PARTIAL, or BLOCKED; CHANGED, RAN, RESULT, and OPEN." ;;
esac

mkdir -p .delegate
run=$(mktemp -d ".delegate/$role.$model.XXXXXX")
status=0
perl -e 'alarm shift; exec @ARGV or die "exec: $!\n"' "${DELEGATE_TIMEOUT:-1800}" \
  codex -a never exec --model "$model" --sandbox "$sandbox" \
  -c "model_reasoning_effort=$effort" -c agents.enabled=false \
  -c "developer_instructions=\"$instructions\"" \
  --json --output-last-message "$run/report.md" - \
  < "$brief" > "$run/events.jsonl" 2> "$run/stderr.log" || status=$?

if [ "$status" -ne 0 ]; then
  tail -n 10 "$run/stderr.log" >&2
  echo "dispatch: Codex exited $status; logs: $run" >&2
  exit "$status"
fi
[ -s "$run/report.md" ] || die "no usable Codex report; logs: $run"
cat "$run/report.md"
printf '\nmodel: %s  effort: %s  sandbox: %s  logs: %s\n' "$model" "$effort" "$sandbox" "$run"
