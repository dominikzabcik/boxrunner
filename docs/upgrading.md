# Upgrading

boxrunner has two parts to upgrade: the agent on the box and the Worker in your Cloudflare account. Only the latest release gets security fixes, so keep both on it.

You do not need to watch the releases page. The agent looks for boxrunner's latest release once a day, through the release page's redirect, which needs no GitHub API quota. When a newer release is out, `sudo boxrunner status` and the [dashboard](dashboard.md) say so for the agent and for the Worker, and `sudo boxrunner doctor` prints a warning. The [changelog](../CHANGELOG.md) says which part each change is in.

The Worker refuses an agent of a major version it does not serve, so within one major version either part can go first.

## The agent

```sh
sudo boxrunner upgrade           # the latest release
sudo boxrunner upgrade 0.2.2     # one release
```

`upgrade` downloads the installer of that release from the release's assets on GitHub. With [gh](https://cli.github.com) installed and signed in, it checks the installer's build attestation first. Then it runs the installer, as the install command does:

1. The installer checks the agent's binary against the checksums written into it and, with gh signed in, the binary's build attestation. If either does not match, nothing is installed.
2. It installs the binary and runs `boxrunner install`, which keeps the box's configuration and token, makes any change the new release needs, and restarts the service on the new binary.

Running jobs keep running through the restart. `upgrade` refuses an older release than the one installed unless you pass `--downgrade`.

An agent from before `upgrade` existed upgrades the same way with the install command, without `--worker` and `--token`:

```sh
curl -fsSL https://boxrunner.zabcik.me/install.sh | sudo sh
```

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
