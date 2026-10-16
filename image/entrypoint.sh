#!/bin/sh
# Runs the container's command as runner. With BOXRUNNER_DOCKER=1, the job
# runs under Sysbox and first gets a Docker daemon of its own, which pulls
# Docker Hub images through the box's registry mirror at BOXRUNNER_MIRROR and
# resolves names through BOXRUNNER_DNS, the forwarder of a class whose
# network is closed to what its allowlist leaves out.
# With BOXRUNNER_CACHE, the runner's node becomes node.boxrunner, which sends
# the cache calls of JavaScript actions to the box's Actions cache.
set -eu

if [ -n "${BOXRUNNER_CACHE:-}" ]; then
	for bin in /home/runner/externals/node*/bin; do
		if [ -e "$bin/node.boxrunner" ] && ! ln -sfn node.boxrunner "$bin/node" 2>/dev/null; then
			echo "boxrunner: cannot link $bin/node to the cache's node wrapper; this job uses GitHub's cache" >&2
		fi
	done
fi

if [ "$(id -u)" != 0 ]; then
	if [ "${BOXRUNNER_DOCKER:-}" = 1 ]; then
		echo "boxrunner: the image runs as $(id -un), so it cannot start Docker; end the class's Dockerfile with USER root" >&2
	fi
	exec "$@"
fi

if [ "${BOXRUNNER_DOCKER:-}" = 1 ]; then
	rm -f /usr/local/bin/docker
	dockerd --group docker ${BOXRUNNER_MIRROR:+"--registry-mirror=$BOXRUNNER_MIRROR"} ${BOXRUNNER_DNS:+"--dns=$BOXRUNNER_DNS"} >/var/log/dockerd.log 2>&1 &
	dockerd=$!
	waited=0
	until docker info >/dev/null 2>&1; do
		waited=$((waited + 1))
		if [ "$waited" -gt 30 ] || ! kill -0 "$dockerd" 2>/dev/null; then
			echo "boxrunner: the job's Docker daemon did not start:" >&2
			tail -n 20 /var/log/dockerd.log >&2
			exit 1
		fi
		sleep 1
	done
fi

export HOME=/home/runner USER=runner LOGNAME=runner
exec setpriv --reuid=runner --regid=runner --init-groups "$@"
