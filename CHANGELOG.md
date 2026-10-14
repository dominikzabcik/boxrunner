# Changelog

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
