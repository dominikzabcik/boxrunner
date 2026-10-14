# The job image: GitHub's official runner image plus what most workflows
# expect from GitHub's hosted Ubuntu runners. The agent builds it on the box
# with RUNNER_VERSION set to the runner's latest release.
ARG RUNNER_VERSION
FROM ghcr.io/actions/actions-runner:${RUNNER_VERSION}
ARG RUNNER_VERSION
LABEL boxrunner.runner-version=${RUNNER_VERSION}

USER root
# gh, as on GitHub's Ubuntu image, from GitHub's own package repository.
RUN install -d -m 755 /etc/apt/keyrings \
	&& curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli.gpg \
	&& echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list
RUN apt-get update -qq \
	&& DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
		build-essential ca-certificates curl gh git jq python3 python3-pip python3-venv shellcheck unzip zstd >/dev/null \
	&& rm -rf /var/lib/apt/lists/*

# GitHub's Ubuntu image has node on the PATH, and tools such as CodeQL's
# JavaScript extractor call it without setup-node. The runner ships a current
# Node LTS for JavaScript actions; this exposes that one.
RUN for tool in node npm npx; do ln -sf /home/runner/externals/node24/bin/$tool /usr/local/bin/$tool; done \
	&& node --version

# setup-node, setup-python and friends install into the tool cache.
RUN install -d -o runner -g runner /opt/hostedtoolcache
ENV RUNNER_TOOL_CACHE=/opt/hostedtoolcache

# Jobs get no Docker daemon in this version; the shim says so instead of
# failing with a missing socket.
COPY docker /usr/local/bin/docker

USER runner
WORKDIR /home/runner
CMD ["/home/runner/run.sh"]
