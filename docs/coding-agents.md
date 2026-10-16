# Running coding agents on boxrunner

This guide sets up a class for an AI coding agent, such as [claude-code-action](https://github.com/anthropics/claude-code-action), that works on issues, pull request reviews and failed checks on your own box, with its network closed to everything but a list.

It is for you if a workflow in your repository starts an agent that reads what people write, changes the code, runs the tests and pushes a branch. Such a job runs code that text in an issue can steer, so it gets a class of its own: one job at a time, under Sysbox, with an [egress allowlist](config.md#egress-allowlist). The example uses `claude-code-action`; other agents need the same class and a different step.

## Read this first

### Never let an agent job fall back to GitHub's runners

[Fallback](fallback.md) moves jobs to GitHub's runners while the box is down. When no heartbeat arrived for `FALLBACK_MINUTES`, the Worker sets the Actions variable `BOXRUNNER_DOWN` in every repository it serves, and a workflow whose `runs-on` reads it, as in `${{ vars.BOXRUNNER_DOWN && 'ubuntu-latest' || 'boxrunner' }}`, sends its new jobs to `ubuntu-latest`. GitHub's runners have no allowlist: there the agent's job reaches the whole internet, with the same API key and App token.

So an agent workflow names its class and nothing else: `runs-on: agent`. When you add fallback to your other workflows, leave this one out. While the box is down its jobs wait for it, and GitHub cancels a job that waited 24 hours. Two more things keep the job on the box:

- **A label no other runner has.** GitHub's runners never carry `agent`. If you run other self-hosted runners, give the class a name none of them uses.
- **A first step that fails off the box.** The workflow below starts with a step that exits when `runner.environment` is not `self-hosted`. It runs before any step that uses a secret, so if someone adds the fallback expression later, the job fails instead of running the agent.

### An allowlist limits where the job connects, not what it sends

The list decides which hosts the job can reach. It does not decide what the job sends to them. The job holds an API key and an App token, and the `github` preset lets it reach GitHub. With those, it can push your code to a branch, paste it into a comment or send it to the model API, and the list allows each of these. [What it does not stop](config.md#what-it-does-not-stop) lists the other paths: addresses that CDNs share, the registry mirror, the storage behind Git LFS.

Assume the agent can read every secret its job uses. Any step can become root in its container, the runner holds the job's secrets in memory, and the action hands the API key and the App token to Claude Code's process. Keep what the job holds small: an App installed only on the repositories the agent works on, with the permissions below, and an API key used for nothing else, with a spending limit.

### Never give the agent production credentials

No deploy keys, no production database URL, no cloud role that reaches production, and no `id-token: write` that a cloud account trusts for production. Whatever the job can read, a prompt can make it send to an allowed host. The example gives the agent a Postgres seeded from test data and a staging API with credentials for staging only.

Protect the default branch with a ruleset the agent's App cannot bypass, so the agent pushes to branches and a person merges. Do not give the App the Workflows permission: without it GitHub refuses a push that changes `.github/workflows`, so the agent cannot write a workflow that runs with your repository's secrets. Your CI still runs the agent's code on its branches, so keep secrets out of the CI jobs that run there.

### Untrusted triggers

The agent follows instructions in whatever it reads: issue bodies, comments, reviews, commit messages and files. Treat a run as code written by whoever wrote that text. In a private repository, anyone with read access can open an issue or comment on one.

- **Never use `pull_request_target` for an agent.** It runs the base branch's workflow, with its secrets, for a pull request from a fork, whose author then writes what the agent reads. The Worker refuses public repositories, but a private repository can have forks inside its organization. `sudo boxrunner doctor` warns when a repository the box serves runs workflows from fork pull requests without approval, with write tokens or with secrets.
- **Skip forks.** The workflow below runs on a review or a failed check only when the pull request's branch is in the repository itself.
- **Gate every run behind a label a person sets.** In a private repository, adding a label takes triage access. The agent's App can add labels too, since Issues write allows it, so a prompt could make the agent label another issue. The workflow therefore checks that the last time the `agent` label was added, a user added it, not a bot, and that the label is still there when the job starts. Removing the label stops a run that has not started yet.
- **The thread can change after the label goes on.** The agent reads the issue and its comments when it runs, so an edit or a comment made after the label reaches it.

`claude-code-action` also checks that the account that triggered it has write access, and it refuses runs that bots start unless `allowed_bots` names them.

## The class

Add the class to `/etc/boxrunner/config.toml`:

```toml
[[class]]
name = "agent"
cpus = 4
memory = "8GiB"
shm = "2GiB"
max = 1
max_duration = "90m"
repos = ["octo/site"]
isolation = "sysbox"
docker = true
dockerfile = "/etc/boxrunner/images/agent/Dockerfile"
egress = [
  "github",                   # the runner, checkout and the GitHub API
  "api.anthropic.com:443",    # the model API
  "registry.npmjs.org:443",   # packages for bun and pnpm
  "10.20.30.0/24:8443",       # the staging API, on a private network
]
```

What each choice does:

- **`isolation = "sysbox"`.** A job's root is an unprivileged user on the box ([isolation](config.md#isolation)). `docker = true` implies it; written out, it stays if `docker` goes.
- **`max = 1`.** One agent job at a time. GitHub holds the class's other jobs in its queue until the runner is free, so two agents never push to the same branch at once. Jobs of one class share a bridge, and with one job there is nobody to share it with. The workflow's `concurrency` group adds the rest: of several runs for the same issue or branch, only the newest waits.
- **`max_duration = "90m"`.** Stops a job that outlives the workflow's `timeout-minutes`, such as a runner that hangs after its job.
- **`repos`.** Only the repositories the agent works on.
- **`shm = "2GiB"`.** Room in `/dev/shm` for Chromium in end-to-end tests.
- **`docker = true`.** Gives the job its own Docker for the Postgres service container. It has a cost: the registry mirror serves the job all of Docker Hub whatever the list says, and the image names a job asks for reach Docker Hub. If your tests can use a Postgres installed in the class image instead, leave `docker` off, and the job cannot reach the mirror.
- **`egress`.** The `github` preset, the model API, one package registry and the staging API by address range and port. Each entry is one more place the job can send data, so add a host only when a refusal shows the job needs it ([checking the setup](#checking-the-setup)).

### The Actions cache stays off

With `cache = true`, the class's jobs save entries to the box's cache that the jobs of the same repository in other classes restore, so an agent could leave an entry that your main CI then restores and runs. The box's cache also passes every call that is not a cache call on to GitHub's results service, whether the list names it or not ([what it does not stop](config.md#what-it-does-not-stop)). So the class leaves `cache` off, and the workflow saves no cache entries: no `actions/cache` steps, and no `cache:` input on setup actions. The tools a cache would restore are in the class image instead.

### The class image

Save this as `/etc/boxrunner/images/agent/Dockerfile`:

```dockerfile
ARG BASE
FROM ${BASE}
# Chromium and the libraries it needs, where the runner user finds them.
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright
RUN npx -y playwright@1.64.1 install --with-deps chromium
USER runner
RUN npm install --global bun@1.4.3 pnpm@12.11.2 @anthropic-ai/claude-code@2.1.301
# The entrypoint starts the job's Docker as root.
USER root
```

The agent builds the image on the box's own network, outside the list, so jobs need none of these downloads. Use the Playwright version in your lockfile, since each release expects its own Chromium build. Claude Code is the version that the action's release installs (2.1.301 for v1.0.251, in its `src/entrypoints/run.ts`), and the workflow points the action at it with `path_to_claude_code_executable`, so the job needs neither `claude.ai` nor `downloads.claude.ai`. Change both together when you update the action. The action still downloads its own Bun from GitHub's releases, which the `github` preset allows, and installs its packages from `registry.npmjs.org`.

### Turn it on

1. Add `agent` to the Worker's `CLASSES` variable, as in `"boxrunner,boxrunner-light,agent"`, and deploy the Worker.
2. Run `sudo boxrunner install`. It installs Sysbox, the registry mirror and `nftables` for the new class.
3. Run `sudo systemctl restart boxrunner`. A restart stops the running jobs of egress classes, so drain the box first when one runs ([upgrading](upgrading.md#rebooting-the-box)).

## The workflow

The agent gets its own GitHub App, not boxrunner's, whose key stays in the Worker. Create it with these repository permissions: Contents, Issues and Pull requests read and write, and Actions read, to read the logs of failed checks. Install it only on the repositories the agent works on. In each, store the App's client ID as the variable `AGENT_APP_CLIENT_ID`, its private key as the secret `AGENT_APP_PRIVATE_KEY` and the API key as the secret `ANTHROPIC_API_KEY`, and create the label `agent`. On a plan with environments for private repositories, keep the two secrets in an environment named `agent` and add `environment: agent` to the job, so no other workflow can read them.

Save this as `.github/workflows/agent.yml`. It assumes your CI workflow is named `CI`, your tests run with `bun run test` and `test/fixtures/seed.sql` fills the test database, and the App's bot is `octo-agent[bot]`:

```yaml
name: Agent

on:
  issues:
    types: [labeled]
  pull_request_review:
    types: [submitted]
  workflow_run:
    workflows: [CI]
    types: [completed]

permissions: {}

concurrency:
  group: agent-${{ github.event.issue.number || github.event.pull_request.number || github.event.workflow_run.head_branch }}
  cancel-in-progress: false

jobs:
  agent:
    if: >-
      (github.event_name == 'issues' && github.event.label.name == 'agent') ||
      (github.event_name == 'pull_request_review' &&
        github.event.pull_request.head.repo.full_name == github.repository &&
        contains(github.event.pull_request.labels.*.name, 'agent')) ||
      (github.event_name == 'workflow_run' &&
        github.event.workflow_run.conclusion == 'failure' &&
        github.event.workflow_run.head_repository.full_name == github.repository &&
        startsWith(github.event.workflow_run.head_branch, 'claude/'))
    runs-on: agent
    timeout-minutes: 60
    permissions:
      contents: read
      issues: read
      pull-requests: read
    services:
      postgres:
        image: postgres:17-alpine
        env:
          POSTGRES_DB: app
          POSTGRES_PASSWORD: test
        ports:
          - 5432:5432
        options: >-
          --health-cmd "pg_isready -U postgres"
          --health-interval 2s
          --health-timeout 5s
          --health-retries 15
    env:
      DATABASE_URL: postgres://postgres:test@localhost:5432/app
      STAGING_API_URL: https://10.20.30.40:8443
      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1"
    steps:
      - name: Refuse to run anywhere but the box
        if: runner.environment != 'self-hosted'
        run: |
          echo "This job must run on the agent class, behind its egress list." >&2
          exit 1

      - name: Check that a person added the agent label
        env:
          GH_TOKEN: ${{ github.token }}
          NUMBER: ${{ github.event.issue.number || github.event.pull_request.number }}
          BRANCH: ${{ github.event.workflow_run.head_branch }}
        run: |
          if [ -z "$NUMBER" ]; then
            NUMBER=$(gh pr list -R "$GITHUB_REPOSITORY" --head "$BRANCH" --json number --jq '.[0].number // ""')
          fi
          if [ -z "$NUMBER" ]; then
            echo "No open pull request comes from $BRANCH." >&2
            exit 1
          fi
          if ! gh api "repos/$GITHUB_REPOSITORY/issues/$NUMBER" --jq '.labels[].name' | grep -qx agent; then
            echo "#$NUMBER does not have the agent label." >&2
            exit 1
          fi
          by=$(gh api --paginate "repos/$GITHUB_REPOSITORY/issues/$NUMBER/events" \
            --jq '.[] | select(.event == "labeled" and .label.name == "agent") | .actor.type' | tail -n 1)
          if [ "$by" != User ]; then
            echo "The agent label on #$NUMBER was added by a $by, not a person." >&2
            exit 1
          fi

      - uses: actions/create-github-app-token@64ab729a6294000f5aa4a4bc0b73d1f32bc45510 # v3.3.0
        id: app
        with:
          client-id: ${{ vars.AGENT_APP_CLIENT_ID }}
          private-key: ${{ secrets.AGENT_APP_PRIVATE_KEY }}
          permission-contents: write
          permission-issues: write
          permission-pull-requests: write
          permission-actions: read

      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: ${{ github.event.workflow_run.head_branch }}
          persist-credentials: false

      - name: Seed the test database
        env:
          POSTGRES: ${{ job.services.postgres.id }}
        run: docker exec -i "$POSTGRES" psql -U postgres -d app -v ON_ERROR_STOP=1 < test/fixtures/seed.sql

      - run: bun install --frozen-lockfile

      - uses: anthropics/claude-code-action@374f254f5efaf968abfe15d399fd90972e8138b9 # v1.0.251
        with:
          anthropic_api_key: ${{ secrets.ANTHROPIC_API_KEY }}
          github_token: ${{ steps.app.outputs.token }}
          path_to_claude_code_executable: /home/runner/.npm-global/bin/claude
          label_trigger: agent
          allowed_bots: octo-agent[bot]
          additional_permissions: |
            actions: read
          prompt: ${{ github.event_name == 'workflow_run' && format('CI run {0} failed on this branch. Read the logs of its failed jobs, fix the cause and push the fix to this branch.', github.event.workflow_run.id) || '' }}
          claude_args: |
            --max-turns 40
            --allowedTools "Bash(bun run test:*),Bash(bunx playwright test:*)"
```

How the parts fit:

- **Triggers.** A person adds the `agent` label to an issue. A person reviews a labeled pull request and writes `@claude` in the review. CI fails on a branch the agent pushed, which the action names `claude/…`. The job's `if` drops every other event before a runner starts, so they never take the class's one place. zizmor reports `workflow_run` as a dangerous trigger, because it runs with the repository's secrets after any branch's CI, a fork's included. Here the `if` and the label check limit it to the agent's own branches with a labeled pull request, so keep both.
- **`permissions`.** The workflow's token can only read: the label check uses it. Everything the agent writes goes through the App's token. `create-github-app-token` narrows that token to the four permissions the agent needs and to the current repository, and revokes it when the job ends.
- **The label check.** It finds the issue or pull request, confirms the label is on it and that a user, not a bot, added it last. On a failed check, it looks up the open pull request from the branch.
- **`allowed_bots`.** A failed check on the agent's branch comes from a CI run that the App's push started, and the action refuses runs that bots start unless they are named. Name only the agent's own App.
- **`timeout-minutes` and `--max-turns`.** The first bounds the job's time, the second the number of turns the model takes. `max_duration` on the class is the box's backstop.
- **`--allowedTools`.** Claude Code runs no shell commands by default; this allows the test commands. Running the tests runs the repository's code, so this widens what the agent does, not what it can reach.
- **`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`.** Claude Code skips telemetry and error reports, whose hosts the list refuses, so the refusals that `status` lists are the ones worth reading.
- **Loops.** Each fix the agent pushes runs CI again, and a failure starts the agent again. The turn cap and the timeout bound each round, not the number of rounds. Remove the label to stop.

The action pins (by commit) are the releases current when this page was written. Look up the commit of a newer release with `gh api repos/anthropics/claude-code-action/commits/v1.0.251 --jq .sha`, with the tag you want.

## Checking the setup

On the box, after the restart:

```sh
sudo boxrunner doctor
sudo boxrunner status
```

`doctor` shows the `agent` class's isolation and the host uid that a job's root maps to, and checks its egress rules. From a container on the class's network it proves the policy: an allowed host answers, a name outside the list does not resolve, an address of an allowed name is unreachable by number, and the host, the metadata service and the box's resolver are unreachable. It fails while any of this is not in place, and warns about fork pull request settings in the repositories the box serves.

`status` has a table for the class with the packets dropped and the lookups refused since the agent started, and the latest refusals, each with the job that made it, the address or name, and the name an address was opened for. Run it after a first test: add the label to an issue that asks for a small change and wait for the job. Each refusal is either a host a tool needs, which you add to the list (and restart), or a place the agent tried to reach that it should not, in which case read the job's log and the issue that started it.

With [metrics](config.md#metrics) on, `boxrunner_egress_ready` is 1 while the agent serves the class with its list in force, and `boxrunner_egress_blocked_total` counts the refusals by `kind`, `packet` or `lookup`. Alert on both:

```promql
boxrunner_egress_ready{class="agent"} == 0
increase(boxrunner_egress_blocked_total{class="agent"}[1h]) > 0
```
