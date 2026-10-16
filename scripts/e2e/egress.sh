# shellcheck shell=bash
# shellcheck disable=SC2154 # $mode and $work are run.sh's
# Sourced by run.sh once the agent runs. Gives the class boxrunner-light an
# egress allowlist and checks, from containers on that class's own network
# and with the options the agent gives its jobs, that the class reaches what
# it lists and nothing else: not another name, not an address by number, not
# another DNS server, not the host, not the metadata service, not a private
# service it does not list, and not an address that a job forges as its own
# source. It then stops the agent and deletes the table, and the class stays
# closed, and flushes the whole ruleset with the agent running, stopped and
# killed: no job of the class outlives the rules. It uses $mode, $work,
# $isolation, $version, expect, fail and step.

class=boxrunner-light
case "$mode" in
runc) runtime=() ;;
*) runtime=(--runtime sysbox-runc) ;;
esac

step "a service on a private address, behind a router of its own"
sudo ip netns add e2e-svc
sudo ip link add e2e-v0 type veth peer name e2e-v1
sudo ip link set e2e-v1 netns e2e-svc
sudo ip addr add 10.20.0.1/24 dev e2e-v0
sudo ip link set e2e-v0 up
sudo ip netns exec e2e-svc sh -c 'ip addr add 10.20.0.2/24 dev e2e-v1; ip link set e2e-v1 up; ip link set lo up; ip route add default via 10.20.0.1'
sudo ip netns exec e2e-svc python3 -m http.server 5432 --bind 10.20.0.2 >/dev/null 2>&1 &
for _ in $(seq 30); do
	curl -fsS -o /dev/null --max-time 2 http://10.20.0.2:5432/ 2>/dev/null && break
	sleep 0.5
done
curl -fsS -o /dev/null --max-time 2 http://10.20.0.2:5432/ || fail "the private service does not answer from the host"

# metrics_wait waits for the agent to serve every egress class, as its metrics say.
metrics_wait() {
	for _ in $(seq 90); do
		curl -fsS -o "$work/metrics.txt" http://127.0.0.1:9464/metrics 2>/dev/null &&
			grep -q "^boxrunner_egress_ready{class=\"$class\"} 1\$" "$work/metrics.txt" && return 0
		sleep 1
	done
	sudo journalctl -u boxrunner.service --no-pager | tail -n 40
	fail "the agent does not serve the class $class with its allowlist"
}

step "the class $class gets an egress allowlist; the agent builds its network and rules"
sudo sed -i "/^name = \"$class\"\$/a egress = [\"github\", \"example.com\", \"*.example.org\", \"10.20.0.0/24:5432\"]" /etc/boxrunner/config.toml
# install adds nft where it is missing; the agent starts the class when it restarts.
sudo boxrunner install --worker "$worker" --token test "${flags[@]}" | tee "$work/install-egress.txt"
command -v nft >/dev/null || fail "nft is missing after install"
sudo systemctl restart boxrunner.service
metrics_wait
net=$(sudo docker network ls --filter "label=boxrunner.egress=$class" --format '{{.Name}}')
[ -n "$net" ] || fail "no Docker network carries the label boxrunner.egress=$class"
gateway=$(sudo docker network inspect "$net" --format '{{(index .IPAM.Config 0).Gateway}}')
subnet=$(sudo docker network inspect "$net" --format '{{(index .IPAM.Config 0).Subnet}}')
bridge=$(sudo docker network inspect "$net" --format '{{index .Options "com.docker.network.bridge.name"}}')
open_gateway=$(sudo docker network inspect boxrunner --format '{{(index .IPAM.Config 0).Gateway}}')
echo "network $net, bridge $bridge, subnet $subnet, gateway $gateway"
sudo nft list table inet boxrunner | tee "$work/table.txt" >/dev/null
expect "$work/table.txt" "iifname \"$bridge\" jump egress_" "iifname \"$bridge\" jump host_"
sudo iptables-save | tee "$work/iptables.txt" >/dev/null
expect "$work/iptables.txt" '^-A INPUT -m comment --comment boxrunner -j BOXRUNNER-EGRESS$' \
	'^-A BOXRUNNER-EGRESS -i boxrunner-[0-9a-f]+ -j DROP$' '^-A BOXRUNNER-EGRESS-FWD -m mark ! --mark 0x10000000/0x10000000 -j DROP$' \
	'^-A (DOCKER-USER|FORWARD) -i boxrunner-\+ -m comment --comment boxrunner -j BOXRUNNER-EGRESS-FWD$'

step "doctor proves the policy from a container on the class's network"
sudo boxrunner doctor | tee "$work/doctor-egress.txt" || fail "doctor exited with $?"
expect "$work/doctor-egress.txt" "^ok +egress $class rules " "^ok +egress $class agent " "^ok +egress $class allows github.com resolves and answers" \
	"^ok +egress $class refuses a name example\\.(net|org) " "^ok +egress $class refuses an address " "^ok +egress $class host " "^ok +egress $class resolver "
if grep -vE "^(ok  |warn  (release|worker release|runner release) )" "$work/doctor-egress.txt"; then
	fail "doctor printed the lines above"
fi

# job runs a script on stdin in a container as the agent would start one of the
# class: its network, its DNS and its isolation.
job() {
	sudo docker run -i --rm --network "$net" --dns "$gateway" "${runtime[@]}" \
		-e "GATEWAY=$gateway" -e "OPEN_GATEWAY=$open_gateway" -e "DENIED_IP=$denied_ip" -e "EXAMPLE_IPS=$example_ips" \
		-e "VM_IP=$vm_ip" -e "RESOLVER=$resolver" boxrunner/runner:local "$@"
}
denied_ip=$(getent ahostsv4 example.net | awk 'NR == 1 { print $1 }')
example_ips=$(getent ahostsv4 example.com | awk '{ print $1 }' | sort -u | tr '\n' ' ')
vm_ip=$(hostname -I | awk '{ print $1 }')
resolver=$(awk '/^nameserver/ && $2 !~ /^127\./ { print $2; exit }' /run/systemd/resolve/resolv.conf /etc/resolv.conf 2>/dev/null || true)
if [ -z "$denied_ip" ] || [ -z "$example_ips" ]; then fail "the box cannot resolve example.net and example.com"; fi

step "a job of the class reaches what it lists and is refused the rest"
job bash -s <<'SCRIPT' | tee "$work/egress-job.txt"
set -u
probe() { if timeout 4 bash -c "exec 3<>/dev/tcp/$1/$2" 2>/dev/null; then echo "$3 open"; else echo "$3 closed"; fi; }
ask() { if timeout 3 bash -c "exec 3<>/dev/udp/$1/53; printf '\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x06github\x03com\x00\x00\x01\x00\x01' >&3; head -c1 <&3 >/dev/null" 2>/dev/null; then echo "$2 open"; else echo "$2 closed"; fi; }
status() { echo "$1 $(curl -sS -m 20 -o /dev/null -w '%{http_code}' "$2" 2>/dev/null || true)"; }
for ip in $EXAMPLE_IPS; do probe "$ip" 443 "by-number-before-resolving"; done
status allowed-name https://example.com
status wildcard-name https://www.example.org
status apex-of-wildcard https://example.org
status other-name https://example.net
for ip in $(getent ahostsv4 example.com | awk '{ print $1 }' | sort -u); do probe "$ip" 443 "by-number-after-resolving"; done
probe "$DENIED_IP" 443 denied-by-number
probe "$GATEWAY" 22 host-by-gateway
probe "$VM_IP" 22 host-by-address
probe 169.254.169.254 80 metadata
probe "$OPEN_GATEWAY" 5000 mirror
probe "$OPEN_GATEWAY" 5001 cache
probe 10.20.0.2 5432 private-listed
probe 10.20.0.2 5433 private-other-port
if [ -n "$RESOLVER" ]; then
	probe "$RESOLVER" 53 resolver-tcp
	ask "$RESOLVER" resolver-udp
fi
if getent hosts example.net >/dev/null 2>&1; then echo "lookup-other-name resolved"; else echo "lookup-other-name refused"; fi
SCRIPT
expect "$work/egress-job.txt" '^allowed-name 200$' '^wildcard-name 200$' '^apex-of-wildcard 000$' '^other-name 000$' '^denied-by-number closed$' \
	'^host-by-gateway closed$' '^host-by-address closed$' '^metadata closed$' '^private-listed open$' '^private-other-port closed$' \
	'^by-number-after-resolving open$' '^lookup-other-name refused$'
grep -q '^by-number-before-resolving open$' "$work/egress-job.txt" && fail "a job reached an address by number before it resolved the name"
grep -q '^by-number-after-resolving closed$' "$work/egress-job.txt" && fail "an address of a name the job resolved is closed"
grep -qE '^resolver-(tcp|udp) open$' "$work/egress-job.txt" && fail "a job reached the box's resolver"
# The registry mirror is open to a class with docker = true, the Actions cache
# to one with cache = true, which run.sh gives every class, and nothing else of the host is.
expect "$work/egress-job.txt" '^cache open$'
if [ "$mode" = docker ]; then
	expect "$work/egress-job.txt" '^mirror open$'
else
	expect "$work/egress-job.txt" '^mirror closed$'
fi

step "the other class, which has no allowlist, still cannot reach the private service"
sudo docker run --rm --network boxrunner boxrunner/runner:local bash -c 'timeout 4 bash -c "exec 3<>/dev/tcp/10.20.0.2/5432" 2>/dev/null && echo open || echo closed' | tee "$work/open-class.txt"
expect "$work/open-class.txt" '^closed$'

if [ "$mode" != runc ]; then
	step "root in a job under Sysbox changes its address to the open class's, and is still refused: the class is the bridge"
	job bash -s <<'SCRIPT' | tee "$work/egress-spoof.txt"
set -u
probe() { if timeout 4 bash -c "exec 3<>/dev/tcp/$1/$2" 2>/dev/null; then echo "$3 open"; else echo "$3 closed"; fi; }
old=$(ip -4 -o addr show eth0 | awk '{ print $4 }')
if sudo -n ip addr add 172.18.0.77/16 dev eth0 2>/dev/null; then
	sudo ip addr del "$old" dev eth0
	sudo ip route replace "$GATEWAY" dev eth0 scope link
	sudo ip route replace default via "$GATEWAY" dev eth0 src 172.18.0.77
	echo "spoofing worked"
	probe "$DENIED_IP" 443 spoofed-to-denied
	for ip in $EXAMPLE_IPS; do probe "$ip" 443 spoofed-to-allowed; done
else
	echo "spoofing not possible"
fi
SCRIPT
	expect "$work/egress-spoof.txt" '^spoofing worked$' '^spoofed-to-denied closed$' '^spoofed-to-allowed closed$'
fi

if [ "$mode" = docker ]; then
	step "Docker in a job of the class pulls through the mirror and obeys the same policy"
	sudo docker rm -f e2e-inner >/dev/null 2>&1 || true
	sudo docker run -d --name e2e-inner --network "$net" --dns "$gateway" "${runtime[@]}" \
		-e BOXRUNNER_DOCKER=1 -e "BOXRUNNER_MIRROR=http://$open_gateway:5000" -e "BOXRUNNER_DNS=$gateway" \
		boxrunner/runner:local sleep 900 >/dev/null
	for _ in $(seq 60); do
		sudo docker exec -u runner e2e-inner docker info >/dev/null 2>&1 && break
		sleep 1
	done
	sudo docker exec -u runner e2e-inner docker info >/dev/null 2>&1 || {
		sudo docker logs e2e-inner | tail -n 30
		fail "the job's Docker did not start"
	}
	sudo docker exec -u runner -e "DENIED_IP=$denied_ip" e2e-inner bash -c '
		docker pull -q busybox >/dev/null && echo "pull ok" || echo "pull failed"
		inner() { docker run --rm busybox "$@"; }
		if inner wget -T 15 -q -O /dev/null http://example.com; then echo "inner allowed-name ok"; else echo "inner allowed-name failed"; fi
		if inner wget -T 6 -q -O /dev/null http://example.net; then echo "inner other-name ok"; else echo "inner other-name refused"; fi
		if inner wget -T 6 -q -O /dev/null "http://$DENIED_IP/"; then echo "inner by-number ok"; else echo "inner by-number refused"; fi
		if inner wget -T 6 -q -O /dev/null http://10.20.0.2:5432/; then echo "inner private-listed ok"; else echo "inner private-listed failed"; fi
	' | tee "$work/egress-inner.txt"
	sudo docker rm -f e2e-inner >/dev/null
	expect "$work/egress-inner.txt" '^pull ok$' '^inner allowed-name ok$' '^inner other-name refused$' '^inner by-number refused$' '^inner private-listed ok$'
fi

step "the agent counts and describes what the class was refused"
sudo boxrunner status | tee "$work/status-egress.txt"
expect "$work/status-egress.txt" '^EGRESS CLASS +ENTRIES +PACKETS DROPPED +LOOKUPS REFUSED$' "^$class +4 +[1-9][0-9]* +[1-9][0-9]*\$" \
	'^REFUSED AT +CLASS +RUNNER +REPOSITORY +WHAT$' "lookup of example\\.(net|org)\$" 'packet to [0-9.]+:(22|443|80)'
curl -fsS "http://127.0.0.1:9464/metrics" | tee "$work/metrics-egress.txt" >/dev/null
expect "$work/metrics-egress.txt" "^boxrunner_egress_ready\\{class=\"$class\"\\} 1\$" \
	"^boxrunner_egress_blocked_total\\{class=\"$class\",kind=\"packet\"\\} [1-9][0-9]*\$" \
	"^boxrunner_egress_blocked_total\\{class=\"$class\",kind=\"lookup\"\\} [1-9][0-9]*\$"
sudo journalctl -u boxrunner.service --no-pager | tee "$work/agent-egress.txt" >/dev/null
expect "$work/agent-egress.txt" "msg=\"egress refused a lookup\" class=$class .* name=example\\.(net|org)" "msg=\"egress refused a connection\" class=$class"

# closed runs a container of the class and reports whether it can resolve
# example.com, connect to the addresses it has, and connect to an address of
# example.net, which was never opened.
closed() {
	# shellcheck disable=SC2016 # the container's shell expands $EXAMPLE_IPS
	job bash -c 'getent hosts example.com >/dev/null 2>&1 && echo "resolves" || echo "does not resolve"
		for ip in $EXAMPLE_IPS; do timeout 4 bash -c "exec 3<>/dev/tcp/$ip/443" 2>/dev/null && echo "connects" || echo "does not connect"; done
		timeout 4 bash -c "exec 3<>/dev/tcp/$DENIED_IP/443" 2>/dev/null && echo "reaches another" || echo "does not reach another"'
}

# held starts a container of the class that keeps running, as a job does.
held() {
	sudo docker rm -f e2e-held >/dev/null 2>&1 || true
	sudo docker run -d --name e2e-held --network "$net" --dns "$gateway" "${runtime[@]}" boxrunner/runner:local sleep 900 >/dev/null
}
# gone waits up to $1 seconds for that container to be removed.
gone() {
	for _ in $(seq "$1"); do
		sudo docker inspect e2e-held >/dev/null 2>&1 || return 0
		sleep 1
	done
	return 1
}
# journal writes what the agent logged since $since to a file.
journal() { sudo journalctl -u boxrunner.service --no-pager --since "$since" | tee "$work/$1" >/dev/null; }
backstop() {
	sudo nft list table inet boxrunner >/dev/null || fail "the table is not there"
	sudo iptables-save | tee "$work/iptables-back.txt" >/dev/null
	expect "$work/iptables-back.txt" '^-A BOXRUNNER-EGRESS-FWD -m mark ! --mark 0x10000000/0x10000000 -j DROP$' \
		'^-A (DOCKER-USER|FORWARD) -i boxrunner-\+ -m comment --comment boxrunner -j BOXRUNNER-EGRESS-FWD$' '^-A BOXRUNNER-EGRESS -i boxrunner-[0-9a-f]+ -j DROP$'
}

step "with the agent stopped the class has no DNS and opens nothing; its rules stay"
sudo systemctl stop boxrunner.service
sudo nft list table inet boxrunner >/dev/null || fail "stopping the agent removed the table"
closed | tee "$work/egress-down.txt"
expect "$work/egress-down.txt" '^does not resolve$' '^does not reach another$'
grep -q '^reaches another$' "$work/egress-down.txt" && fail "a job reached an address nobody opened with the agent stopped"
sudo systemctl start boxrunner.service
metrics_wait

step "with the table deleted the class stays closed, and the agent puts the table back and removes the job that ran without it"
held
since=$(date '+%F %T')
sudo nft delete table inet boxrunner
# The agent removes the job within ten seconds, which may cut the probe short. It must never connect.
# shellcheck disable=SC2016 # the container's shell expands $EXAMPLE_IPS and $DENIED_IP
sudo docker exec -e "EXAMPLE_IPS=$example_ips" -e "DENIED_IP=$denied_ip" e2e-held bash -c '
	for ip in $EXAMPLE_IPS; do timeout 2 bash -c "exec 3<>/dev/tcp/$ip/443" 2>/dev/null && echo "connects" || echo "does not connect"; done
	timeout 2 bash -c "exec 3<>/dev/tcp/$DENIED_IP/443" 2>/dev/null && echo "reaches another" || echo "does not reach another"' | tee "$work/egress-flushed.txt" || true
grep -qE '^(connects|reaches another)$' "$work/egress-flushed.txt" && fail "a job connected with the table deleted"
gone 30 || fail "the job that ran without the table still runs"
backstop
journal agent-flushed.txt
expect "$work/agent-flushed.txt" 'msg="the egress rules were gone; put them back"' \
	'msg="removed a job of an egress class" runner=e2e-held .*why="the egress rules were gone while it ran"'
closed | tee "$work/egress-back.txt"
expect "$work/egress-back.txt" '^resolves$' '^connects$' '^does not reach another$'

step "a stopping agent removes the jobs of the class, so none runs without it"
held
since=$(date '+%F %T')
sudo systemctl stop boxrunner.service
gone 5 || fail "a job of the class outlived the agent"
journal agent-stop.txt
expect "$work/agent-stop.txt" 'msg="removed a job of an egress class" runner=e2e-held .*why="the agent is stopping"'

step "with the agent stopped, nft flush ruleset removes every rule; the agent builds them again before it serves the class"
sudo nft flush ruleset
sudo nft list table inet boxrunner >/dev/null 2>&1 && fail "the flush left the table"
sudo systemctl start boxrunner.service
metrics_wait
backstop

step "a job that outlives a crash of the agent is removed when the agent starts again"
held
since=$(date '+%F %T')
pid=$(systemctl show -p MainPID --value boxrunner.service)
sudo systemctl kill -s KILL boxrunner.service
for _ in $(seq 30); do
	now=$(systemctl show -p MainPID --value boxrunner.service)
	[ "$now" != 0 ] && [ "$now" != "$pid" ] && break
	sleep 1
done
metrics_wait
gone 5 || fail "a job of the class still runs after the agent started again"
journal agent-crash.txt
expect "$work/agent-crash.txt" 'msg="removed a job of an egress class" runner=e2e-held .*why="it ran while the agent was not running"'

step "nft flush ruleset with the agent running: the agent puts the rules back and removes the job that ran without them"
held
since=$(date '+%F %T')
sudo nft flush ruleset
flushed=$(date +%s)
# What the job reaches until then is measured, not judged, since it races the
# agent: the flush took the host's rules too, so the agent's metrics port on
# the class's gateway, which the rules drop, may answer.
sudo docker exec -e "GATEWAY=$gateway" e2e-held bash -c 'timeout 3 bash -c "exec 3<>/dev/tcp/$GATEWAY/9464" 2>/dev/null && echo "host metrics after the flush: open" || echo "host metrics after the flush: closed"' || true
gone 30 || fail "the job that ran while the rules were flushed still runs"
echo "the job was removed $(($(date +%s) - flushed)) s after the flush"
backstop
journal agent-flush.txt
expect "$work/agent-flush.txt" 'msg="the egress rules were gone; put them back"' \
	'msg="removed a job of an egress class" runner=e2e-held .*why="the egress rules were gone while it ran"'

step "Docker and the agent put back their own rules, which the flush took, and the class works again"
sudo systemctl restart docker
sudo systemctl restart boxrunner.service
metrics_wait
closed | tee "$work/egress-restored.txt"
expect "$work/egress-restored.txt" '^resolves$' '^connects$' '^does not reach another$'
