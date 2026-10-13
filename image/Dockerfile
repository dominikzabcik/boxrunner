# The job image: GitHub's official runner image plus what most workflows
# expect from GitHub's hosted Ubuntu runners. The agent builds it on the box
# with RUNNER_VERSION set to the runner's latest release.
ARG RUNNER_VERSION
FROM ghcr.io/actions/actions-runner:${RUNNER_VERSION}
ARG RUNNER_VERSION
LABEL boxrunner.runner-version=${RUNNER_VERSION}

USER root
RUN apt-get update -qq \
	&& DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
		build-essential ca-certificates curl git jq python3 python3-pip python3-venv unzip zstd >/dev/null \
	&& rm -rf /var/lib/apt/lists/*

# setup-node, setup-python and friends install into the tool cache.
RUN install -d -o runner -g runner /opt/hostedtoolcache
ENV RUNNER_TOOL_CACHE=/opt/hostedtoolcache

# Jobs get no Docker daemon in this version; the shim says so instead of
# failing with a missing socket.
COPY docker /usr/local/bin/docker

USER runner
WORKDIR /home/runner
CMD ["/home/runner/run.sh"]
