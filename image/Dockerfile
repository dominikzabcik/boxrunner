# The job image: GitHub's official runner image plus what most workflows
# expect from GitHub's hosted Ubuntu runners. The agent builds it on the box
# with RUNNER_VERSION set to the runner's latest release and RUNNER_DIGEST to
# the digest of its image, which boxrunner's CI verified.
ARG RUNNER_DIGEST
FROM ghcr.io/actions/actions-runner@${RUNNER_DIGEST}
ARG RUNNER_VERSION
LABEL boxrunner.runner-version=${RUNNER_VERSION}

USER root
# gh, as on GitHub's Ubuntu image, from GitHub's own package repository.
RUN install -d -m 755 /etc/apt/keyrings \
	&& curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli.gpg \
	&& echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list
RUN apt-get update -qq \
	&& DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
		build-essential ca-certificates curl gh git iproute2 iptables jq python3 python3-pip python3-venv shellcheck unzip zstd >/dev/null \
	&& rm -rf /var/lib/apt/lists/*

# GitHub's Ubuntu image has node on the PATH, and tools such as CodeQL's
# JavaScript extractor call it without setup-node. The runner ships a current
# Node LTS for JavaScript actions; this exposes that one.
RUN for tool in node npm npx; do ln -sf /home/runner/externals/node24/bin/$tool /usr/local/bin/$tool; done \
	&& node --version

# npm install --global works without sudo, as on GitHub's runners: global
# packages land in the runner's home, whose bin directory is on the PATH.
ENV NPM_CONFIG_PREFIX=/home/runner/.npm-global
ENV PATH=/home/runner/.npm-global/bin:$PATH

# The Compose plugin, as on GitHub's Ubuntu image, next to the image's buildx.
RUN case "$(dpkg --print-architecture)" in \
		amd64) arch=x86_64 sum=db1889184726840f75c4f9c001048430d4f25b3be3cb084d3ddd762bc0aed576 ;; \
		arm64) arch=aarch64 sum=732e3a84c1a0f67256ce80bc2598a24546b10ca05f9faa97efceb1171ece2ef7 ;; \
		*) echo "no Compose binary for $(dpkg --print-architecture)" >&2; exit 1 ;; \
	esac \
	&& curl -fsSL -o /usr/local/lib/docker/cli-plugins/docker-compose "https://github.com/docker/compose/releases/download/v5.5.1/docker-compose-linux-${arch}" \
	&& echo "${sum}  /usr/local/lib/docker/cli-plugins/docker-compose" | sha256sum -c - \
	&& chmod 755 /usr/local/lib/docker/cli-plugins/docker-compose \
	&& docker compose version

# setup-node, setup-python and friends install into the tool cache.
RUN install -d -o runner -g runner /opt/hostedtoolcache
ENV RUNNER_TOOL_CACHE=/opt/hostedtoolcache

# A class with cache = true sends its actions' cache calls to the box. The
# entrypoint of such a job links each node the runner ships to
# node.boxrunner, which runs node.real, a second name for the same binary.
# Jobs of other classes run the runner's node as it is.
COPY node.boxrunner /usr/local/lib/boxrunner/node.boxrunner
RUN for bin in /home/runner/externals/node*/bin; do \
		case "$bin" in *_alpine/bin) continue ;; esac; \
		ln "$bin/node" "$bin/node.real" \
		&& cp /usr/local/lib/boxrunner/node.boxrunner "$bin/node.boxrunner" || exit 1; \
	done \
	&& ls /home/runner/externals/node*/bin/node.boxrunner

# The runner image ships Docker's engine. A class with docker = true runs
# under Sysbox, and the entrypoint starts a daemon for the job and removes
# this shim; other jobs get the shim, which says why there is no daemon.
COPY docker /usr/local/bin/docker
# The entrypoint starts as root and runs the job as runner.
COPY entrypoint.sh /usr/local/bin/boxrunner-entrypoint

USER root
WORKDIR /home/runner
ENTRYPOINT ["/usr/local/bin/boxrunner-entrypoint"]
CMD ["/home/runner/run.sh"]
