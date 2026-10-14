# Fallback to GitHub's runners

While the box is down, its jobs wait in the queue until it returns. With fallback on, the Worker sends new jobs to GitHub's own runners instead, and back to the box once it reports again. Fallback is off until you turn it on.

## How it works

The agent sends the Worker a heartbeat every 30 seconds. When no heartbeat arrived for `FALLBACK_MINUTES`, the Worker sets the Actions variable `BOXRUNNER_DOWN` in every repository it serves. Its value is the time the Worker switched, in UTC. A workflow reads the variable in `runs-on`:

```yaml
jobs:
  test:
    runs-on: ${{ vars.BOXRUNNER_DOWN && 'ubuntu-latest' || 'boxrunner' }}
    steps:
      - uses: actions/checkout@v5
      - run: make test
  lint:
    runs-on: ${{ vars.BOXRUNNER_DOWN && 'ubuntu-latest' || 'boxrunner-light' }}
    steps:
      - uses: actions/checkout@v5
      - run: make lint
```

When the box reports again, the Worker waits until it has reported for two minutes with no three heartbeats in a row missing, then deletes the variable. A box that keeps restarting and dying therefore does not take jobs back between its crashes.

The Worker changes the variable only when it switches. Jobs whose `runs-on` does not read the variable keep waiting for the box.

## Turn it on

1. **Give the App the Variables permission.** Open the App's settings on GitHub (`https://github.com/settings/apps/<app slug>`), choose **Permissions & events**, set **Repository permissions**, **Variables** to **Read and write** and save. GitHub then asks the account the App is installed on to approve the change, by email and on the installation's page (**Settings**, **Applications**, **Installed GitHub Apps**, **Configure**). Approve it there. New Apps from `/setup` do not get this permission, because only fallback needs it.
2. **Set `FALLBACK_MINUTES`** on the Worker: a whole number of minutes from 2 to 1440. Set it in `wrangler.jsonc` and run `bunx wrangler deploy`, or in the Cloudflare dashboard under the Worker's **Settings**, **Variables and Secrets**. Choose a time longer than your agent usually stays stopped, for example during an upgrade, so a planned restart does not switch.
3. **Read the variable in your workflows**, as above.

The dashboard shows whether fallback is on, whether the box is down and why the last change of the variable failed, if it did. To hear when the Worker switches, turn on [alerts](alerts.md) too.

## Jobs that already wait for the box

GitHub picks a job's runner when it queues the job. A job queued before the Worker switched, in the first `FALLBACK_MINUTES` of an outage, stays with the box's label and runs when the box returns ([a job queued while the box is down waits for it](architecture.md#how-a-job-runs)). Only jobs queued after the switch go to GitHub's runners.

To move a waiting job to GitHub's runners, cancel its run and start a new one, for example by pushing again or with `gh workflow run` for a workflow with `workflow_dispatch`. A new run reads the variable when GitHub queues its jobs. Whether `gh run rerun` reads it again has not been tested. If the box is gone for good, see [replacing a box](architecture.md#replacing-a-box).

## Before you turn it on

- **Jobs on GitHub's runners use your plan's minutes.** Once they are used up with a spending limit of $0, those jobs fail to start instead of waiting.
- **The jobs run on a different machine.** `ubuntu-latest` has Docker, but not the tools a class's own Dockerfile adds. Leave fallback out of the `runs-on` of a job that needs them, or install them in a step.
- **A repository the Worker stops serving while the box is down** keeps the variable. Delete it by hand: `gh variable delete BOXRUNNER_DOWN -R octo/site`.

## When the Worker cannot change the variable

Without the Variables permission, GitHub refuses the Worker's token request. The Worker then logs `Could not set BOXRUNNER_DOWN (attempt 1): the App lacks the Variables permission…` (see the Worker's logs in the Cloudflare dashboard), shows the reason on the dashboard and tries again after 1, 2, 4, 8 minutes and so on, up to once an hour. Each attempt asks for one token per served repository, so a missing permission costs a few failed requests an hour and nothing else. After you approve the permission, the next attempt sets the variable. Any other error from GitHub is retried the same way and shown on the dashboard.

Two parts of this are not yet checked against GitHub. The Worker asks for the permission by the key `actions_variables`, taken from public code that uses it: GitHub's documentation names the permission "Variables" but gives no key. And it expects GitHub to refuse a token for a permission the installation lacks with 422, which GitHub documents only as "Validation failed". If GitHub answers otherwise, the Worker retries as above and the dashboard shows GitHub's error instead of the missing permission.

## Turn it off

Empty `FALLBACK_MINUTES`. An unreadable value, such as `5m`, also turns fallback off, and the dashboard says why. If the variable is set, the Worker deletes it at the next heartbeat, or within an hour while the box stays down.

## By hand

Fallback needs nothing from the Worker if you flip the variable yourself:

```sh
gh variable set BOXRUNNER_DOWN --body manual -R octo/site
gh variable delete BOXRUNNER_DOWN -R octo/site
```

## What it costs

- **Cloudflare.** The Worker wakes itself with a Durable Object alarm, never a cron trigger, so fallback takes none of the five cron triggers the free plan allows per account. While the box reports, the alarm fires once every `FALLBACK_MINUTES`: 288 times a day at 5 minutes, each one request and, because setting an alarm is billed as a row written, one row. While the box is down it fires once an hour. With fallback and [alerts](alerts.md) off it never fires. Alerts use the same alarm, so turning both on costs no more.
- **GitHub.** Each switch costs, per served repository, one installation token and one or two variable calls, plus the repository list, which the Worker reads at most once a minute. An installation may make 5,000 requests an hour.
