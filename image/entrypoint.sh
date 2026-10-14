#!/bin/sh
# Runs the container's command as runner. With BOXRUNNER_DOCKER=1, the job
# runs under Sysbox and first gets a Docker daemon of its own, which pulls
# Docker Hub images through the box's registry mirror at BOXRUNNER_MIRROR.
set -eu

if [ "$(id -u)" != 0 ]; then
	if [ "${BOXRUNNER_DOCKER:-}" = 1 ]; then
		echo "boxrunner: the image runs as $(id -un), so it cannot start Docker; end the class's Dockerfile with USER root" >&2
	fi
	exec "$@"
fi

if [ "${BOXRUNNER_DOCKER:-}" = 1 ]; then
	rm -f /usr/local/bin/docker
	dockerd --group docker ${BOXRUNNER_MIRROR:+"--registry-mirror=$BOXRUNNER_MIRROR"} >/var/log/dockerd.log 2>&1 &
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
