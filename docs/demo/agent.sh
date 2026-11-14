#!/usr/bin/env bash
# Имитирует запрос Claude Code к хуку Holdfast (тот же JSON, что шлёт
# PreToolUse) и печатает ответ так, как его увидит агент.
#
#   agent bash "rm -rf ~"
#   agent read migrations/001_init.sql
set -euo pipefail

kind=$1
arg=$2
case $kind in
bash) tool=Bash; input=$(jq -nc --arg c "$arg" '{command: $c}') ;;
read) tool=Read; input=$(jq -nc --arg p "$PWD/$arg" '{file_path: $p}') ;;
edit) tool=Edit; input=$(jq -nc --arg p "$PWD/$arg" '{file_path: $p, old_string: "a", new_string: "b"}') ;;
*) echo "usage: agent bash|read|edit <arg>" >&2; exit 2 ;;
esac

dim=$'\e[2m' red=$'\e[1;31m' green=$'\e[1;32m' cyan=$'\e[1;36m' reset=$'\e[0m'
printf '%sагент → %s: %s%s\n' "$dim" "$tool" "$arg" "$reset"

out=$(jq -nc --arg t "$tool" --arg cwd "$PWD" --argjson ti "$input" \
	'{session_id: "demo", cwd: $cwd, hook_event_name: "PreToolUse", tool_name: $t, tool_input: $ti}' |
	CLAUDE_PROJECT_DIR=$PWD holdfast guard check)

if [[ -z $out ]]; then
	printf '%s✓ разрешено%s\n\n' "$green" "$reset"
	exit 0
fi
decision=$(jq -r '.hookSpecificOutput.permissionDecision // ""' <<<"$out")
case $decision in
deny) printf '%s✗ запрещено:%s %s\n\n' "$red" "$reset" "$(jq -r '.hookSpecificOutput.permissionDecisionReason' <<<"$out")" ;;
ask) printf '%s? спросить пользователя:%s %s\n\n' "$cyan" "$reset" "$(jq -r '.hookSpecificOutput.permissionDecisionReason' <<<"$out")" ;;
*) printf '%s✓ разрешено + подсказка агенту:%s\n%s\n\n' "$green" "$reset" "$(jq -r '.hookSpecificOutput.additionalContext' <<<"$out")" ;;
esac
