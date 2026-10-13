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

Keeping one idle runner per repository and class does not fit a small box. Ten repositories and two classes would mean twenty idle runners, while the main class may allow one runner at a time. Runners start when GitHub assigns a job. One cold start took 8 seconds from queue to start on a laptop with the image already pulled (probe). Warm runners for chosen repositories may come later. A runner that has not taken a job within three minutes is removed; one minted and never started can be removed (probe).

## Idle-priority class

On a 4-core ARM box, four timed threads took 1,319 ms alone, 1,345 ms beside four busy threads in a top-level SCHED_IDLE slice, and 6,536 ms beside four busy threads at normal priority. A first attempt named the slice `runners-idle.slice`. systemd read the dash as nesting and put it inside `runners.slice`, where it was idle only against its siblings and still took half the CPU. The slice is therefore named `boxrunner_idle.slice`, and `doctor` checks `cpu.idle` on it.

## Setup in the browser, behind a secret

The private key goes from GitHub straight to the Worker through the App manifest flow, so nobody copies it. A freshly deployed Worker is reachable by anyone, so its setup page requires a secret set at deploy time. It signs the manifest's `state` and ties it to a cookie, and it works once. Both design reviews found the unguarded version claimable by whoever came first.

## Not in v0.1

- **Docker inside jobs.** It needs Sysbox, which must first be tested with the idle slice and CPU limits.
- **Public repositories.** GitHub's own guidance is that self-hosted runners should almost never serve them.
- **Several boxes per Worker.** How GitHub spreads jobs across two scale sets with one label is untested.
- **A published image.** The box builds its image from GitHub's official runner image, so the first release depends on no image of ours.
- **A heartbeat, a status page and fallback to GitHub's runners.** These are planned next.
