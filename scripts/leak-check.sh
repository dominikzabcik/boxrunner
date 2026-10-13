#!/usr/bin/env bash
# Fails when the tree or any commit contains a pattern from a private denylist.
#
#   scripts/leak-check.sh <denylist-file>
#   LEAK_DENYLIST="$(cat denylist)" scripts/leak-check.sh
#
# The denylist is one extended regex per line (# comments allowed), matched
# case-insensitively. It never lives in this repository, and the output names
# only the place and the pattern's line number, so a public CI log cannot
# reveal what the pattern is.
set -euo pipefail

if [ $# -gt 0 ]; then
	patterns=$(cat "$1")
else
	patterns=${LEAK_DENYLIST:?pass a denylist file or set LEAK_DENYLIST}
fi

found=0
n=0
while IFS= read -r pattern; do
	n=$((n + 1))
	case $pattern in '' | '#'*) continue ;; esac

	if hits=$(git grep -IilE -e "$pattern" -- . 2>/dev/null); then
		while IFS= read -r file; do echo "denylist line $n: in $file"; done <<<"$hits"
		found=1
	fi
	# Every commit's message and diff: deleting a file does not delete it from history.
	if hits=$(git log --all -i -E -G "$pattern" --format=%h 2>/dev/null) && [ -n "$hits" ]; then
		echo "denylist line $n: in the diff of commit(s) $(tr '\n' ' ' <<<"$hits")"
		found=1
	fi
	if hits=$(git log --all -i -E --grep="$pattern" --format=%h 2>/dev/null) && [ -n "$hits" ]; then
		echo "denylist line $n: in the message of commit(s) $(tr '\n' ' ' <<<"$hits")"
		found=1
	fi
	if hits=$(git log --all --format='%h %an %ae %cn %ce' | grep -iE -e "$pattern" | cut -d' ' -f1) && [ -n "$hits" ]; then
		echo "denylist line $n: in the author or committer of commit(s) $(tr '\n' ' ' <<<"$hits")"
		found=1
	fi
done <<<"$patterns"

if [ $found -eq 0 ]; then
	echo "leak check: clean ($(git rev-list --all --count) commits, $(git ls-files | wc -l | tr -d ' ') files)"
fi
exit $found
