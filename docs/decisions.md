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

## An egress allowlist per class

Jobs could reach anything on the internet except the host, private ranges and the metadata service, and nothing recorded where they went ([#53](https://github.com/dominikzabcik/boxrunner/issues/53)). A class that runs a coding agent needs the opposite. Its network is closed, and opens only to the destinations its owner names, such as GitHub, an LLM provider's API, package registries and a few services on private addresses. A prompt in an issue or a pull request can make such an agent run any command a job can, so the policy has to hold against code in the job that is root in its container, uses addresses by number, other DNS servers, DNS over HTTPS, raw sockets and forged source addresses.

Probes ran on October 16, 2026, in an Ubuntu 24.04 arm64 VM with Docker 29.9.1, nftables 1.0.9 and Sysbox 0.7.1, and the end-to-end workflow runs the same checks on GitHub's Ubuntu 22.04, 24.04 and 24.04 arm64 images.

**How a class's traffic is told apart.** Two ways were weighed:

- **One bridge, a set keyed by the container's address and the destination (the first idea).** Only the container that looked a name up could use its addresses. But a job under Sysbox is root with every capability in its network namespace, and `ip addr add` and `ip link set eth0 address` both worked (*measured*), so a job can take another container's source address. That defeats a policy keyed by address in two ways. A job of an egress class that sends from the address of a container of an open class is judged as the open one. And a forged source can match a connection that another bridge's container has open, which conntrack accepts. A container's address is also known to the agent only after `docker start`, so its first packets would be unjudged, and an address that a finished container left behind would hand its open addresses to the next container.
- **One Docker network per egress class (chosen).** The bridge a packet arrives on is the one thing a job cannot forge. The rules say `iifname "<bridge>" jump <class's chain>`, the chain first drops packets whose source is outside the bridge's subnet, and the open addresses belong to the class, not to a container. All jobs of a class have the same list, so a set per class loses nothing the per-container key gave. The price is a network and a /16 or /24 of Docker's address pools for each egress class, and the registry mirror, the Actions cache and the DNS forwarder, which sit on the host, each need a rule for each bridge.

**How the destinations are enforced.** Four designs:

| | Wildcards and CDN churn | Bypasses | Cost |
|---|---|---|---|
| **A DNS forwarder opens addresses in an nftables set (chosen)** | A name's addresses are opened as the job resolves it, so whatever the CDN answers is what is open. A wildcard is matched against the name the job asks for. | A hard-coded address, a different DNS server, DNS over HTTPS, IPv6 and raw sockets all meet a drop, because only addresses the forwarder opened are open. | A forwarder in the agent, about 3 ms of `nft` per new address (*measured*: 200 runs of `nft add element` took 0.68 s). Needs `nft`. |
| **B The agent resolves the list into per-class sets every few minutes** | A wildcard cannot be listed. A CDN answers each resolver with another set of addresses, so a job is refused addresses the agent never saw. | Same as A for what it does allow. | Simple, and unusable for the hosts GitHub lists. |
| **C A CONNECT proxy or transparent SNI proxy** | Judges the name of each connection, so CDN churn does not matter. | Covers TLS on port 443 and tools that honor `HTTPS_PROXY`. ECH hides the name. Other ports (ssh, a database) still need address rules. | Every byte passes the agent, and a proxy is far more code than A. |
| **D Addresses and ranges only** | Cannot name `github.com`. | The same as A for what it does allow. | The simplest, and not enough on its own: it is the static part of A. |

A forwarder works because Docker's embedded resolver forwards what it does not know from the container's own network namespace. With `--dns <gateway>`, a query arrived at the gateway's port 53 from the container's address (*measured*). So the forwarder knows which class asked from the bridge it listens on, with no table of containers to keep in step. The forwarder refuses a name the policy does not allow without asking upstream, so a refused name goes nowhere. An allowed name does reach the name servers of its domain, so a wildcard over a domain someone else runs carries what a job puts in the name to them. For an allowed name it reads the answer, takes the addresses on the chain of aliases from the question, and opens each one for the answer's TTL (at least five minutes, at most an hour) before the answer goes back, so a job never holds an address it cannot use. It lowers the TTLs in the answer to the time the addresses have left, at most an hour, so a job that caches an answer as long as its TTL says does not outlive the address. That time is shorter than the answer's own TTL when an address was open already: an element with more than half its time left is not set again, to spare an `nft` call per lookup. It sends the box's resolver a query with an ID of its own and accepts only an answer that repeats the question, so a job that knows the ID it chose cannot forge the answer that opens an address. It answers AAAA with an empty answer, because a job's network has no IPv6.

**Why nftables, and why next to iptables.** The existing rules are iptables chains for the job bridge, and the first idea was an ipset. An nftables set has a timeout for each element in the kernel, a rule set replaces in one transaction that keeps the sets, and the package `nftables` is in the archives of Ubuntu 22.04 and 24.04 and Debian 12, and `install` adds it. The table is separate from Docker's and the existing chains, and an `accept` in one table does not override a `DROP` in another: a connection to an address that an iptables rule dropped still timed out after an nft rule accepted it first (*measured*). So the egress bridges must not pass through the iptables private-range drops at all, and the existing jump, which names `boxrunner0`, already keeps them out. The table judges what leaves an egress bridge, and what the host receives from it. The host's own iptables rules stay for two reasons. Hosts with ufw drop what a rule does not accept, and only an iptables accept goes before ufw's rules, as the registry mirror's and the cache's already do. And they are the backstop below.

**Failure modes.** The agent serves an egress class only while its network, the table, the host's rules and the forwarder are all in place; otherwise it starts no runner of the class, stops its listeners and says why in `status`, the `boxrunner_egress_ready` metric and `doctor`. A start that finds the class unserved is refused, never sent to the job network. No job of an egress class runs without the agent, since only the agent puts the rules back. A class without `egress` is untouched.

| What happens | What the class does |
|---|---|
| The agent stops or restarts | It stops the class's jobs, with 30 seconds each to cancel their job while the forwarder still answers. The table stays, with no DNS, so nothing new opens. The first design kept them running, and a flush while the agent was down left them open. |
| The agent crashes or is killed | Its jobs keep running with the rules but no DNS until the agent starts again, which removes them before it builds the rules. |
| Someone deletes the table | The host drops what leaves an egress bridge without the mark the table sets on what it lets through (`BOXRUNNER-EGRESS-FWD`), so the class stays closed. Within ten seconds the agent puts the table back and removes the class's jobs. |
| Someone flushes the whole ruleset (`nft flush ruleset`) | On a host with iptables-nft the flush also takes the backstop, Docker's rules and the job network's rules. Until the agent notices, within ten seconds, nothing judges what the class's jobs send: a probe reached the host on the bridge's gateway, Docker's default bridge's gateway and a private container on it (*measured*). The agent then puts the table, the backstop and the job network's rules back and removes every job of an egress class, since a connection opened meanwhile would survive in conntrack. Docker's rules come back when Docker restarts. |
| Someone deletes only the iptables rules | The table judges every packet and its drops are final, so nothing changes for the jobs. The agent puts the rules back within a minute. |
| The class's network is removed (`docker network prune`) | The class is not served until the agent makes the network again on the same subnet and bridge, within ten seconds, and the forwarder listens there again. |
| The forwarder stops | The class is not served until it listens again; the agent tries again after one second, doubling up to a minute. |
| The host reboots | No job survives a reboot, and the agent builds the table before it serves the class. |
| A class loses its `egress` key, or its setup fails | A job the agent could not remove keeps a closed network until it finishes. |
| `nft` or the table fails to build | The class is not served. A transaction that fails replaces the table in one step, so no packet meets it empty. |

**What stays open, and is documented.** An allowlist limits where a job connects, not what a job with credentials sends to a service it may reach. The `github` preset follows GitHub's list for self-hosted runners, and three of its hosts take data from anyone: `github.com` accepts a push to any repository the job can write to, `github-cloud.s3.amazonaws.com` resolves to Amazon S3's shared us-east-1 front ends, where a presigned upload to any bucket works, and names under `*.actions.githubusercontent.com` can resolve to shared Microsoft edge addresses. The rules judge addresses, not names inside TLS, so domain fronting through a shared address works, and so does a tunnel over an allowed host. A wildcard over a domain others can register under is a way out, and so are the names a job looks up under any wildcard. The registry mirror serves all of Docker Hub to a class with `docker = true`, and the image names a job asks for reach Docker Hub. With `cache = true` a job saves cache entries that other classes' jobs of the same repository restore, and the cache passes other calls on to GitHub's results service. The first design also showed a limit: jobs of a class share a bridge, so their separation from each other is the job network's.

**Logging.** The table logs what it drops to an NFLOG group at most 20 times a second per class, and counts every drop. When reading the group fails, the agent opens it again. The agent reads the group over the netfilter netlink socket in a small package of its own. The other ways were a library, and `/dev/kmsg`, which the kernel fills only from the initial network namespace and which also lands in the system log. It names the job from the address on the class's network, and the name an address was opened for, and logs the first of each distinct refusal in a minute.

**Review.** Two reviews that tried to break the design found five problems, and the code fixes each. A job could forge the answer from the box's resolver, because the forwarder used the job's own query ID and accepted any answer with that ID. A forged source on one bridge could match another bridge's connection, which the subnet check closes. A deleted table left the bridges open, which the backstop closes. An `add element` on an element that exists may not renew its time: the nftables wiki says the time cannot be changed ([Element timeouts](https://wiki.nftables.org/wiki-nftables/index.php/Element_timeouts)), while Linux 7.2 renewed it in a probe (*measured*), so a renewal removes the address and adds it again. And a private address could come in through an alias to a host nobody named.

A third review, of the code, found more, and the code fixes each. A start that found the class's network missing put the job on the open job network. `nft flush ruleset` took the backstop with the table, and a job's connections opened in that time outlived the rules; jobs also outlived a stopped agent, with nobody to put the rules back. A second lookup of an address that was open already got the upstream's TTL, longer than the address had left. The class stayed "ready" with its network removed, its forwarder stopped or its rules not restored. A first start listed the box's networks before it made the class's, so the class's own subnet was missing from the deny list until a restart.

## Organization scale sets, fenced by a selected runner group

A repository scale set needs the App's repository permission Administration (read and write), which can also change the repository's settings and delete it. A company that runs boxrunner for an organization would rather give the App no power over its repositories. An organization scale set uses the same protocol with the organization's registration token ([actions/scaleset](https://github.com/actions/scaleset/blob/e6daac702355cdb5b880b4fbdcf6d85dcd9e48e5/client.go#L1020-L1033)), and the App needs only the organization permission Self-hosted runners ([GitHub's docs](https://docs.github.com/en/actions/tutorials/use-actions-runner-controller/authenticate-to-the-api)).

It moves the fence. A repository scale set takes jobs only of its repository, which the Worker checks is private before every runner. An organization scale set takes jobs of every repository its runner group admits, and a runner it mints is bound to no repository, so the Worker cannot check the job's repository before the runner exists. The ways to keep public and unwanted repositories off the box:

- **Repository scale sets in every repository (the default, kept).** Each repository is fenced on its own, at the cost of Administration on each.
- **Any runner group, with the agent as the fence.** Accept the default group whatever it admits and let the agent acquire only the served repositories' jobs. It works on every plan, but a job from any repository the group admits, a new one included, would still be routed to the scale set and wait there, and the only check against a public repository would be one GitHub setting the Worker never looked at.
- **A group limited to selected repositories, refused otherwise (chosen).** The Worker serves a group only while its repository access is "Selected repositories" and it does not allow public repositories, reads it again before every session and runner, and deletes the organization's scale sets when it starts allowing public repositories, as it does for a repository that turns public. The agent still acquires only jobs of repositories the Worker and the class serve, so `REPOS` and a class's `repos` keep working, and a repository the group admits by mistake is never run. The cost is the group: GitHub lets only Team and Enterprise organizations create one, and whether a Free organization can limit its default group is not known.
- **The Worker keeps the group's repositories in step with `REPOS`.** Self-hosted runners (write) lets the App set a group's repositories. It would save a step, but it would make the Worker change an organization's settings, and a mistake in `REPOS` would open the box to a repository. Left out.

**The App key covers more, not less.** Self-hosted runners (write) is an organization permission: it ignores which repositories the installation selects. With it, the key can register a just-in-time runner with any labels in any group, the Default group included, and set any group to admit all repositories or public ones ([GitHub's list of what it allows](https://docs.github.com/en/rest/authentication/permissions-required-for-github-apps#organization-permissions-for-self-hosted-runners)). The runner group fences only what the Worker does. So a leaked key can take jobs, secrets and `GITHUB_TOKEN`s across the whole organization. Repository Administration lets a leaked key do the same and also change or delete repositories, but only in the repositories the App is installed on. Organization mode trades that power over repositories for a reach over every repository. The Worker is the only holder of the key in both modes, so the tradeoff is about what a leak of the Worker's secrets costs, and SECURITY.md says so.

**The group the Worker checks is the group the scale sets are in.** The Worker finds the group by name, then reads it by id, and a group can be renamed in between, another group can take the name, or `RUNNER_GROUP` can change. So it keeps the group id with each scale set, checks the scale set's group at GitHub before every session and runner, creates scale sets only when the Actions service's id for the name is the id it checked, and deletes a scale set in any other group, or every scale set of a group that is renamed or deleted (GitHub moves a deleted group's runners to the Default group).

**The Worker checks `REPOS` before each runner too.** The agent decides which offered jobs to acquire, from a list up to a minute old. A runner is bound to no job, so the Worker cannot check the job a runner will take. Instead the agent names the repositories of every job the scale set holds, and the Worker refuses the runner while one is not served. One such job then stops the class's runners until someone cancels it: refusing is the safe side, and it can only happen in the minute after a repository leaves `REPOS`. The agent trusts its own list in between, so the check is against a stale list, not a dishonest agent; the box token already trusts the agent with its own state.

**Refused jobs are read back.** GitHub offers a job in one message. The current actions/scaleset listener never reads the queue's waiting jobs, but actions-runner-controller's first listener read `acquirablejobs` when a session opened ([autoScalerMessageListener.go @ 87938ee](https://github.com/actions/actions-runner-controller/blob/87938ee5bf647e71200bf2fbd5392d5fe8d59f97/cmd/githubrunnerscalesetlistener/autoScalerMessageListener.go#L96-L115)). A job the agent turned down while its list was out of date would otherwise wait at GitHub until it timed out, so the agent asks the Worker for the waiting jobs when a session opens and whenever its list changes, and acquires those it now serves.

A Worker serves either repositories or one organization, not both, so one set of permissions and one notion of "served" holds at a time. The fork pull request settings cannot be read without Administration, on the organization or on each repository, so the warnings about them are off for an organization, and `doctor` says it cannot check them.

None of this has run against a real organization: the tests use a fake built from actions/scaleset and GitHub's REST documentation. [Config](config.md#not-checked-against-github-yet) lists the probe that would check it, and the Free plan question decides whether it helps a small organization at all.

## An organization can own a repository-mode App

A private App can be installed only on the account that owns it, and setup used to create the App under whoever ran it, so per-repository scale sets could not serve an organization's repositories. Organization scale sets could, but their permission covers every repository of the organization. Repository Administration on the repositories the App is installed on reaches less, so the `APP_OWNER` variable makes setup post the manifest to the organization's settings and keeps the repository permissions.

It is a variable, not a field on the setup form, so the setup page, the check against `ORG` and the sign-in page read one value, and the person who deploys the Worker decides who owns its key, as with `ORG`. An organization cannot sign in, and the Worker's only sign-in rule is "the App's owner or `ADMINS`", so setup refuses to create an App an organization owns while `ADMINS` is empty: the alternative was a Worker whose dashboard nobody can open. A public App, which any account can install, was ruled out: the Worker serves every private repository the App is installed on unless `REPOS` narrows it, so anyone who installed it would get the box.

## One class on several boxes

GitHub lets one session at a time read a scale set's queue, so two boxes that served one repository and class did not share its jobs: the one that opened the session ran them all. A runner is not tied to the session, though. Its one-job config comes from the scale set, and actions-runner-controller runs its listener and its runners as separate pods, often on separate nodes. So one box holds the session and acquires the jobs, and any box with the box token can mint a runner of the same scale set that takes one.

The Worker splits the jobs, because it already holds every box's latest heartbeat: the jobs the session holder reports, and each box's runners, classes and room. It splits them on every heartbeat, from those heartbeats alone, in a fixed order (rank, then name), so two boxes that ask a second apart get the same split and never both start a runner for one job. A runner that starts anyway takes no job and leaves after its idle timeout, as before. The Durable Object answers one heartbeat at a time, so no split interleaves with another.

Two alternatives were set aside. The Worker could hold the session itself, but a Durable Object would then long-poll GitHub's queue all day, and every box would depend on it for every job, including a lone box. A workflow could pick a box's own class in `runs-on`, but then each workflow carries the routing, and a box that goes down after the pick strands the job.

## The agent upgrades itself, late and carefully

Only the latest release gets security fixes, and a box nobody logs in to would stay on the release it was installed from. The agent therefore installs a newer release of its own major version, unless the config sets `auto_upgrade = false` ([upgrading](upgrading.md#automatic-upgrades)). It is on by default, because a default that needs a decision leaves most boxes behind.

- **Six hours after it first sees a release,** not at once. The wait leaves time to pull or fix a broken release before most boxes take it. It counts from the agent's first sighting, kept in a file, so a restart does not reset it and a new release restarts the wait.
- **Never a new major version.** A major version can change what a box needs, and the Worker refuses an agent of a major version it does not serve. A person decides when to cross it.
- **Not while a job of an egress class runs.** A restart stops those jobs, because nobody else puts their rules back. Other jobs survive the restart, so they do not hold an upgrade back; a box that must never restart under a running job turns the setting off, or drains first.
- **In a transient systemd unit,** `boxrunner-upgrade`, started with `systemd-run`. The installer restarts `boxrunner.service`, and systemd stops everything in that service's cgroup, an installer included. The unit is the same `boxrunner upgrade` a person runs, so there is one way to install a release, with its checks.
- **With a backoff, persisted.** After each attempt the agent waits an hour, then twice as long each time up to a day, whether or not the attempt looked successful: the agent only learns that an upgrade worked when it restarts as the new version.

## Not in v0.1

- **Public repositories.** GitHub's own guidance is that self-hosted runners should almost never serve them.
- **One label over several boxes.** Boxes can share a Worker by splitting classes or repositories. How GitHub spreads jobs across two scale sets with one label, and whether it assigns a job to a scale set whose box is down, is untested.
- **A published image.** The box builds its image from GitHub's official runner image, so the first release depends on no image of ours.
- **A heartbeat, a status page and fallback to GitHub's runners.** These are planned next.
