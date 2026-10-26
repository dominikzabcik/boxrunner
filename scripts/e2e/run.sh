#!/usr/bin/env bash
# Installs boxrunner on this machine with install.sh against a stub Worker,
# checks the box with doctor, installs again expecting no changes, uninstalls
# and checks that nothing is left. It changes the system: run it only on a
# machine you can throw away. CI runs it on GitHub's VMs (.github/workflows/e2e.yml).
#
#   scripts/e2e/run.sh runc|sysbox|docker
#
# runc installs without flags, sysbox with --sysbox and docker with --docker.
# scripts/e2e/egress.sh adds an egress allowlist to one class and checks it.
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
cleanup() {
	kill $stub
	sudo docker rm -f e2e-inner e2e-held >/dev/null 2>&1
	sudo ip netns pids e2e-svc 2>/dev/null | xargs -r sudo kill
	sudo ip netns del e2e-svc 2>/dev/null
	printf "\n### the stub Worker saw\n"
	sort "$work/worker.log" | uniq -c
}
trap cleanup EXIT
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

step "the agent serves metrics on every address and the Actions cache to every class"
sudo sed -i '1i metrics_public = true' /etc/boxrunner/config.toml
sudo sed -i '1i metrics = "0.0.0.0:9464"' /etc/boxrunner/config.toml
sudo sed -i '1i cache = "2GB"' /etc/boxrunner/config.toml
sudo sed -i 's/^\(name = ".*"\)$/\1\ncache = true/' /etc/boxrunner/config.toml
sudo systemctl restart boxrunner.service
for _ in $(seq 60); do
	curl -fsS -o "$work/metrics.txt" http://127.0.0.1:9464/metrics 2>/dev/null &&
		grep -q '^boxrunner_heartbeat_last_success_timestamp_seconds [1-9]' "$work/metrics.txt" && break
	sleep 1
done
expect "$work/metrics.txt" "^boxrunner_agent_info\{box=\"[a-z0-9-]+\",version=\"$version\"\} 1$" \
	"^boxrunner_worker_info\{version=\"$version\"\} 1$" '^boxrunner_class_max_runners\{class="boxrunner"\} [0-9]+$' \
	'^boxrunner_draining 0$' '^boxrunner_disk_free_bytes [1-9][0-9]*$' '^boxrunner_heartbeat_last_success_timestamp_seconds [1-9]'

step "doctor checks the box (systemd $(systemctl --version | awk 'NR == 1 { print $2 }'))"
sudo boxrunner doctor | tee "$work/doctor.txt" || fail "doctor exited with $?"
expect "$work/doctor.txt" '^ok +idle slice +container runs in /boxrunner_idle.slice, cpu.idle 1$' '^ok +config ' '^ok +cgroup driver +systemd' '^ok +firewall rules +in place for IPv4 and IPv6' \
	'^ok +host +the host at .* is unreachable from jobs' '^ok +metadata +169.254.169.254 is unreachable from jobs' \
	'^ok +private ' '^ok +metrics +the metrics port at [0-9.]+:9464 is unreachable from jobs$' '^ok +github ' "^ok +worker +$worker accepts the token; 0 repositories, classes boxrunner, boxrunner-light$" \
	'^ok +actions cache +actions in a job reach the cache at http://[0-9.]+:5001$' \
	"^ok +isolation boxrunner +$isolation" "^ok +isolation boxrunner-light +$isolation"
if [ "$mode" = docker ]; then
	expect "$work/doctor.txt" '^ok +sysbox ' '^ok +docker in jobs ' '^ok +registry mirror '
fi
# The release checks ask github.com for boxrunner's and the runner's latest
# releases, so they only warn: a release out after this commit, or one the
# runner digests workflow has not verified yet, is no fault of the box. The
# default classes may be allowed more memory than the box has when each runs at
# its max, so the memory check warns here.
expect "$work/doctor.txt" '^(ok|warn) +memory ' '^ok +runner image +sha256:[0-9a-f]{64}, which boxrunner.s CI verified$'
if grep -vE "^(ok  |warn  (release|worker release|runner release|memory) )" "$work/doctor.txt"; then
	fail "doctor printed the lines above"
fi

step "a container on the job network connects out; the agent records where, and not what the firewall dropped"
sudo docker run --rm --network boxrunner boxrunner/runner:local bash -c 'curl -sS -m 30 -o /dev/null https://github.com; curl -sS -m 3 -o /dev/null http://169.254.169.254/ || true'
for _ in $(seq 20); do
	sudo boxrunner status | awk '/^REPOSITORY +FIRST SEEN/ { in_table = 1 } in_table { print } /^Up to / { in_table = 0 }' >"$work/status-destinations.txt"
	grep -qE '^\? +[A-Z][a-z]{2} [0-9]{2} [0-9:]{5} +[0-9]+ +[0-9.]+:443$' "$work/status-destinations.txt" && break
	sleep 1
done
expect "$work/status-destinations.txt" '^REPOSITORY +FIRST SEEN +CONNECTIONS +DESTINATION$' \
	'^\? +[A-Z][a-z]{2} [0-9]{2} [0-9:]{5} +[0-9]+ +[0-9.]+:443$'
grep -q '169\.254\.169\.254' "$work/status-destinations.txt" && fail "status lists the metadata service, which the firewall dropped"
sudo journalctl -u boxrunner.service --no-pager | tee "$work/agent-destinations.txt" >/dev/null
expect "$work/agent-destinations.txt" 'msg="a job connected somewhere new" repo="" runner=[^ ]+ dest=[0-9.]+:443'
sudo nft list table inet boxrunner_seen | tee "$work/seen-table.txt" >/dev/null
expect "$work/seen-table.txt" 'iifname "boxrunner0" ct state new .*log group 4138' 'iifname "boxrunner-\*" ct state new .*log group 4138'

# The workflow hands over its own runtime token, which GitHub signs, so the
# cache checks it as it would a job's.
if [ -n "${RUNTIME_TOKEN_FILE:-}" ]; then
	step "a job saves to the Actions cache and the next restores it; artifacts go on to GitHub"
	gateway=$(sudo docker network inspect boxrunner --format '{{(index .IPAM.Config 0).Gateway}}')
	results=$(cat "$RESULTS_URL_FILE")
	key="e2e-cache-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}-$mode-$(uname -m)-$(lsb_release -rs)"
	{
		echo "BOXRUNNER_CACHE=http://$gateway:5001"
		echo "ACTIONS_RESULTS_URL=$results"
		echo "ACTIONS_RUNTIME_TOKEN=$(cat "$RUNTIME_TOKEN_FILE")"
		echo "ACTIONS_CACHE_SERVICE_V2=true"
		echo "KEY=$key"
	} >"$work/job.env"
	chmod 600 "$work/job.env"
	# job runs the script on stdin, cache-client.mjs, once per call in a job
	# container, with the env the runner gives a JavaScript action.
	job() {
		sudo docker run -i --rm --network boxrunner --env-file "$work/job.env" boxrunner/runner:local bash -c '
			set -euo pipefail
			mkdir -p /tmp/job && cd /tmp/job && cat >client.mjs
			npm install --silent --no-audit --no-fund @actions/cache@6.3.0 @actions/artifact@6.3.1 >/dev/null
			export GITHUB_WORKSPACE=$PWD RUNNER_TEMP=/tmp
			for call in "$@"; do node client.mjs "$call" "$KEY"; done' job "$@"
	}
	job save <scripts/e2e/cache-client.mjs | tee "$work/save.txt"
	expect "$work/save.txt" "^results-url http://$gateway:5001/$" '^saved [1-9][0-9]*$'
	job restore artifact <scripts/e2e/cache-client.mjs | tee "$work/restore.txt"
	expect "$work/restore.txt" "^restored $key$" "^payload saved by $key$" '^artifact [1-9][0-9]*$'
	sudo journalctl -u boxrunner.service --no-pager | tee "$work/agent.txt" >/dev/null
	expect "$work/agent.txt" "msg=\"cache: saved\" repo=[0-9]+ scope=refs/[^ ]+ key=$key bytes=[1-9]"

	step "with the agent stopped, a job's actions use GitHub's results service"
	sudo systemctl stop boxrunner.service
	job miss <scripts/e2e/cache-client.mjs | tee "$work/fallback.txt"
	expect "$work/fallback.txt" "^results-url $results$" '^restored undefined$'
	sudo systemctl start boxrunner.service
fi

# shellcheck source=scripts/e2e/egress.sh
. scripts/e2e/egress.sh

step "the idle slice stays idle through a daemon-reload, and the agent makes a new one idle"
idle=/sys/fs/cgroup/boxrunner_idle.slice/cpu.idle
sudo systemctl daemon-reload
[ "$(cat $idle)" = 1 ] || fail "cpu.idle is $(cat $idle) after a daemon-reload"
sudo systemctl stop boxrunner.service boxrunner_idle.slice
sudo systemctl start boxrunner.service
for _ in $(seq 30); do
	[ "$(cat $idle 2>/dev/null)" = 1 ] && break
	sleep 1
done
[ "$(cat $idle 2>/dev/null)" = 1 ] || fail "cpu.idle is not 1 after the slice and the agent restarted"

step "everything install makes is there, which shows the check after uninstall can see it"
sudo scripts/e2e/leftovers.sh | tee "$work/installed.txt"
expect "$work/installed.txt" '^path /usr/local/bin/boxrunner$' '^path /etc/boxrunner$' '^path /var/lib/boxrunner$' \
	'^path /run/boxrunner.sock$' '^path /etc/systemd/system/boxrunner.service$' '^path /etc/systemd/system/boxrunner_idle.slice$' \
	'^unit boxrunner.service$' '^unit file boxrunner.service$' '^unit file boxrunner_idle.slice$' \
	'^iptables -A INPUT -m comment --comment boxrunner -j BOXRUNNER-INPUT$' \
	'^ip6tables -A INPUT -m comment --comment boxrunner -j BOXRUNNER-INPUT$' \
	'^iptables -A (DOCKER-USER|FORWARD) -i boxrunner0 -m comment --comment boxrunner -j BOXRUNNER-FORWARD$' \
	'^ip6tables -A (DOCKER-USER|FORWARD) -i boxrunner0 -m comment --comment boxrunner -j BOXRUNNER-FORWARD$' \
	'^iptables -A INPUT -m comment --comment boxrunner -j BOXRUNNER-EGRESS$' '^nft table inet boxrunner$' '^nft table inet boxrunner_seen$' \
	'^link boxrunner0$' '^link boxrunner-[0-9a-f]{5}$' '^network boxrunner$' '^network boxrunner-boxrunner-light$' '^image boxrunner/runner:local$'
expect "$work/installed.txt" '^image ghcr\.io/actions/actions-runner:[0-9]+\.[0-9]+\.[0-9]+$'
if [ "$mode" = docker ]; then
	expect "$work/installed.txt" '^container boxrunner-mirror$' '^volume boxrunner-mirror$' '^image registry@sha256:[0-9a-f]{64}$'
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
