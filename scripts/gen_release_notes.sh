#!/bin/bash
# Generate release notes for the commits in RANGE (default: last tag..HEAD),
# grouped by conventional-commit type. Output is markdown, used both as the
# annotated tag message (and thus the GitHub release body) and as the
# CHANGELOG.md section written by `make release`.
set -euo pipefail

cd "$(dirname "$0")/.."
RANGE="${1:-}"
if [ -z "$RANGE" ]; then
    prev=$(git describe --tags --abbrev=0 2>/dev/null || true)
    if [ -n "$prev" ]; then RANGE="${prev}..HEAD"; else RANGE="HEAD"; fi
fi

section() { # $1 header, $2 grep pattern
    local commits
    commits=$(git log "$RANGE" --oneline --no-merges 2>/dev/null | grep -Ei "$2" || true)
    [ -z "$commits" ] && return 0
    echo "### $1"
    echo "$commits" | sed -E 's/^([0-9a-f]+) /\1 /; s/^/- /'
    echo
}

section "Added"    '^[0-9a-f]+ feat'
section "Fixed"    '^[0-9a-f]+ fix'
section "Changed"  '^[0-9a-f]+ (refactor|perf|feat!)'
section "Docs"     '^[0-9a-f]+ docs'
section "Internal" '^[0-9a-f]+ (chore|ci|test|style|build)'

# Anything not matched above still deserves a line.
rest=$(git log "$RANGE" --oneline --no-merges 2>/dev/null | grep -Evi '^[0-9a-f]+ (feat|fix|refactor|perf|docs|chore|ci|test|style|build)' || true)
if [ -n "$rest" ]; then
    echo "### Other"
    echo "$rest" | sed 's/^/- /'
    echo
fi
