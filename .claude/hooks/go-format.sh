#!/usr/bin/env bash
# PostToolUse hook: gofmt the edited Go file, then vet its package.
# Exit 2 feeds the vet output back to Claude; formatting is applied silently.
set -u
f="$(jq -r '.tool_input.file_path // empty' 2>/dev/null)"
case "$f" in *.go) ;; *) exit 0 ;; esac
[ -f "$f" ] || exit 0
gofmt -w "$f" || exit 0
dir="$(dirname "$f")"
root="$(git -C "$dir" rev-parse --show-toplevel 2>/dev/null)" || exit 0
rel="./$(realpath --relative-to="$root" "$dir")"
out="$(cd "$root" && go vet "$rel" 2>&1)" && exit 0
printf 'go vet %s:\n%s\n' "$rel" "$out" >&2
exit 2
