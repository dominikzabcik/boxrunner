#!/usr/bin/env bash
# Installs boxrunner on this machine with install.sh against a stub Worker,
# checks the box with doctor, installs again expecting no changes, uninstalls
# and checks that nothing is left. It changes the system: run it only on a
# machine you can throw away. CI runs it on GitHub's VMs (.github/workflows/e2e.yml).
#
#   scripts/e2e/run.sh runc|sysbox|docker
#
# runc installs without flags, sysbox with --sysbox and docker with --docker.
set -euo pipefail

mode=${1:?usage: scripts/e2e/run.sh runc|sysbox|docker}
case "$mode" in
runc) flags=() isolation="runc: a job's root is root on the host" ;;
sysbox) flags=(--sysbox) isolation="sysbox: a job's root is uid [1-9]" ;;
docker) flags=(--docker) isolation="sysbox: a job's root is uid [1-9]" ;;
*) echo "unknown mode $mode" >&2 && exit 2 ;;
esac
cd "$(dirname "$0")/../.."
worker=http://127.0.0.1:8787
work=$(mktemp -d)

step() { printf '\n### %s\n' "$*"; }
fail() {
	echo "FAIL: $*" >&2
	exit 1
}
# expect checks that every pattern matches a line of a file.
expect() {
	local file=$1
	shift
	for pattern in "$@"; do
		grep -qE -- "$pattern" "$file" || fail "no line in $(basename "$file") matches: $pattern"
	done
}

step "build the release and the stub Worker"
version=$(sed -n "s/^export const version = '\(.*\)';$/\1/p" worker/src/version.ts)
scripts/build-release.sh "$version"
go build -o "$work/stub-worker" ./scripts/e2e/stub-worker
"$work/stub-worker" -version "$version" >"$work/worker.log" 2>&1 &
stub=$!
trap 'kill $stub; printf "\n### the stub Worker saw\n"; sort "$work/worker.log" | uniq -c' EXIT
for _ in $(seq 50); do
	curl -fsS -o /dev/null -H 'Authorization: Bearer test' "$worker/v1/repos" 2>/dev/null && break
	sleep 0.1
done

step "install.sh installs boxrunner $version (${flags[*]:-no flags})"
sudo env BOXRUNNER_DOWNLOAD="file://$PWD/dist" sh dist/install.sh --worker "$worker" --token test "${flags[@]}" | tee "$work/install.txt"
expect "$work/install.txt" '^==> checksum ok$' '^Done in [0-9]+ steps'

step "the agent runs and reports to the Worker with the box token"
for _ in $(seq 60); do
	grep -q '^POST /v1/heartbeat 200$' "$work/worker.log" && break
	sleep 1
done
expect "$work/worker.log" '^POST /v1/heartbeat 200$'
systemctl is-active --quiet boxrunner.service || fail "boxrunner.service is not active"

# systemd added CPUWeight=idle in version 252. On an older one, such as Ubuntu
# 22.04's 249, the idle class runs at normal weight; doctor reports that, and
# this expects it until install handles it.
systemd=$(systemctl --version | awk 'NR == 1 { print $2 }')
if [ "$systemd" -ge 252 ]; then
	want=0 idle='^ok +idle slice +container runs in /boxrunner_idle.slice, cpu.idle 1'
else
	want=1 idle='^FAIL +idle slice +container runs in /boxrunner_idle.slice, cpu.idle 0'
fi

step "doctor checks the box (systemd $systemd)"
code=0
sudo boxrunner doctor | tee "$work/doctor.txt" || code=$?
[ "$code" = "$want" ] || fail "doctor exited with $code, want $want"
expect "$work/doctor.txt" "$idle" '^ok +config ' '^ok +cgroup driver +systemd' '^ok +firewall rules +in place for IPv4 and IPv6' \
	'^ok +host +the host at .* is unreachable from jobs' '^ok +metadata +169.254.169.254 is unreachable from jobs' \
	'^ok +private ' '^ok +github ' "^ok +worker +$worker accepts the token; 0 repositories, classes boxrunner, boxrunner-light$" \
	"^ok +isolation boxrunner +$isolation" "^ok +isolation boxrunner-light +$isolation"
if [ "$mode" = docker ]; then
	expect "$work/doctor.txt" '^ok +sysbox ' '^ok +docker in jobs ' '^ok +registry mirror '
fi
# The release checks ask github.com for boxrunner's latest release, so they
# only warn: a release out after this commit is no fault of the box.
if grep -vE "^(ok  |warn  (release|worker release) )" "$work/doctor.txt" | grep -vE "$idle"; then
	fail "doctor printed the lines above"
fi

step "everything install makes is there, which shows the check after uninstall can see it"
sudo scripts/e2e/leftovers.sh | tee "$work/installed.txt"
expect "$work/installed.txt" '^path /usr/local/bin/boxrunner$' '^path /etc/boxrunner$' '^path /var/lib/boxrunner$' \
	'^path /run/boxrunner.sock$' '^path /etc/systemd/system/boxrunner.service$' '^path /etc/systemd/system/boxrunner_idle.slice$' \
	'^unit boxrunner.service$' '^unit file boxrunner.service$' '^unit file boxrunner_idle.slice$' \
	'^iptables -A INPUT -m comment --comment boxrunner -j BOXRUNNER-INPUT$' \
	'^ip6tables -A INPUT -m comment --comment boxrunner -j BOXRUNNER-INPUT$' \
	'^iptables -A (DOCKER-USER|FORWARD) -i boxrunner0 -m comment --comment boxrunner -j BOXRUNNER-FORWARD$' \
	'^ip6tables -A (DOCKER-USER|FORWARD) -i boxrunner0 -m comment --comment boxrunner -j BOXRUNNER-FORWARD$' \
	'^link boxrunner0$' '^network boxrunner$' '^image boxrunner/runner:local$'
if [ "$mode" = docker ]; then
	expect "$work/installed.txt" '^container boxrunner-mirror$' '^volume boxrunner-mirror$'
fi

step "install again changes nothing"
sudo boxrunner install --worker "$worker" --token test "${flags[@]}" | tee "$work/again.txt"
expect "$work/again.txt" '^No changes: the box is already set up\.$'

step "uninstall removes everything"
sudo boxrunner uninstall | tee "$work/uninstall.txt"
expect "$work/uninstall.txt" '^boxrunner is gone\.'
sudo scripts/e2e/leftovers.sh | tee "$work/left.txt"
[ -s "$work/left.txt" ] && fail "uninstall left the lines above"
command -v boxrunner >/dev/null && fail "boxrunner is still on the PATH at $(command -v boxrunner)"

step "uninstall again finds nothing"
sudo "$PWD/dist/boxrunner-linux-$(go env GOARCH)" uninstall | tee "$work/again.txt"
expect "$work/again.txt" '^Nothing to remove\.$'

printf '\nPASS: install, doctor, install again and uninstall (%s)\n' "$mode"
