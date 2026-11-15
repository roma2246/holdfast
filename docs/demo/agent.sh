#!/usr/bin/env bash
# Imitates an agent action: sends the Holdfast hook the same JSON as
# Claude Code does (PreToolUse) and prints the reply the way the agent sees it.
#
#   agent run  "mv a b"   check and, unless the hook denied it, execute
#   agent bash "rm -rf ~" check only
#   agent read migrations/001_init.sql
set -euo pipefail

kind=$1
arg=$2
case $kind in
run | bash) tool=Bash; input=$(jq -nc --arg c "$arg" '{command: $c}') ;;
read) tool=Read; input=$(jq -nc --arg p "$PWD/$arg" '{file_path: $p}') ;;
edit) tool=Edit; input=$(jq -nc --arg p "$PWD/$arg" '{file_path: $p, old_string: "a", new_string: "b"}') ;;
*) echo "usage: agent run|bash|read|edit <arg>" >&2; exit 2 ;;
esac

dim=$'\e[2m' red=$'\e[1;31m' green=$'\e[1;32m' cyan=$'\e[1;36m' reset=$'\e[0m'
printf '%sagent → %s: %s%s\n' "$dim" "$tool" "$arg" "$reset"

out=$(jq -nc --arg t "$tool" --arg cwd "$PWD" --argjson ti "$input" \
	'{session_id: "demo", cwd: $cwd, hook_event_name: "PreToolUse", tool_name: $t, tool_input: $ti}' |
	CLAUDE_PROJECT_DIR=$PWD holdfast guard check)

decision=""
if [[ -n $out ]]; then
	decision=$(jq -r '.hookSpecificOutput.permissionDecision // ""' <<<"$out")
fi
case $decision in
deny)
	printf '%s✗ denied:%s %s\n\n' "$red" "$reset" "$(jq -r '.hookSpecificOutput.permissionDecisionReason' <<<"$out")"
	exit 0
	;;
ask)
	printf '%s? ask the user:%s %s\n\n' "$cyan" "$reset" "$(jq -r '.hookSpecificOutput.permissionDecisionReason' <<<"$out")"
	exit 0
	;;
esac

if [[ -n $out ]]; then
	printf '%s✓ allowed + a hint for the agent:%s\n%s\n' "$green" "$reset" "$(jq -r '.hookSpecificOutput.additionalContext' <<<"$out")"
else
	printf '%s✓ allowed%s\n' "$green" "$reset"
fi
if [[ $kind == run ]]; then
	bash -c "$arg"
fi
echo
