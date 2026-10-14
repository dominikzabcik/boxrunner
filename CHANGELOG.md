# Changelog

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
