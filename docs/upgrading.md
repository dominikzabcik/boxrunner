# Upgrading

boxrunner has two parts to upgrade: the agent on the box and the Worker in your Cloudflare account. Only the latest release gets security fixes, so keep both on it.

You do not need to watch the releases page. The agent looks for boxrunner's latest release every hour, through the release page's redirect, which needs no GitHub API quota. When a newer release is out, `sudo boxrunner status` and the [dashboard](dashboard.md) say so for the agent and for the Worker, and `sudo boxrunner doctor` prints a warning. The [changelog](../CHANGELOG.md) says which part each change is in.

The Worker refuses an agent of a major version it does not serve, so within one major version either part can go first.

## The agent

The agent upgrades itself, as [below](#automatic-upgrades). To upgrade it by hand, for example to take a release before the wait is over or a new major version:

```sh
sudo boxrunner upgrade           # the latest release
sudo boxrunner upgrade 0.2.2     # one release
```

`upgrade` downloads the installer of that release from the release's assets on GitHub. With [gh](https://cli.github.com) installed and signed in, it checks the installer's build attestation first. Then it runs the installer, as the install command does:

1. The installer checks the agent's binary against the checksums written into it and, with gh signed in, the binary's build attestation. If either does not match, nothing is installed.
2. It installs the binary and runs `boxrunner install`, which keeps the box's configuration and token, makes any change the new release needs, and restarts the service on the new binary.

Running jobs keep running through the restart, except those of classes with an [egress allowlist](config.md#when-the-agent-stops-and-when-the-rules-go), which it stops; drain first to let them finish. `upgrade` refuses an older release than the one installed unless you pass `--downgrade`.

An agent from before `upgrade` existed upgrades the same way with the install command, without `--worker` and `--token`:

```sh
curl -fsSL https://boxrunner.zabcik.me/install.sh | sudo sh
```

### Automatic upgrades

Unless the box turns it off, the agent installs a newer release of its own major version without anyone running `sudo boxrunner upgrade`. It starts the upgrade when all of this holds:

- **The release has been the latest for six hours** since this agent first saw it. The agent keeps that time in `/var/lib/boxrunner/release-seen.json`, so a restart does not start the wait over. The wait leaves time to pull or fix a broken release before most boxes take it.
- **The release has the same major version as the agent.** A newer major version, an older release and a development build are never installed this way. `status` says when a new major version is out.
- **The box does not drain.**
- **No job of a class with an [egress allowlist](config.md#when-the-agent-stops-and-when-the-rules-go) is running,** because the restart stops it. Jobs of other classes keep running through the restart, as in a manual upgrade. A step that talks to the [Actions cache](config.md#actions-cache) at that moment can fail, so drain first when that matters to you.

The agent weighs this every five minutes. It runs `boxrunner upgrade` for the release in a transient systemd unit of its own, `boxrunner-upgrade`, because the installer restarts `boxrunner.service` and systemd would stop an installer that ran inside it. The upgrade is the one described above, with the same checks. Its output goes to the journal:

```sh
journalctl -u boxrunner-upgrade
```

When starting the upgrade fails or the upgrade does not finish, the agent logs it and tries again after an hour, then after two, four, eight and sixteen hours, and after that once a day. It never retries sooner.

`sudo boxrunner status` shows whether auto-upgrade is on and, while a newer release waits, why and until when. `sudo boxrunner doctor` shows the same setting and fails when auto-upgrade is on but `systemd-run` does not run. To turn it off, set `auto_upgrade = false` in [the config](config.md#top-level) and restart the agent; the agent then only logs and shows that a newer release is out.

## Rebooting the box

A reboot stops every job container, and so does a restart of Docker, which an upgrade of Docker's packages does on its own. Drain the box first, so that no job is running when it happens:

```sh
sudo boxrunner drain --wait     # close the sessions and wait for running jobs
sudo apt-get upgrade            # the updates that need the reboot
sudo reboot
sudo boxrunner resume           # once the box is back
```

`drain` closes the agent's sessions and removes the runners that have not taken a job, warm ones included, and the agent starts no new runners. Running jobs keep running, and `--wait` returns once every session is closed and no job container runs. Jobs queued in the meantime wait at GitHub and start after `resume`. The box keeps reporting, so [fallback](fallback.md) does not send them to GitHub's runners.

The drain outlasts a restart of the agent and the reboot: the agent keeps it in `/var/lib/boxrunner/draining` and opens no sessions until `sudo boxrunner resume`. While it lasts, `sudo boxrunner status` says the box is draining and `sudo boxrunner doctor` fails on it, so a forgotten drain shows.

Docker's `live-restore` setting would keep containers running through a restart of Docker, but `install` leaves it off: under Sysbox, a job that survives such a restart loses DNS ([decisions](decisions.md#dockers-live-restore-stays-off)). `doctor` shows whether it is on.

## The Worker

`sudo boxrunner doctor` names the Worker's release. A Worker that names none was deployed before Workers reported their release, so it needs an upgrade.

Redeploying keeps the Worker's state: the App and its key, the box token, the paused classes and the dashboard's history live in its Durable Object, and secrets such as `SETUP_SECRET` stay. A deploy does replace variables set in the Cloudflare dashboard with the ones in `wrangler.jsonc`, so keep your values of `CLASSES`, `REPOS` and `ADMINS` in that file.

### Deployed with the Deploy button

The button copied the Worker from the `stable` branch into a new repository in your GitHub account, and Workers Builds deploys that copy when you push to it. The copy does not receive boxrunner's changes. To upgrade, copy the latest release's Worker over it and push:

```sh
git clone https://github.com/<you>/<your copy> && cd <your copy>
curl -fsSL https://github.com/dominikzabcik/boxrunner/archive/refs/heads/stable.tar.gz \
  | tar -xz --strip-components=2 boxrunner-stable/worker
git diff wrangler.jsonc   # put your own CLASSES, REPOS and ADMINS back
git add -A && git commit -m "Upgrade the boxrunner Worker" && git push
```

Each release moves `stable` to itself, so this is always the latest release's Worker. The deployment shows under your Worker's **Deployments** in the Cloudflare dashboard.

### Deployed with Wrangler from a clone

```sh
cd boxrunner
git fetch origin
git checkout stable
git pull --ff-only
cd worker && bun install
bunx wrangler deploy
```

If you changed `wrangler.jsonc`, commit that change on your own branch and merge `stable` into it instead, so the checkout keeps your values.
