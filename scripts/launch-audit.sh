#!/usr/bin/env bash
# Checks everything that becomes public when the repository does, against a
# private denylist, and the settings a public repository needs. Prints one
# line per check and exits non-zero if any fails.
#
#   scripts/launch-audit.sh <denylist-file>
#
# Like leak-check.sh, it names where a pattern matched, never the pattern.
set -uo pipefail

denylist=${1:?usage: scripts/launch-audit.sh <denylist-file>}
repo=${REPO:-dominikzabcik/boxrunner}
cd "$(dirname "$0")/.." || exit 1
failed=0
ok() { echo "ok    $*"; }
bad() { echo "FAIL  $*"; failed=1; }

patterns=$(grep -Ev '^\s*(#|$)' "$denylist")
scan() { # scan <label> < text
	local text n=0 hit=0
	text=$(cat)
	while IFS= read -r p; do
		n=$((n + 1))
		if grep -qiE -e "$p" <<<"$text"; then
			bad "$1: denylist pattern $n matches"
			hit=1
		fi
	done <<<"$patterns"
	[ $hit -eq 0 ] && ok "$1: clean"
}

if out=$(scripts/leak-check.sh "$denylist" 2>&1); then ok "git: $out"; else bad "git: $(tr '\n' ' ' <<<"$out")"; fi

if command -v gitleaks >/dev/null; then
	if gitleaks git --redact --no-banner . >/dev/null 2>&1; then ok "gitleaks: no secrets in history"; else bad "gitleaks: findings in history"; fi
else
	bad "gitleaks: not installed"
fi

gh issue list -R "$repo" --state all -L 500 --json title,body,comments \
	-q '.[] | .title, .body, (.comments[].body)' | scan "issues and their comments"
gh pr list -R "$repo" --state all -L 500 --json title,body,comments \
	-q '.[] | .title, .body, (.comments[].body)' | scan "pull requests and their comments"
gh release list -R "$repo" -L 100 --json tagName -q '.[].tagName' | while read -r tag; do
	gh release view "$tag" -R "$repo" --json name,body -q '.name, .body'
done | scan "release notes"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
latest=$(gh release list -R "$repo" -L 1 --json tagName -q '.[0].tagName')
if [ -n "$latest" ] && gh release download "$latest" -R "$repo" -D "$tmp" >/dev/null 2>&1; then
	strings "$tmp"/* 2>/dev/null | scan "release $latest assets"
else
	ok "release assets: none yet"
fi

vars=$(gh variable list -R "$repo" --json name -q '.[].name' | tr '\n' ' ')
if [ -z "$vars" ]; then ok "no repository variables (no workflow points at a self-hosted runner)"; else bad "repository variables still set: $vars"; fi

if grep -rqE "runs-on: .*self-hosted" .github/workflows; then bad "a workflow names self-hosted runners"; else ok "workflows name no self-hosted runner"; fi

visibility=$(gh api "repos/$repo" -q .visibility)
echo "info  repository is $visibility"
if [ "$visibility" = public ]; then
	sec=$(gh api "repos/$repo" -q '.security_and_analysis | "\(.secret_scanning.status) \(.secret_scanning_push_protection.status)"')
	if [ "$sec" = "enabled enabled" ]; then ok "secret scanning and push protection on"; else bad "secret scanning / push protection: $sec"; fi
	pvr=$(gh api "repos/$repo/private-vulnerability-reporting" -q .enabled 2>/dev/null)
	if [ "$pvr" = true ]; then ok "private vulnerability reporting on"; else bad "private vulnerability reporting off"; fi
	sha=$(gh api "repos/$repo/actions/permissions" -q .sha_pinning_required 2>/dev/null)
	if [ "$sha" = true ]; then ok "Actions require SHA-pinned actions"; else bad "Actions do not require SHA pinning"; fi
fi

exit $failed
