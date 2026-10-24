# Changelog

## v0.9.4

**Features**
- A menu bar app for macOS shows the fleet: each box, its runners and limits, the waiting and the latest jobs. It pairs with the Worker the way a device pairs with a TV: it shows a code, you approve it on the dashboard, and it keeps a token of its own in the keychain. Its switch takes your Mac's box out of the fleet and back ([app](app/macos/README.md)). The Worker gains the pairing and `GET /v1/fleet`, `POST /v1/fleet/limit` for paired devices, and a **Paired devices** section with **Unpair** ([api](docs/api.md#paired-devices)).

## v0.9.3

**Features**
- A class can be limited on one box from the dashboard: its runners there are capped below the class's `max`, and 0 stops the box taking its jobs while the rest of the fleet goes on. The box hears its limits in the answer to its heartbeat, removes waiting runners over the limit and lets running jobs finish; the Worker gives the jobs the box no longer takes to the other boxes. `boxrunner status` shows the limit ([dashboard](docs/dashboard.md#limiting-a-class-on-one-box)). Upgrade the Worker and the boxes.

## v0.9.2

**Features**
- Each finished job records what its container used: CPU time across all cores and its peak memory, the job's own Docker included. The agent reads them from the container's cgroup while it runs, since the cgroup goes with the container. `boxrunner status` and the dashboard show them, and the dashboard names the box that ran each job, so the boxes of a fleet can be told apart. Peak memory needs Linux 5.19.

## v0.9.1

The tag v0.9.0 points at a commit before the version bump, so it has no release; v0.9.1 is the first release with these changes.

**Features**
- Several boxes can serve one class. One box holds the scale set's session and acquires the jobs; the Worker splits them between the boxes by `rank` and free room, and each box mints runners for its share, which it hears in the answer to its heartbeat. While a box shares a class it reports every 10 seconds, and the box holding the session reports as soon as its queue changes. `status` and `doctor` show the share instead of failing. Upgrade the Worker and every box ([config](docs/config.md#one-class-on-several-boxes)).

**Security**
- Built with Go 1.26.10, which fixes GO-2026-6809 in `crypto/x509`: the status server's TLS path reached the vulnerable certificate verification.

**Fixes**
- A box with Docker classes no longer runs out of disk with no job running. Sysbox keeps each job's own `/var/lib/docker` in `/var/lib/sysbox/docker/<container id>` and deletes it with the container, but it missed 13 of 198 containers in two days on one box, about 2.4 GB each, until free space fell below the disk floor and the box started no runner. The agent now deletes what Sysbox kept for any container Docker no longer has. Upgrade the agent; the first sweep frees what earlier jobs left.

## v0.8.4

**Fixes**
- Classes with `cache = true` pass hyphenated action inputs again. The node wrapper ran under `/bin/sh`, which is dash on Ubuntu and drops environment variables whose names are not shell names, so an action lost every input such as `INPUT_ANALYSIS-KINDS`: CodeQL's init failed with "Input required and not supplied: analysis-kinds" in every cache class since v0.6.0. Upgrade the agent; the job image is rebuilt with the fix (#111).

## v0.8.3

**Fixes**
- The `github` egress preset lets jobs download npm packages from GitHub Packages: `npm.pkg.github.com` redirects each download to `pkg-npm.githubusercontent.com`, which the preset did not list, so installing a private scoped package in an egress class failed (#109).

## v0.8.2

**Fixes**
- The job image fetches Ubuntu's packages over HTTPS. A box whose network allows only port 443 out could not build it, because the runner image lists Ubuntu's archive over plain HTTP; jobs' own `apt-get` now needs no port 80 either (#106).

## v0.8.1

**Setup**
- An organization can own the App while the box serves repositories one by one: set the Worker's `APP_OWNER` to the organization's login, and setup creates the App under it with repository Administration on the repositories you install it on. Before, setup always created the App under the person who clicked, and a private App owned by a person cannot be installed on an organization's repositories (#104, docs/config.md).
- When an organization owns the App, nobody signs in as its owner, so setup needs `ADMINS` and only those logins open the dashboard. This holds for `ORG` mode too; Workers already set up are unaffected.

## v0.8.0

**Organization scale sets (not yet checked against a real organization)**
- Opt-in: with the Worker variables `ORG` and `RUNNER_GROUP`, the Worker makes one scale set per class for the whole organization, inside a runner group, and the App needs the organization permission Self-hosted runners instead of repository Administration. Repository scale sets stay the default and are unchanged (#102, docs/config.md).
- The runner group is the fence: the Worker serves it only while it admits selected private repositories and no public ones, re-reads it at GitHub before every session and runner, checks each scale set's own group, and deletes scale sets in any other group. A mint names its jobs' repositories and the Worker refuses one it does not serve; the agent re-reads waiting jobs when its list changes.
- That permission reaches the whole organization: a leaked App key can register runners in any group. SECURITY.md and docs/decisions.md weigh it against repository Administration. Issue #28 stays open until the probe in docs/config.md passes on a real organization.

**Egress**
- Docker networks made after the agent started are denied to egress classes within ten seconds, without a restart (#95).
- After `nft flush ruleset`, the agent puts back the job network's rules too, on every box, egress classes or not (#96).

**Seeing what happens**
- `status`, the dashboard, `doctor` and the metrics show the job image or class image the agent is building, instead of `image none` (#99).

**Fixes**
- The status socket answers only after the agent published its first state, so an early `status` no longer shows an empty box.

## v0.7.0

**Egress allowlist**
- Opt-in per class: `egress = ["github", "api.example.com:443", "*.mirror.example", "10.20.0.0/16:5432"]` closes the class's network to everything but the list. The class runs on its own Docker network; a DNS forwarder in the agent answers only listed names and opens their addresses in an nftables set for as long as the answer lives, and an nftables table judges what leaves the bridge. Addresses by number, other resolvers, IPv6, UDP, the host, the metadata service and private networks the list does not name are refused. Classes without `egress` are unchanged (#53, docs/config.md#egress-allowlist).
- Refused connections and lookups are logged, rate-limited, and counted in `status` and the metrics; `doctor` proves the policy from a container on each egress class's network.
- Jobs of an egress class never run without their rules: when the rules vanish (`nft flush ruleset`, a deleted table) the agent puts them back and removes the jobs that ran in the gap, and it stops those jobs when it stops and removes survivors after a crash. Restarting or upgrading the agent therefore cancels running egress jobs; drain first.
- An allowlist limits where a job connects, not what it sends to an allowed service with the credentials it holds. docs/config.md says what it does not stop. Two independent security reviews found and verified fixes for a fail-open start path, the rules flush and TTLs outliving their addresses before release; two smaller gaps are tracked in #95 and #96.

**Docs**
- docs/coding-agents.md: running an AI coding agent on an egress class, with an example workflow and what to keep it from (fallback to GitHub's runners, production credentials, untrusted triggers).

## v0.6.0

**Local Actions cache**
- Opt-in: a top-level `cache = "20GB"` and `cache = true` on a class keep `actions/cache` and every `setup-*` cache on the box. The agent serves the cache on the job bridge, checks each job's runtime token against GitHub's published keys and keeps each repository's entries apart, with the branch rules GitHub uses. Upload and download links carry 256 random bits. Everything else on the results service, artifacts included, passes through to GitHub, and a job falls back to GitHub's cache when the box's does not answer within 2 seconds (#26, docs/config.md).
- The cache has a size cap with least-recently-used eviction, a per-repository fair share, caps on blocks, open uploads and connections per job, and empties itself below the disk floor. Two independent security reviews found and verified fixes for disk filling, cross-repository eviction and connection exhaustion before release; three smaller fairness edges are tracked in #91.

**Site**
- boxrunner.zabcik.me has a new design: warm paper, a serif display face and the box drawn as an engraving, with the docs in the same look.

**Project**
- CI checks the site on every pull request, and Dependabot's majors come one per pull request.

## v0.5.0

**Security**
- The job image is built only from an actions/runner image whose digest CI verified. A daily workflow checks each runner release's build attestation (signed by actions/runner's release workflow on the matching `releases/m<minor>` branch) and publishes `runner-digests.json`, and the agent builds `FROM …@sha256:<digest>` only while the tag still points to that digest. A release not yet in the list falls back to the newest one that is. The digest shows in `status`, `doctor` and the dashboard (#50). Verifying on each box instead would have grown the agent from 8 to 20 MB and 72 modules (#71).
- `doctor` and the dashboard warn when a served private repository runs workflows from fork pull requests without approval, or gives them write tokens or secrets (#51).

**Seeing what happens**
- Optional Prometheus metrics: `metrics = "127.0.0.1:9464"` serves runners, assigned and completed jobs, job duration and queue time, disk and versions. ARC's names where they mean the same; jobs cannot reach the port (#54).
- The alert webhook also posts when a repository's jobs wait with no runner starting, naming the likely cause (a paused class, a draining or silent box, a class held after failing runners, a class at its max), and the dashboard lists those waits (#47).
- With the App's `workflow_job` event turned on, the dashboard lists jobs nobody here runs: a `runs-on` that is not a class, a repository the Worker does not serve, a paused class (#48, docs/unserved-jobs.md).

**Fixes**
- The agent waits for its status socket to close before it exits, so a stopped agent leaves no socket behind (#69).

**Project**
- A logo: a box holding one main job and the idle jobs in the space it leaves, drawn with every icon by docs/art/build.py.
- Dependabot's grouped updates take minor and patch versions only; each major comes as its own pull request.

## v0.4.0

**Fixes**
- The idle class is idle on Ubuntu 22.04 and other systems with systemd older than 252. `CPUWeight=idle` only sets `cpu.idle` from systemd 252, so on systemd 249 idle jobs ran at normal priority and `doctor` failed its idle slice check. The agent now sets `cpu.idle` itself and the slice keeps the CPU controller on. The idle class needs Linux 5.15 or newer; `doctor` names an older kernel (#73).
- Reloading the page that shows a new box token no longer replaces the token again; the form redirects to a page that shows it once (#57).
- `uninstall` removes the agent's status socket, which the agent could leave behind when it stopped.

**Running unattended**
- `boxrunner drain [--wait]` closes the box's sessions and starts no runners while running jobs finish; `boxrunner resume` undoes it, and a drain survives a reboot. docs/upgrading.md#rebooting-the-box has the steps. Docker's live-restore stays off, because Sysbox containers lose DNS after a Docker restart with it (#49).
- An optional `ALERT_WEBHOOK` posts once when every box has been silent for `FALLBACK_MINUTES` or `ALERT_MINUTES` (10 by default), and once when a box reports again. One message works for ntfy, Discord and Slack (docs/alerts.md, part of #47).

**Project**
- A workflow installs, checks, reinstalls and uninstalls boxrunner on GitHub's Ubuntu 22.04, 24.04 and 24.04 arm64 VMs, with runc, Sysbox and Docker in jobs, on every change to the installer and nightly (#52).

## v0.3.0

**Security**
- Two build-time dependencies with advisories are pinned to their fixed releases: sharp 0.35.5 and katex 0.18.2.
- Every class runs under Sysbox when the box has it, so a job's root is an unprivileged user on the host instead of root (#45). `isolation = "runc"` keeps a class on Docker's default runtime, and `install --sysbox` installs Sysbox without turning on Docker in jobs. `doctor` shows the uid a job's root maps to. **Behavior change:** on a box that already has Sysbox from `install --docker`, classes without `docker = true` move to Sysbox when the agent restarts.
- `boxrunner shell` opens a failed job's copy under the runtime the job ran under (#60).
- Releases carry the build attestation as `boxrunner.sigstore.json`, and Dependabot keeps Go modules, Bun packages and actions current.

**Upgrading**
- `boxrunner upgrade [VERSION]` downloads a release's installer, verifies it, runs it and restarts the agent; it refuses a downgrade unless asked. Re-running the installer by hand now also restarts the agent when its binary changed, which it did not before.
- The agent checks for a newer release once a day, and `status`, `doctor` and the dashboard say when the agent or the Worker is behind. The Worker reports its version. docs/upgrading.md covers the agent, a Deploy-button Worker and a Wrangler one.

**Running unattended**
- Opt-in fallback to GitHub's runners while the box is silent: the Worker sets the repository variable `BOXRUNNER_DOWN`, which a workflow reads in `runs-on`, and deletes it once the box has reported for two minutes (#25). It needs the App's Variables permission; docs/fallback.md.
- A class whose runners exit before taking a job is held with exponential backoff (1 minute up to 30) instead of starting runners in a loop; `status` and `doctor` say so. Optional `max_duration` stops a job container that runs too long.
- Superseded job, class and runner base images are removed after a rebuild and when the agent starts; `status` shows the disk boxrunner's images use.

**Several boxes**
- When two boxes serve the same repository and class, the second one says which box holds the session instead of pointing to a dead box's jobs, and `doctor` fails until each box has its own classes or repositories (#27, docs/config.md#several-boxes).

## v0.2.2

**Security**
- Built with Go 1.26.9. v0.2.0 and v0.2.1 were built with Go 1.26.8, whose standard library has known vulnerabilities in `net/http`, `crypto/tls`, `mime/multipart` and `html/template` that the agent's code reaches (govulncheck). `go.mod` now names the toolchain, and CI and the release job run govulncheck, so a release with a known vulnerability in its binaries is not published.

**Dashboard**
- Pause and resume a class, and replace the box token, from the dashboard (#17). A paused class starts no new runners on any box; running jobs finish.

## v0.2.1

**Fixes**
- The Worker deletes the scale sets of a repository it stops serving (public, archived or left out of `REPOS`) the next time it lists repositories, so they leave the repository's runner settings even when no agent asks for that repository again. Where the App was removed it forgets them (#31).
- `boxrunner status` points to recovering a dead box's jobs only on a box that has never run a job, or after a crash left a session open, no longer after every restart of an idle box.

**Releases**
- The Deploy to Cloudflare button deploys the Worker from the `stable` branch, which each release moves forward, so a new Worker matches the agent the installer downloads (#21).

## v0.2.0

The first public release.

**Runs jobs**
- Ephemeral GitHub Actions runners on one Linux box (amd64 or arm64): one fresh container per job, removed when the job ends.
- Runner scale sets: the agent long-polls GitHub's queue, so the box needs no open port. Jobs start 6 to 12 seconds after they are queued, or 2 seconds for a repository with a warm runner (`warm`).
- Priority classes: an `idle` class runs only on cores a `normal` class leaves idle (SCHED_IDLE). A normal job slowed by 0 to 3% beside busy idle jobs, and by 390% beside busy normal ones.
- Docker inside jobs with Sysbox (`docker = true`, `install --docker`): `services:`, `container:` jobs, Docker actions, `docker build` and `docker compose`, with a pull-through mirror for Docker Hub on the box.
- `boxrunner shell` opens a shell in a running job, or in a failed job a class keeps (`keep_failed`).
- Custom images per class from a Dockerfile, rebuilt when the job image or the Dockerfile changes.

**Keeps the key off the box**
- A Cloudflare Worker holds the GitHub App key. The box holds a token that can ask for runners and nothing else; session tokens read only their own queue.
- Setup in the browser through the App manifest flow, behind a setup secret; nobody copies a key. Deploy to Cloudflare in one click.
- The Worker refuses public repositories, narrows every installation token to one repository and the permission the call needs, and serves only the classes and repositories it lists.
- A dashboard on the Worker with GitHub sign-in for the App's owner: boxes, runners, recent jobs, minutes not billed by GitHub.

**Hardened and unattended**
- Jobs run on their own bridge; the host, private ranges, tailnets and the cloud metadata service do not answer them. `boxrunner doctor` proves it from inside a job.
- CPU, memory, process and shared-memory limits per class; no runner starts below a disk floor, and the job that wrote the most is stopped below half of it.
- Restarts and crashes converge: sessions close on stop, adopted containers keep running, a crashed session is waited out.
- The job image tracks the runner's latest release daily, without API quota.

**Installs in one line**
- `curl -fsSL https://boxrunner.zabcik.me/install.sh | sudo sh -s -- --worker … --token …`, verified by checksums written into the installer and the build attestation.
- `install` converges; running it again changes nothing. `uninstall` leaves no unit, slice, rule or file behind.
