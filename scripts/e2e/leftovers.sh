#!/usr/bin/env bash
# Lists what boxrunner has on this machine, one line each: its files, systemd
# units, the idle slice's cgroup, firewall rules, Docker objects and the bridge.
# Prints nothing where boxrunner is not installed. Run it as root.
set -uo pipefail

for path in /usr/local/bin/boxrunner /etc/boxrunner /var/lib/boxrunner /run/boxrunner.sock \
	/etc/systemd/system/boxrunner.service /etc/systemd/system/boxrunner_idle.slice \
	/sys/fs/cgroup/boxrunner_idle.slice; do
	[ -e "$path" ] && echo "path $path"
done

systemctl list-units --all --plain --no-legend 'boxrunner*' | awk '{ print "unit " $1 }'
systemctl list-unit-files --plain --no-legend 'boxrunner*' | awk '{ print "unit file " $1 }'

# Docker's own rules for the bridge name it too, so they count.
for cmd in iptables ip6tables; do
	"$cmd-save" | grep -i boxrunner | sed "s/^/$cmd /"
done

ip -o link show boxrunner0 >/dev/null 2>&1 && echo "link boxrunner0"
ip -o link show 2>/dev/null | awk -F': ' '$2 ~ /^boxrunner-/ { print "link " $2 }'
nft list table inet boxrunner >/dev/null 2>&1 && echo "nft table inet boxrunner"
docker network ls --filter label=boxrunner.egress --format 'network {{.Name}}'
docker network inspect boxrunner >/dev/null 2>&1 && echo "network boxrunner"
docker ps -a --filter label=boxrunner.box --format 'container {{.Names}}'
docker ps -a --filter label=boxrunner.mirror --format 'container {{.Names}}'
docker volume ls --filter label=boxrunner.mirror --format 'volume {{.Name}}'
docker images --filter label=boxrunner.image --format 'image {{.Repository}}:{{.Tag}}'
exit 0
