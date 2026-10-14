#!/usr/bin/env bash
# Prints runner-digests.json: each actions/runner release from 2.323.0 on, the
# first with provenance attestations, with the digest its ghcr.io tag points
# to, once gh has verified that actions/runner's release workflow built that
# digest on the release branch of the version's minor release. Any version
# that fails stops the script, so a partial list is never printed and boxes
# keep the last list. Boxes build their job image only from a digest in this
# list (docs/architecture.md#image).
# It needs gh signed in (GH_TOKEN in CI), curl and jq.
#
#   scripts/runner-digests.sh             every release from 2.323.0
#   scripts/runner-digests.sh 2.338.0     only these versions
set -euo pipefail

image=ghcr.io/actions/actions-runner
# Releases whose image fails the check below for a known reason, left out of
# the list. 2.330.0's tag was moved five days after the release to an image
# that docker-publish.yml built on main, not release.yml on releases/m330.
skip=" 2.330.0 "
if [ $# -gt 0 ]; then
	versions=$(printf '%s\n' "$@")
else
	versions=$(gh api --paginate repos/actions/runner/releases --jq '.[]
		| select((.draft or .prerelease) | not) | .tag_name
		| select(test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")) | ltrimstr("v")
		| select((split(".") | map(tonumber)) >= [2, 323, 0])' |
		while read -r version; do
			[[ $skip == *" $version "* ]] || echo "$version"
		done)
fi
versions=$(sort -uV <<<"$versions")

token=$(curl -fsS "https://ghcr.io/token?scope=repository:${image#ghcr.io/}:pull" | jq -r .token)
lines=()
for version in $versions; do
	[[ $version =~ ^[0-9]+\.([0-9]+)\.[0-9]+$ ]] || {
		echo "$version is not a runner version" >&2
		exit 1
	}
	branch="refs/heads/releases/m${BASH_REMATCH[1]}"
	# The multi-platform index, as the agent resolves the tag.
	digest=$(curl -fsSI -H "Authorization: Bearer $token" \
		-H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json' \
		"https://ghcr.io/v2/${image#ghcr.io/}/manifests/$version" |
		tr -d '\r' | awk 'tolower($1) == "docker-content-digest:" { print $2 }')
	[[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || {
		echo "$image:$version: the registry named no sha256 digest ($digest)" >&2
		exit 1
	}
	# --cert-identity names the branch, which --signer-workflow cannot, so a
	# registry cannot pass off an older minor release's image under a newer tag.
	gh attestation verify "oci://$image@$digest" \
		--repo actions/runner \
		--cert-identity "https://github.com/actions/runner/.github/workflows/release.yml@$branch" \
		--cert-oidc-issuer https://token.actions.githubusercontent.com \
		--source-ref "$branch" \
		--deny-self-hosted-runners >/dev/null
	echo "verified $image:$version at $digest, built by release.yml on $branch" >&2
	lines+=("$version $digest")
done
[ ${#lines[@]} -gt 0 ] || {
	echo "no runner release to verify" >&2
	exit 1
}
printf '%s\n' "${lines[@]}" | jq -Rn '[inputs | split(" ") | {(.[0]): .[1]}] | add'
