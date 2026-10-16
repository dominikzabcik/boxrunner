# Decisions

Why boxrunner is built the way it is, newest last. Each entry names its evidence. Probes ran against github.com on October 13, 2026, from a throwaway private repository on a personal account. They used the `actions/scaleset` Go client with a personal access token.

## Why this exists

A personal website ran Lighthouse, Playwright and CodeQL on every pull request in a private repository. In October 2026 it used GitHub Free's 2,000 private minutes in twelve days, and with a $0 spending limit every job stopped. A 4-core ARM VPS that was already paid for and mostly idle now runs those jobs. A whole pull request takes 12 to 13 minutes there, against about 15 on GitHub's runners. boxrunner is that setup, generalized and made safe for other people to run.

## Runner scale sets, not fixed slots

The first version kept a fixed number of runners waiting, each registered for one repository. That cannot burst, and every repository needs its own slots. Runner scale sets let GitHub tell the box how many jobs wait, through a long poll the box opens. The box needs no open port.

- A repository-level scale set works on a personal account's repository (probe: job queued 21:26:18, started 21:26:26 on a cold container).
- Two repositories of one account can each have a scale set with the same name (probe).
- A job queued while no session exists arrives as assigned when a session opens, so a box that was down picks up what waited (probe).

## The App key stays in the Worker

Registering runners for a personal repository needs the App's "Administration: write" permission. That permission can also delete the repository. The box may be shared with other software and other people, so it gets only:

- **A box token.** With it, the Worker answers only for private repositories the App is installed on and for the Worker's own classes.
- **A session token per scale set.** It reads that scale set's queue, acknowledges messages and acquires jobs. Asked to mint a runner config, read, list or delete a scale set, or open a session, it is refused with "Invalid Claims", where the admin token succeeds (probe).

The admin token behind these calls lives ten minutes and the session token seventy (probe, decoded from the tokens).

## No delete-and-recreate for recovery

A session left open by a crash blocks every new session on that scale set with 409 (probe). Two ways out:

- **Close it.** The admin token can close the session by its id (probe). The agent closes its sessions on every normal stop.
- **Wait.** GitHub released an abandoned session after about a minute (probe, one sample, checked every 30 seconds). After a crash the agent retries with backoff.

Deleting the scale set and creating it again would clear the block. But a job already assigned to the deleted scale set stayed queued, and a new scale set with the same name and label did not take it within two minutes (probe). So boxrunner never does that.

## Runners start when a job waits

Keeping one idle runner per repository and class does not fit a small box. Ten repositories and two classes would mean twenty idle runners, while the main class may allow one runner at a time. Runners start when GitHub assigns a job. One cold start took 8 seconds from queue to start on a laptop with the image already pulled (probe). A runner that has not taken a job within three minutes is removed; one minted and never started can be removed (probe).

A class can keep one warm runner for each repository it names (`warm`). On a 4-core x86 box, three jobs in a row on a warm repository started 2 seconds after they were queued, each on a runner that refilled after the previous job; cold starts there take 8 to 12 seconds.

## Idle-priority class

On a 4-core ARM box, four timed threads took 1,319 ms alone, 1,345 ms beside four busy threads in a top-level SCHED_IDLE slice, and 6,536 ms beside four busy threads at normal priority. A first attempt named the slice `runners-idle.slice`. systemd read the dash as nesting and put it inside `runners.slice`, where it was idle only against its siblings and still took half the CPU. The slice is therefore named `boxrunner_idle.slice`, and `doctor` checks `cpu.idle` on it.

`boxrunner bench` repeats the measurement with containers: on a 4-core ARM VPS a normal-priority job took 428 ms alone, 429 ms (+0%) beside four busy idle-class containers and 2,096 ms (+390%) beside four busy normal ones; on a 4-core x86 VPS 1,393 ms, 1,438 ms (+3%) and 6,905 ms (+396%).

## Setup in the browser, behind a secret

The private key goes from GitHub straight to the Worker through the App manifest flow, so nobody copies it. A freshly deployed Worker is reachable by anyone, so its setup page requires a secret set at deploy time. It signs the manifest's `state` and ties it to a cookie, and it works once. Both design reviews found the unguarded version claimable by whoever came first.

## Docker inside jobs with Sysbox

Jobs that use `services:`, `container:`, Docker container actions or `docker build` need a Docker daemon. Mounting the box's Docker socket would give every job root on the box, and a privileged container would too. Sysbox runs a container with root mapped to an unprivileged range on the host and lets it run its own Docker. It was tested in a fresh Ubuntu 24.04 arm64 VM (kernel 7.0, Docker 29.1.3) with Sysbox 0.7.1:

- `docker:dind`, not privileged, under `sysbox-runc` with `--cpus 2 --memory 2g --cgroup-parent boxrunner_idle.slice` ran `docker run alpine` and `docker build` inside. Its root was uid 100000 on the host (uid map `0 100000 65536`), the idle slice still read `cpu.idle 1`, and the container's `cpu.max` read `200000 100000`.
- The job image under `sysbox-runc` on the `boxrunner` network, as the `runner` user, ran `docker run`, `docker build` and a Postgres container that accepted connections. Its containers reached github.com, and connections from them to the host and the metadata address timed out after 3 s, so the firewall applies to them.
- The official runner image already ships Docker's engine (`dockerd` 29.8.2 in runner 2.338.0), so the job image adds only `iptables`.

Sysbox's package refuses to install while Docker has any container, unless `/etc/docker/daemon.json` already sets `bip` and `default-address-pools`; otherwise it sets its own and restarts Docker. `install` therefore writes Docker's current bridge address and address pools (Docker's built-in ones when none are set) into that file first. On a box with a running container, `install --docker` then left Docker's process and the container untouched, and the package only made Docker reload its configuration to add the runtime.

Every job starts with an empty Docker, so images are pulled cold; the next section adds a mirror on the box.

## A registry mirror for Docker in jobs

Docker Hub allows 100 anonymous pulls per six hours per IP, and each docker job pulled its service images from it again. A `registry` 2.8.3 container in proxy mode now caches Docker Hub on the box, and each job's Docker uses it as its registry mirror. Measured in an Ubuntu 24.04 arm64 VM (Docker 29.1.3, Sysbox 0.7.1), with job containers started as the agent starts them:

- `docker pull postgres:17-alpine` took 19.6 s in the first job. The mirror fetched the image from Docker Hub and logged 17 new cache entries.
- The same pull took 4.9 s in a second job, started after the first was removed. The mirror answered its 18 requests from the cache and logged no new entries.
- The mirror runs on the host's network and listens only on the `boxrunner0` gateway, so jobs reach it as the host and the firewall needs one exception: TCP to that address and port. From a job, the mirror answered and every other port on the host timed out, including a server on all addresses and one on the gateway's next port.
- Listening on the gateway alone did not keep others out: a container on Docker's default bridge reached the mirror. The input chain now sees everything the host receives and drops traffic to the mirror that does not come from the bridge; the same container then timed out.

Images inside a docker job do not show in Docker's own size for its container. Sysbox keeps a job's `/var/lib/docker` on the host in `/var/lib/sysbox/docker/<container id>`; after that pull it held 407 MB, while Docker said the container wrote 49 kB. The disk floor now adds those directories to each container's size. Sysbox deleted the directory after the container was removed.

The job image adds the Docker Compose plugin, which GitHub's Ubuntu image also has; `docker compose up` ran a one-service project in a job.

## Warm runners are chosen per repository

A class's `warm` list keeps one idle runner ready for each named repository, so its jobs skip the container start. It is opt-in per repository because the reason above still holds: a personal account with many repositories cannot keep one idle runner each on a small box, and every warm runner holds one of the class's `max` places for as long as it waits. A warm runner never takes the place a waiting job needs: the scheduler refills a warm runner only after every waiting job of the class has a place, and the configuration refuses a `warm` list that would fill `max` while the class serves other repositories. A warm runner is not evicted when another repository's job waits; removing a registered runner and starting it again later would cost the cold start warm runners exist to avoid. Runners of one scale set are interchangeable, so the scheduler does not track which runner is warm; while the scale set has more runners than jobs, its newest runner without a job is the warm one.

## Fallback flips a repository variable

A workflow cannot say "this label, or GitHub's runners if nobody serves it": every label in `runs-on` must match. So the choice has to be made before GitHub queues the job, from something `runs-on` can read: `github`, `needs`, `strategy`, `matrix`, `vars` and `inputs`. Three ways were weighed:

- **The Worker sets a repository variable (chosen).** The Worker already knows when heartbeats stop. It sets `BOXRUNNER_DOWN` in every served repository and deletes it when the box is back, and a workflow reads `vars.BOXRUNNER_DOWN` in `runs-on`. It costs nothing while the box is up. It needs the App's Variables permission, which existing installations must approve, so it is opt-in (`FALLBACK_MINUTES`) and the setup manifest does not ask for it.
- **A first job on GitHub's runner asks the Worker** which label to use, and the real jobs read its output through `needs`. It needs no new permission, but every run on a private repository pays a billed minute and a hosted runner's start, also while the box is healthy. That spends the free minutes boxrunner exists to save, and when they run out with a $0 spending limit, the first job cannot run and nothing runs. It would also need a Worker endpoint that answers without the box token.
- **Documentation only**: flip the variable by hand with `gh variable set`. It needs no code and no permission, but nobody flips it at 3 a.m. The docs keep it as the way to do it without the permission.

The Worker wakes itself with a Durable Object alarm rather than a cron trigger. A cron trigger in `wrangler.jsonc` would run in every deployment, opted in or not, and take one of the free plan's five cron triggers per account. The alarm is set only while fallback is on and fires once every `FALLBACK_MINUTES` while the box reports. The variable goes back to the box only after two minutes of heartbeats without a gap, so a box that keeps crashing does not take jobs between crashes. Jobs GitHub already queued for the box's label stay there: GitHub picks the runner at queue time, and the App has no Actions permission to cancel and re-run them.

## Docker's live-restore stays off

Docker stops every container when its daemon stops, unless `live-restore` is on in `daemon.json` ([Docker's docs](https://docs.docker.com/engine/daemon/live-restore/)). Turning it on would let jobs survive an upgrade of Docker's packages. `install` does not turn it on, for two reasons:

- **Sysbox breaks DNS in containers that survive.** Sysbox rewrites the DNS rules inside a container on a user-defined network, as every job is on `boxrunner`. A restart of Docker with live-restore puts Docker's own rules back over them, and DNS inside the container stops working. Sysbox's maintainers reproduced this in [nestybox/sysbox#270](https://github.com/nestybox/sysbox/issues/270), which has been open since April 2021. Its workaround, starting `sysbox-mgr` with `alias-dns=false`, turns off what that flag's own help calls "required for system containers on user-defined Docker bridge networks".
- **It does not cover the cases that matter most.** A reboot stops every container anyway. Docker restores containers only across patch releases, and may not after a change to daemon options such as `bip`, which `install` writes.

Sysbox's documentation does not mention live-restore, and it was not tried on a box with boxrunner. `boxrunner drain` covers every case instead: no job runs when Docker or the box restarts. `doctor` shows whether live-restore is on, for a box whose owner turned it on.

## The runner image's provenance is verified in CI, not on the box

actions/runner publishes a Sigstore bundle with SLSA provenance for each `ghcr.io/actions/actions-runner` release from 2.323.0 on, signed by `https://github.com/actions/runner/.github/workflows/release.yml@refs/heads/releases/m<minor>` (`m327` for both 2.327.0 and 2.327.1) and naming the tag's multi-platform index digest. GitHub's attestation API serves them (`/repos/actions/runner/attestations/{digest}`); `ghcr.io` has none. 2.330.0 is the exception, checked on October 15, 2026: five days after the release, a manual run of `docker-publish.yml` on `main` built its image again and moved the tag, so the digest it points to has only that workflow's attestation. Two places to verify the bundles were weighed:

- **On every box, with sigstore-go** ([#71](https://github.com/dominikzabcik/boxrunner/pull/71)). Each build verifies the image it pulls, and the box needs no other tool. But the agent, which runs as root, grew from 8.2 MB to 20.1 MB on amd64 (7.6 MB to 18.8 MB on arm64) and from 1 to 72 linked modules: go-openapi behind Rekor's generated models, gRPC, OpenTelemetry, protobuf and a TUF client among them. Every box would trust 71 more modules with root to guard against one thing, a moved runner tag.
- **In CI, with gh (chosen).** A daily workflow on GitHub's runners verifies each release with `gh attestation verify` and publishes the verified digests. The agent builds only a listed digest, and only while the tag points to it. It gains no dependency and no size. The trust moves to this repository, which boxes already trust for the agent and its installer. The price is a moving part, the workflow and its branch, and an online dependency: a new runner release waits up to a day for the list, and a box cannot build an image, or be installed, while the list is unreachable.

The agent does not check the list's own attestation, since that takes the same Sigstore verifier. The attestation is there for people: anyone can check a copy of the list with `gh attestation verify`, and the `digests` branch keeps every list it published, each commit naming the workflow run that made it. The list lives on a branch, read through raw.githubusercontent.com, rather than in a release: a release would show among boxrunner's releases and could become the latest one, which the agent reads to find boxrunner's own version, and replacing a release asset deletes it first, so a box could find none.

The workflow publishes nothing when any release fails, so a brief failure, such as an outage of the attestation API, cannot drop a release a box builds and make it go back to an older one. A release that fails for a known reason is named in the script and left out, as 2.330.0 is. `--cert-identity` binds the release branch; `--signer-workflow` names only the workflow file. If actions/runner renames its release branches, the workflow fails, boxes keep building the newest listed release, and `doctor` warns that the latest one waits.

## An Actions cache on the box

Since February 2025, `actions/cache` talks only to GitHub's cache service v2, at the address in `ACTIONS_RESULTS_URL` ([config.ts](https://github.com/actions/toolkit/blob/7e25f67e8fd17369aa21e6e816ac99654106e0dc/packages/cache/src/internal/config.ts)). Every restore downloads its archive from GitHub and every save uploads one, though the box has disk to keep them. A class with `cache = true` keeps them on the box.

**Where the address comes from.** The runner sets `ACTIONS_RESULTS_URL` for each JavaScript and Docker action from the job's connection data, after it merges the job's env ([NodeScriptActionHandler.cs](https://github.com/actions/runner/blob/796cc18ffaaafd5cea525d6231f5a8c93c11c603/src/Runner.Worker/Handlers/NodeScriptActionHandler.cs)), so neither env, `GITHUB_ENV` nor a job hook can change it. The same variable carries artifact uploads and downloads. Three ways in were weighed:

- **Rename the variable in `Runner.Worker.dll`**, as [github-actions-cache-server's docs](https://gha-cache-server.falcondev.io/getting-started) do with `sed`. Actions on the host then inherit the variable from the runner's own environment. But in a `container:` job the runner hands an action only the variables it set, one `docker exec -e` per key ([StepHost.cs](https://github.com/actions/runner/blob/796cc18ffaaafd5cea525d6231f5a8c93c11c603/src/Runner.Worker/Handlers/StepHost.cs)), so those actions, and Docker container actions, would get no results address at all, and `upload-artifact` would fail there.
- **`NODE_OPTIONS` with a preload** that changes the variable. It would survive the runner's self-update, but a workflow that sets `NODE_OPTIONS` for its own reasons replaces it without a word, and every node process of every step would load it. The runner already refuses `NODE_OPTIONS` from `GITHUB_ENV`.
- **A wrapper around the runner's node (chosen).** The image keeps each node binary under a second name, `node.real`, and adds `node.boxrunner`. Only in a class with `cache = true` does the entrypoint link `node` to the wrapper, so other classes run the binary they always did. The wrapper decides per action: it sends the action to the box's cache only when the cache answers within two seconds and names the results service the runner gave as its upstream. A `container:` job runs the wrapper in its own container, where `BOXRUNNER_CACHE` is not set, so it runs node unchanged. A runner that updates itself brings its own node without the wrapper and uses GitHub's cache.

**Our own server.** github-actions-cache-server (MIT) speaks the same protocol, but every job's runtime token would pass through a third party's Node.js container, and its upload URLs carry a 10-digit number ([helpers.ts](https://github.com/falcondev-oss/github-actions-cache-server/blob/c87603b2180084160ede63f9d096a095cada1d8c/lib/helpers.ts)), so a job that guessed it could write into another repository's open upload. The agent's cache is a few hundred lines of Go with no new dependency. It checks each token's RS256 signature against the keys at `token.actions.githubusercontent.com` with Go's standard library, takes the repository id and the cache scopes (`ac`) from it, as that server does, and gives every upload and download URL 256 random bits. It joins blocks in the order of the block list, where that server reads each block's place from its id.

**Measured.** In a prototype, `@actions/cache` 6.3.0 with `@azure/storage-blob` 12.31.0 as npm installed them, saved a 396-byte archive as one `PUT` and a 200 MiB one as 64 MiB blocks and a block list, then restored both, the large one through a restore key, byte for byte. A pull request's token restored the default branch's entry and saved into its own scope, and another repository's token missed. On GitHub's Ubuntu 22.04 and 24.04 runners, amd64 and arm64, under runc, under Sysbox and with Docker in jobs, each with the job's own runtime token: doctor saw an action in a job reach the cache, a job container saved an entry through the wrapper and a second one restored it, `@actions/artifact` 6.3.1 uploaded an artifact through the passthrough, and with the agent stopped an action saw `https://results-receiver.actions.githubusercontent.com/` again and its restore missed without an error.

**What stays open.** An artifact call that a step makes while the agent restarts fails its step: `@actions/artifact` retries server errors but not a refused connection. The agent stops the cache last and gives calls in flight ten seconds, and the docs say to drain before a restart. systemd socket activation could hold connections through a restart, but it would also start a stopped agent on the next connection.

## Not in v0.1

- **Public repositories.** GitHub's own guidance is that self-hosted runners should almost never serve them.
- **One label over several boxes.** Boxes can share a Worker by splitting classes or repositories. How GitHub spreads jobs across two scale sets with one label, and whether it assigns a job to a scale set whose box is down, is untested.
- **A published image.** The box builds its image from GitHub's official runner image, so the first release depends on no image of ours.
- **A heartbeat, a status page and fallback to GitHub's runners.** These are planned next.
