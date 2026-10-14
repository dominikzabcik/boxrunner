# Unserved jobs

A job whose `runs-on` has a typo, a job in a repository the Worker does not serve and a job stranded by a [replaced box](architecture.md#replacing-a-box) all wait in GitHub's queue, and no box here was assigned them. The agent reads only its own scale sets' queues, so no heartbeat carries them: the dashboard's "Jobs waiting for a runner" and the [alert for waiting jobs](alerts.md#when-jobs-wait) cover only jobs GitHub assigned to a box's scale set that got no runner.

With unserved jobs on, GitHub tells the Worker about every job in the repositories the App is installed on, and the [dashboard](dashboard.md) lists the queued ones no box here will run, with the reason and a link to the job. It is off until you turn it on, because the App then needs one more permission.

## What the dashboard lists

| Why | When the job is listed |
|---|---|
| runs-on is not one of this Worker's classes | At once, for a job in a served repository whose `runs-on` is not exactly one class (`boxruner`, `[boxrunner, linux]`, `gpu`) |
| The repository is public, which the Worker never serves | At once, for a job on one of the Worker's classes |
| The repository is archived | The same |
| The repository is not in REPOS | The same, while `REPOS` is set and leaves the repository out |
| Its class is paused | After 10 minutes, for a job on a class paused on the dashboard |
| No box took it within 10 minutes | After 10 minutes, for any other job on a served class and repository: the box is down, every runner of the class is busy or the job is stranded |

A job leaves the list when a runner takes it, when it finishes or is canceled and when it waits for an environment's approval. GitHub cancels a job that waited 24 hours, and the dashboard stops listing it then too.

Jobs for GitHub's runners are never listed: any job with a label that starts with `ubuntu-`, `windows-` or `macos-`. Neither is a job on a label that no class has in a repository the Worker does not serve, since nothing suggests it was meant for this box. A job whose `runs-on` names another self-hosted runner of yours, in a served repository, is listed until that runner takes it, usually within seconds.

The list holds the jobs the Worker heard of after you turned this on. Jobs queued before that do not appear.

## Turn it on

### For a new App

On the setup page, tick **Report jobs that wait for a runner nobody here runs** before you create the App. Its manifest then asks for the **Actions** permission (read) and the **Workflow job** event, and points the App's webhook at `<worker>/webhook`, with the secret GitHub makes for it. Nothing else to do.

### For an existing App

1. Open the App's settings on GitHub: `https://github.com/settings/apps/<app slug>`, or under the organization's **Settings**, **Developer settings**, **GitHub Apps** when an organization owns it.
2. Under **Webhook**, tick **Active** and set **Webhook URL** to `<worker>/webhook`, for example `https://boxrunner.<your-subdomain>.workers.dev/webhook`. Leave **Secret** as it is: setup kept the secret GitHub made with the App. Save the changes.
3. Under **Permissions & events**, set **Repository permissions**, **Actions** to **Read-only**, and tick **Workflow job** under **Subscribe to events**. Save the changes.
4. Accept the new permission on each installation. GitHub asks the account the App is installed on, by email and at `https://github.com/settings/installations` (an organization's under its **Settings**, **GitHub Apps**), under **Review request**. Until then, GitHub sends no job events for that installation.

GitHub sends a `ping` as soon as the webhook is active. The App's **Advanced** tab lists recent deliveries with the Worker's answer:

- **204.** It works. The dashboard says "None." with the time of the last delivery, until a job waits.
- **401 unauthorized.** The secrets differ, for example because the App's webhook secret was changed. Set a new one under **Secret**, then give the Worker the same value with `bunx wrangler secret put WEBHOOK_SECRET`.
- **503 not_configured.** The Worker has no secret at all. Do the same.

`WEBHOOK_SECRET`, when set, takes the place of the secret setup kept.

## What the Worker does with a delivery

`POST /webhook` takes GitHub's deliveries ([api.md](api.md#post-webhook)). The Worker checks the body's HMAC in `X-Hub-Signature-256` against the webhook secret, in constant time, before it reads anything from the body, and refuses a body over 256 KB. It never logs a body.

- **`workflow_job`.** `queued` keeps the job and `in_progress` marks it as taken. `completed` and `waiting` drop it. GitHub does not promise to deliver events in order, so a `queued` that arrives after `in_progress` does not list the job again. For each job the Worker keeps its id, repository, whether the repository is private or archived, labels, name, workflow name, link and when GitHub queued it. It keeps the newest 200 jobs that are queued or running, and only those it could ever list.
- **`ping` and every other event.** Answered 204 and dropped. GitHub sends an App its installation events whatever it subscribes to, and an error would mark each one failed in the App's delivery log.

The Worker decides why a job waits when the dashboard asks, from `CLASSES`, `REPOS` and the paused classes at that moment, so fixing `CLASSES` takes a job off the list at once.

A `completed` delivery larger than 256 KB, for a job with very many steps, is refused. The job left the list when its runner took it, so only its row stays, until 200 newer jobs replace it.

## What the permission allows

The **Actions** permission (read) lets the App read the workflow runs, jobs, logs, artifacts and caches of the repositories it is installed on. The Worker uses none of that: it only receives the `workflow_job` event, which needs the permission. Every installation token it makes is still narrowed to one repository and the one permission a call needs (`metadata`, `administration` or `actions_variables`), so the box never gets a token that can read Actions data. Deliveries come for every repository the App is installed on, served or not, and the Worker keeps only what this page lists.

## What it costs

Each job brings up to three deliveries, so three requests to the Worker, each writing a few rows in the Durable Object. A thousand jobs a day are 3,000 of the free plan's 100,000 requests. A dashboard view reads at most the 200 kept jobs.

## Turn it off

Untick **Active** under **Webhook** in the App's settings, and remove the **Workflow job** event and the **Actions** permission if nothing else needs them. The dashboard keeps showing when the last delivery arrived, so you can tell the deliveries stopped.
