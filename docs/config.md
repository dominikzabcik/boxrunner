# Configuration

The agent reads `/etc/boxrunner/config.toml` when it starts. After a change, run `sudo systemctl restart boxrunner`. A running job keeps running through a restart, except a job of a class with an [egress allowlist](#when-the-agent-stops-and-when-the-rules-go), which the restart stops.

`boxrunner install` writes the file once, sized for the box. It never overwrites the file afterwards. On a later run it only fills in `worker` and `box` when you pass them.

```toml
worker = "https://boxrunner.example.workers.dev"
box = "box-1"
disk_floor = "10GB"

[[class]]
name = "boxrunner"
cpus = 4
memory = "6GiB"
pids = 4096
shm = "1GiB"
priority = "normal"
max = 1

[[class]]
name = "boxrunner-light"
cpus = 4
memory = "3GiB"
priority = "idle"
max = 2
```

## Top level

| Key | Default | Meaning |
|---|---|---|
| `worker` | required | Your Worker's URL. Must be HTTPS. |
| `box` | the hostname's first label | Lower-case letters, digits and dashes. It names the box's runners and sessions at GitHub. |
| `disk_floor` | `10GB` | No runner starts while Docker's data directory, or Sysbox's `/var/lib/sysbox` when it sits on another filesystem, has less free space than this. Below half of it, the agent stops the job container that wrote the most, counting the images and containers of a job's own Docker. |
| `metrics` | none | An IP address and port, such as `127.0.0.1:9464`, where the agent serves [Prometheus metrics](#metrics). |
| `metrics_public` | `false` | Allows `metrics` to be an address other machines can reach. |
| `cache` | none | The most the box's [Actions cache](#actions-cache) keeps, such as `20GB`. At least 1 GiB. Set it together with `cache = true` in a class. |

The box token lives in `/etc/boxrunner/token`, readable by root only.

## Classes

Each `[[class]]` is one kind of runner. A workflow picks it by name, as in `runs-on: boxrunner-light`. Every class must also be in the Worker's `CLASSES` variable, which decides the labels.

| Key | Default | Meaning |
|---|---|---|
| `name` | required | The label in `runs-on`. Box and class names together stay under 56 characters. |
| `cpus` | required | Cores a job of this class may use, for example `4` or `1.5`. |
| `memory` | required | Memory limit, swap included: `6GiB`, `2560MiB`, `3GB`. At least 256 MiB. |
| `pids` | `4096` | Most processes and threads a job may run at once. |
| `shm` | `1GiB` | Size of `/dev/shm`. Browsers crash with Docker's default of 64 MB. |
| `priority` | `normal` | `idle` runs the class's jobs only on cores nothing else wants (SCHED_IDLE), so they never slow a `normal` job. |
| `max` | required | Runners of this class at once on this box. |
| `image` | `boxrunner/runner:local` | Any image whose entrypoint runs the Actions runner. |
| `dockerfile` | none | Builds the class's own image; see below. Set either this or `image`. |
| `keep_failed` | none | Keeps a failed job's container this long, like `15m`, so `sudo boxrunner shell` can open a shell in a copy of it with the workspace as the job left it. At most `24h`. Kept containers do not count toward `max`. |
| `max_duration` | none | Stops a job that runs longer than this, like `6h`, counted from when the runner took it. The runner gets SIGTERM, cancels the job and reports it, and its container is removed. It frees the place of a runner that hangs after its job ends, which `timeout-minutes` cannot do. At least `1m`. |
| `repos` | every repository | Only these repositories, as `["owner/name", …]`. Each class opens one scale set in every repository it serves, so a class meant for one project should name it. For a Worker that serves an [organization](#organizations), the class has one scale set and acquires jobs only of these repositories. |
| `warm` | none | Keeps one idle runner of this class ready for each of these repositories, as `["owner/name", …]`, so their jobs skip the cold start. Each must be one the class serves. A warm runner counts toward `max` and is never removed for being idle; when it takes a job, another starts once no other job is waiting for the place. Name fewer repositories than `max`, unless `repos` lists only warm repositories, so the class's other repositories always have a place. |
| `isolation` | `sysbox` when Docker has Sysbox's runtime, else `runc` | How the class's jobs are kept from the box; see below. |
| `docker` | `false` | Gives each job its own Docker daemon; see below. Needs `isolation = "sysbox"`, which it implies. |
| `cache` | `false` | Keeps the class's [Actions cache](#actions-cache) entries on the box. Needs `cache` at the top. |
| `egress` | none | Closes the class's network to everything its entries do not allow; see [Egress allowlist](#egress-allowlist). Without it the class reaches the internet as before. |

Memory is not prioritised the way CPU is. Give the classes limits that fit side by side: the defaults give the normal class about 75% of the box's memory and each idle runner about 40%.

## Isolation

A job starts as root, and the job image gives `runner` passwordless `sudo`, so any step can become root inside its container. The class's `isolation` decides what that root is on the box:

- `sysbox` runs the job under Sysbox's `sysbox-runc` runtime, where root inside the container is an unprivileged user on the box.
- `runc` runs the job under Docker's default runtime, where root inside the container is root on the box, held back by the container's namespaces and Docker's default capabilities.

A class that does not set `isolation` uses `sysbox` when Docker has the `sysbox-runc` runtime and `runc` otherwise. `sudo boxrunner install --sysbox` installs Sysbox without giving jobs Docker, and `--docker` installs it too. Install then restarts the agent, and from then on such classes start their jobs under Sysbox. A class that sets `isolation = "sysbox"` makes `install` add Sysbox, and the agent does not serve it while Docker has no `sysbox-runc` runtime; it logs why. `isolation = "runc"` keeps a class on Docker's default runtime on a box with Sysbox. `sudo boxrunner doctor` shows each class's isolation and, under Sysbox, the host uid that a job's root maps to.

Sysbox supports Ubuntu 22.04 or newer and Debian 11 or newer on Linux 5.15 or newer, on amd64 and arm64. Docker keeps running while Sysbox installs: install first writes Docker's current bridge address and address pools into `/etc/docker/daemon.json`, so Sysbox's package only makes Docker reload its configuration.

## Several boxes

Several boxes can share one Worker and its box token. Give each box its own `box` name and its own work: different classes, or the same class with `repos` lists that do not overlap. A workflow then picks the box through the class in `runs-on`, and the dashboard shows each box by name. With a Worker that serves an [organization](#organizations), give each box its own classes: a class has one scale set for the whole organization.

```toml
# /etc/boxrunner/config.toml on the first box
box = "box-1"

[[class]]
name = "boxrunner"
cpus = 4
memory = "6GiB"
max = 1
repos = ["octo/site"]
```

```toml
# /etc/boxrunner/config.toml on the second box
box = "box-2"

[[class]]
name = "boxrunner"
cpus = 8
memory = "12GiB"
max = 2
repos = ["octo/docs"]
```

A class only one box serves, such as `boxrunner-large` on a bigger machine, works the same way; add it to the Worker's `CLASSES`.

The Worker makes one scale set per repository and class, and GitHub lets one session at a time read a scale set's queue. Two boxes that serve the same repository and class are not supported, not even with one of them kept as a standby, so `sudo boxrunner doctor` fails on the box that waits. If it happens anyway, the box that opens the session first runs all of the jobs. The other box tries again every five minutes and takes over only after the first one stops, and `sudo boxrunner status` on it names the box that serves the repository and class. Such a takeover is no failover: when a box dies, the jobs it had acquired stay queued, as in [replacing a box](architecture.md#replacing-a-box).

One label spread over several boxes, so that both run jobs from the same `runs-on`, is not supported yet. GitHub can give two scale sets the same label, but which one gets a job, and whether a job waits for a box that is down, is untested.

The boxes share the box token's request budget of about 1,800 requests an hour ([api.md](api.md#conventions)). A box's heartbeat alone takes 120 of them.

A class paused on the [dashboard](dashboard.md#pausing-a-class) is paused on every box. With [fallback](fallback.md) on, the Worker sends jobs to GitHub's runners only when no box has reported for `FALLBACK_MINUTES`, so while one box reports, the jobs of a box that is down wait for it.

## Organizations

By default the Worker makes one scale set per repository and class. That needs the App's repository permission **Administration** (read and write), which also lets the App change a repository's settings and delete it. For an organization, the Worker can instead make one scale set per class for the whole organization. The App then needs the organization permission **Self-hosted runners** (read and write) and **Metadata** (read), and nothing on its repositories.

That permission is not narrower, only different. It covers every repository of the organization, not only those the App is installed on, and the runner group fences what the Worker does with it, not what the key can do. Whoever holds the App key can register a runner with any labels in any group, the **Default** group included, and set any group to admit all repositories or public ones, so a leaked key can take jobs, secrets and `GITHUB_TOKEN`s across the whole organization. With **Administration**, a leaked key can do that, and change or delete repositories, but only in the repositories the App is installed on. See [SECURITY.md](../SECURITY.md) and [the decision](decisions.md#organization-scale-sets-fenced-by-a-selected-runner-group).

Two Worker variables, in `wrangler.jsonc` or the Worker's settings in the Cloudflare dashboard, turn it on:

| Variable | Meaning |
|---|---|
| `ORG` | The organization's login. Empty, the default, serves repositories one by one. |
| `RUNNER_GROUP` | Required with `ORG`. The organization's runner group the scale sets go in, by name. |

With only one of them set, or `ORG` not a login, the Worker answers every agent call with `503 not_configured` and the setup page is locked. The setup page names the variable; the agent's answer only says that `ORG` or `RUNNER_GROUP` is not valid, because the Worker gives it before it checks the box token, and the Worker's log (`wrangler tail`) says why.

### The runner group is the fence

An organization scale set takes the jobs of every repository its runner group admits, whatever repositories the App is installed on. So the runner group, not the App's installation, decides which repositories can reach the box. The Worker serves a group only while:

- its repository access is **Selected repositories**, and
- **Allow public repositories** is off, for the group and, for a group an enterprise shares with the organization, for the enterprise.

It finds the group by name each time it lists the repositories, at most once a minute, and reads it again by its id from GitHub right before a session opens or is refreshed and before a runner is minted. While the group admits every repository, or every private one, the Worker serves none of the organization's repositories and answers `409 group_ineligible` with the reason, which `boxrunner doctor`, `boxrunner status` and the [dashboard](dashboard.md) show too. When the group allows public repositories, is renamed or is deleted (GitHub then moves its runners to the **Default** group), the Worker also deletes its organization scale sets, which disconnects their runners, as it does for a repository that turns public. Once the group is fixed, the agent creates them again within a minute.

The Worker keeps the group each scale set was made in. Before a session or a runner, it also reads the scale set's group from GitHub, and deletes a scale set that is not in the group it just checked, by its own record or by GitHub's: one left in a group that was renamed while another took its name, or in the group `RUNNER_GROUP` named before it changed. Each listing deletes such scale sets too. The agent then makes a new one in the right group.

The served repositories are the group's selected repositories that are private and not archived, narrowed by `REPOS` when it is set. A public repository in the group is not served, and GitHub gives the group no job of a public repository while public repositories are off.

### Setting it up

1. **Make the runner group.** In the organization's settings, under **Actions**, **Runner groups**, create a group, such as `boxrunner`. Set **Repository access** to **Selected repositories**, add the private repositories whose jobs should run on the box and leave **Allow public repositories** off. GitHub lets only Team and Enterprise organizations create groups; see [what is not checked yet](#not-checked-against-github-yet) for the Free plan.
2. **Set the variables** `ORG` and `RUNNER_GROUP` and deploy the Worker.
3. **Run setup.** With `ORG` set, the setup page creates the App under the organization, so you must be one of its owners, with **Self-hosted runners** (read and write) and **Metadata** (read) instead of **Administration**. Install it on the organization. For the runners it does not matter which repositories you pick; [fallback](fallback.md) and [unserved jobs](unserved-jobs.md) still work per repository, so pick the group's repositories for them.
4. **Install the agent** as usual. Workflows use `runs-on: boxrunner` as before.

### On the box

The box's config does not change. The agent opens one session per class for the organization instead of one per repository and class, and acquires an offered job only when the Worker serves its repository and so does the class (`repos`). A job of any other repository stays queued at GitHub: the agent logs it once, `sudo boxrunner status` names the repository and how many of its jobs wait, and the `boxrunner_refused_jobs_total` [metric](#metrics) counts them. Take such a repository out of the group, or let the class serve it.

The agent lists an organization's repositories every minute, not every five minutes, because it alone decides which offered jobs to acquire. GitHub offers a job once, so when a session opens and whenever the list changes, the agent also asks the Worker for the jobs still waiting for the scale set (`GET /v1/jobs`) and acquires those it now serves: a job turned down while a repository was not yet served runs once it is.

`REPOS` is checked again by the Worker before each runner: the agent names the repositories of the jobs the scale set holds, and the Worker refuses the runner while one of them is not a repository it serves, as its listing says, at most a minute old. A runner takes whichever job its scale set holds, so such a job, acquired in the minute before a repository left `REPOS`, stops every new runner of the class until someone cancels it on GitHub. The agent logs that and holds the class's starts, backing off up to 30 minutes, rather than asking every few seconds.

- **`repos`** narrows what a class acquires, as above. It cannot split one class over [several boxes](#several-boxes): a class has one scale set for the whole organization, and one session at a time reads it, so give each box its own classes instead.
- **`warm`** keeps one idle runner for the class's organization scale set when it names any repository of the organization, since that runner takes jobs of every repository.
- **Fork pull requests.** Reading their settings takes the permission **Administration**, on the organization (`GET /orgs/{org}/actions/permissions/fork-pr-workflows-private-repos`) or on each repository, and the App has neither. So the dashboard does not warn about them, and `doctor` says it cannot check them and links the organization's setting, **Fork pull request workflows in private repositories**, under **Settings**, **Actions**, **General**. Check it there and in each served repository.
- **Finished jobs** are recorded under their own repository; runners and assigned jobs under the organization, as `octo (organization)` in `status` and `organization="octo"`, `repository=""` in the metrics, as actions-runner-controller labels an organization scale set.

### Switching a Worker that served repositories

The scale sets the Worker made in each repository keep the class's label, and GitHub may still hand them jobs that no box reads. Take them off GitHub before you switch. With the old App still in place, set `REPOS` to `none/none`, which matches no repository, and redeploy: within five minutes the agent lists the repositories and the Worker deletes every repository scale set it made. Then set `ORG` and `RUNNER_GROUP`, empty `REPOS`, and on the setup page forget the old App and create the new one. An App that the organization already owns can instead keep **Administration** while you add **Self-hosted runners**: the first time the Worker lists the organization, it deletes its repository scale sets itself, and you can then remove **Administration**.

### Not checked against GitHub yet

The organization code is tested against a fake of GitHub's API built from [actions/scaleset](https://github.com/actions/scaleset/blob/e6daac702355cdb5b880b4fbdcf6d85dcd9e48e5/client.go), the client that [actions-runner-controller](https://github.com/actions/actions-runner-controller/blob/5aed393af8a412f6b0fe27507713687a6beeb000/controllers/actions.github.com/autoscalingrunnerset_controller.go#L1022-L1031) uses, and from GitHub's [runner groups API](https://docs.github.com/en/rest/actions/self-hosted-runner-groups). It has not run against a real organization. These are the steps to check it, on a test organization of yours and a throwaway Worker, with one private repository in the group (`octo/site`), one private repository outside it (`octo/docs`) and one in it that `REPOS` leaves out (`octo/api`):

1. **The Free plan.** On a Free organization, open **Settings**, **Actions**, **Runner groups**. Note whether the **Default** group's repository access can be set to **Selected repositories**. If it cannot, the Worker refuses the group and organization mode does not work on Free.
2. **The App.** Deploy the Worker with `ORG`, `RUNNER_GROUP` and `REPOS = "octo/site,octo/docs"`, run setup and install the App on the organization. On the App's page, check that it asks for **Self-hosted runners** (read and write) and **Metadata** (read) and nothing else.
3. **The group.** Install an agent and run `sudo boxrunner doctor`. Its `organization` line should be OK and name 1 repository. A failure that the App lacks the permission means GitHub did not take the manifest key `organization_self_hosted_runners`.
4. **The scale sets.** Under **Runner groups**, the group should list the scale sets `boxrunner` and `boxrunner-light`. The Worker makes them only when the Actions service gives the group the same id as the REST API; if it does not, `doctor` and the agent's log show a `group_ineligible` error that names both ids, and organization mode needs a change to work.
5. **A job.** Push a workflow with `runs-on: boxrunner` to `octo/site`. It should run on the box, and `sudo boxrunner status` should list the finished job under `octo/site`, not `octo`: that checks how a job names its repository.
6. **The fences.** Push the same workflow to `octo/docs`: it should stay queued, and `status` should not mention it, because GitHub keeps it from the group. Push it to `octo/api`: it should stay queued, and `status` should say `octo boxrunner was offered 1 job of octo/api`.
7. **A group that opens up.** Set the group's repository access to **All repositories**. Within a minute `doctor` should fail its `organization` line and new runners should not start. Set it back. Then turn on **Allow public repositories**: the scale sets should leave the group within a minute. Turn it off, and they should come back within a minute or two. Then rename the group and create a new one with the old name: within a minute the scale sets should leave the renamed group and appear in the new one.
8. **Precedence.** Leave a repository scale set named `boxrunner` in `octo/site` from a Worker that served repositories, and push a job there. Note which scale set gets it.
9. **Two classes, one box.** Queue jobs for `boxrunner` and `boxrunner-light` at once and check that both run.
10. **Waiting jobs.** Stop the agent, queue a job in `octo/site`, then start the agent. The job should run within a minute: that checks `acquirablejobs`, which actions-runner-controller's first listener read when a session opened. Then add `octo/api` to `REPOS`: its queued job from step 6 should run within two minutes.

## Docker inside jobs

With `docker = true`, a class's jobs can use `services:`, `container:` jobs, Docker container actions and `docker build`. It needs Sysbox, which `sudo boxrunner install --docker` installs, as does any `install` while a class has `docker = true`. A new config written with `--docker` turns `docker` on for both classes.

Each job runs under Sysbox's `sysbox-runc` runtime, where root inside the container is an unprivileged user on the box, and starts its own Docker daemon before the runner. The job's Docker shares the class's CPU, memory and process limits and its priority, and the firewall applies to the containers it starts. The agent does not serve a class with `docker = true` while Docker has no `sysbox-runc` runtime; it logs why.

Every job starts with an empty Docker, so it pulls each image it uses again. Images from Docker Hub come through a registry mirror on the box: the first job that pulls an image fetches it from Docker Hub, and later jobs get it from the box's cache. The mirror is a `registry` container named `boxrunner-mirror` that listens on port 5000 of the `boxrunner0` bridge's own address, which only job containers can reach, and keeps its cache in the volume `boxrunner-mirror`. It forgets an image a week after fetching it. Images from other registries, such as `ghcr.io`, are pulled directly. The mirror runs while a class has `docker = true`; `install` and the agent start it, and `install` without such a class, or `uninstall`, removes it with its cache.

The job image has Docker Compose, so `docker compose up` works in these jobs.

## Actions cache

With `cache` at the top and `cache = true` in a class, the class's `actions/cache` steps keep their archives on the box instead of uploading them to GitHub. So does every action built on `@actions/cache`, such as `actions/setup-node` with `cache: npm`. A restore reads the box's disk instead of downloading over the internet, and GitHub's 10 GB per repository does not apply. Workflows stay as they are.

```toml
cache = "20GB"

[[class]]
name = "boxrunner"
cpus = 4
memory = "6GiB"
max = 1
cache = true
```

The agent serves GitHub's cache service on port 5001 of the `boxrunner0` bridge's own address, which only job containers can reach, and keeps the archives in `/var/lib/boxrunner/cache`. The runner gives each JavaScript action the address of GitHub's results service, which carries both caches and artifacts. In a class with `cache = true`, the runner's `node` is a wrapper from the job image: when an action starts and the box's cache answers, the wrapper gives the action the cache's address instead. The cache answers the cache's calls itself and passes every other call, such as an artifact upload, on to GitHub unchanged.

Entries follow GitHub's rules ([GitHub's docs](https://docs.github.com/en/actions/reference/workflows-and-actions/dependency-caching#restrictions-for-accessing-a-cache)):

- **Per repository.** A job never sees another repository's entries.
- **Per branch.** A job writes to its own ref, such as `refs/heads/main` or `refs/pull/7/merge`, and restores from its own ref first, then from the base and default branch. A pull request cannot write an entry that `main` would restore.
- **What the token allows.** GitHub writes each job's cache access into the runtime token it gives the job: what `cache-mode` asks for, and read-only access to the default branch's entries for runs from low-trust triggers such as `issue_comment`. The box's cache follows the token.
- **Write once.** An entry with the same key and version stays as the first job saved it.
- **Matching.** The key itself, then the newest entry whose key starts with it, then the newest entry for each restore key in turn.

Entries and the uploads in progress together stay within `cache`, and the cache stops writing before the filesystem that holds `/var/lib/boxrunner/cache` has less than `disk_floor` free. An upload counts as it is written, and joining its blocks counts the block being copied. When a write does not fit, the cache removes the least recently restored entry of the repository that holds the most, uploads and the write included, so a repository's saves remove its own entries before those of a repository that holds less. When the repository that holds the most is the writer's and it has no entries left, the write fails rather than remove more of other repositories' entries. A write that would not fit even with every entry gone, such as one larger than `cache`, fails before it removes any. When a write fails, so does the save. While free space is below `disk_floor`, because jobs took it, the cache removes entries until the space is back, or until none are left, and drops the uploads in progress, so it gives the disk back to jobs.

The cache also holds these limits:

- 10,000 entries per repository; a new one removes the repository's least recently restored.
- 16 uploads in progress per repository and 64 in all. An upload is dropped after ten minutes without a write.
- 4,000 blocks per upload: 125 GiB with `actions/cache`'s 32 MiB blocks.
- 256 connections at once, and 64 from one container: more wait, and a container's 65th is closed. A request's body or a download that stops for 30 seconds is cut off, whether or not the cache reads the body.

Sizes count the bytes written. The filesystem stores each file in whole blocks, 4 KiB on most, so an upload of many tiny blocks takes more disk than it counts for: at most about 1 GiB more in all, with 64 uploads of 4,000 blocks. `disk_floor` is checked against the filesystem's free space, so it holds regardless.

When the cache does not answer within two seconds as an action starts, such as while the agent is stopped, that action talks to GitHub as it would without the cache. A failed cache call is only a warning in `actions/cache`. An artifact upload or download that is in progress when the agent stops can fail its step, since the cache passes those calls on: on stop, the agent keeps the cache serving until its other work is done and gives calls in flight ten seconds. To restart or upgrade the agent with no such risk, drain the box first ([upgrading](upgrading.md#rebooting-the-box)).

What it does not cover:

- **Container jobs and Docker container actions.** In a `container:` job, actions run in the job's own container, which gets no cache address, and Docker container actions get the results service from the runner directly. Both use GitHub's cache.
- **After the runner updates itself.** A job whose runner updated before it started runs the new runner's `node`, without the wrapper, and uses GitHub's cache. The agent rebuilds the job image for each runner release within a day.
- **Other boxes.** Each box keeps its own cache.

`sudo boxrunner doctor` runs the wrapper in a job container and says whether actions there reach the cache. `uninstall` deletes the cache with the rest of `/var/lib/boxrunner`.

## Egress allowlist

A job reaches the whole internet by default. The firewall keeps it off the host, private ranges and the metadata service, but it can still send anything anywhere. A class with `egress` is closed the other way round. Its jobs reach only the destinations its list names. Use it for a class that runs code you do not fully trust, such as a coding agent that reads issues and pull requests, which a prompt in one of them could tell to send your code elsewhere.

```toml
[[class]]
name = "agent"
cpus = 4
memory = "8GiB"
max = 2
repos = ["octo/site"]
docker = true
egress = [
  "github",                     # what a runner and its actions need
  "api.llm.example",            # your LLM provider's API
  "registry.packages.example",  # package registries
  "*.mirror.example",           # any host under a domain
  "10.20.0.0/16:5432",          # a database on a private address, one port
]
```

Each entry is one of these, and each may end in `:port`:

| Entry | Allows |
|---|---|
| `github` | The hosts GitHub lists for self-hosted runners, on port 443. See below. |
| `api.llm.example` | The addresses that name resolves to. |
| `*.mirror.example` | The addresses of any host under the domain, at any depth, but not of `mirror.example` itself. List that too if the class needs it. |
| `203.0.113.7`, `10.20.0.0/16` | That address or range. |
| `:443`, `:5432` | Only that TCP port. Without a port an entry allows 80 and 443. |
| `:*` | Every TCP port. |

How it works, and what follows from it:

- **Names are checked when a job looks them up.** The class's jobs resolve names through a DNS forwarder in the agent. It answers a name an entry allows with the box's own resolver's answer, and refuses every other name without asking anyone. A refused name goes nowhere. An allowed one goes to the box's resolver and on to the name servers of its domain, so a lookup under a wildcard carries whatever the job puts in the name to whoever runs that domain's name servers.
- **An address opens when its name resolves.** The forwarder opens the addresses in an answer, for the answer's TTL but at least five minutes and at most an hour, before the answer goes back to the job. The TTLs in the answer are lowered to the time the addresses have left, so a job that caches the answer does not outlive them. A job that connects to an address it never looked up, such as one it was given as a number, is refused, whether or not the address belongs to an allowed name. Connections already open stay open. A class has at most 8,192 addresses open at once, counting an address once for each port its entries allow; past that, a lookup that would open more fails until some expire.
- **TCP only, and IPv4 only.** A job's network has no IPv6, and an entry cannot allow UDP or ICMP. Other DNS servers, DNS over HTTPS to a resolver that is not on the list, and anything else that is not a TCP connection to an opened address, are dropped.
- **Private addresses need to be named.** A name's private addresses open only for a host written out in full (`db.internal.example`), never for a wildcard, and only when the host itself, not an alias it points to, is on the list. Otherwise list the address or range. Loopback, link-local addresses, multicast and the cloud metadata services (`169.254.0.0/16`, `168.63.129.16`, `100.100.100.200`, `192.0.0.192`) can never be allowed, and a list that names one fails the config check. Neither can Docker's networks on the box, which the agent lists every ten seconds, so a network made after it started is out of reach within ten seconds: a range that covers them, such as `172.16.0.0/12`, is allowed, and those networks stay out of it.
- **The runner must reach GitHub.** The config check fails unless the list allows `github.com`, `api.github.com` and `broker.actions.githubusercontent.com` on port 443. Start the list with `github`.
- **Each class has its own Docker network.** The agent creates `boxrunner-<class>` with its own bridge, labelled `boxrunner.egress`, and takes a subnet from Docker's address pools. The bridge identifies the class. A job can change its own address, and under Sysbox its root can, but it cannot send a packet that arrives on another bridge.
- **On the host a job reaches** only the forwarder on its bridge's gateway, and the registry mirror and Actions cache when the class has `docker = true` or `cache = true`. Everything else the host receives from the bridge is dropped.
- **Docker in jobs obeys the same list.** The job's own Docker uses the forwarder too, and its containers' traffic leaves through the job's address.
- **It needs `nft`.** `sudo boxrunner install` installs the `nftables` package on Debian and Ubuntu. Without it the agent does not serve the class, and `status` and `doctor` say why.

### The `github` preset

`github` stands for the hosts GitHub lists under [Accessible domains by function](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#accessible-domains-by-function) for self-hosted runners, on port 443: `github.com`, `api.github.com`, `*.actions.githubusercontent.com` (the runner's broker and scale set services, the results service, OIDC tokens), `codeload.github.com`, `results-receiver.actions.githubusercontent.com`, `objects.githubusercontent.com`, `objects-origin.githubusercontent.com`, `github-releases.githubusercontent.com`, `github-registry-files.githubusercontent.com`, `release-assets.githubusercontent.com`, `*.pkg.github.com`, `pkg-containers.githubusercontent.com`, `ghcr.io`, `github-cloud.githubusercontent.com` and `github-cloud.s3.amazonaws.com`.

It differs from GitHub's list in one place. GitHub lists `*.blob.core.windows.net` for the results service's blob storage. A job could resolve any Azure storage account under that wildcard, its owner's own among them, and open the addresses of whichever storage cluster holds it. The preset names the twenty storage accounts that GitHub's [meta API](https://docs.github.com/en/rest/meta/meta) lists under `domains.actions`, `productionresultssa0` to `productionresultssa19`, instead. That opens far fewer addresses. Accounts that share those addresses stay reachable, as [what it does not stop](#what-it-does-not-stop) says. If GitHub adds accounts, add them to the list yourself until a release does.

The preset leaves out what GitHub lists only for Dependabot (`dependabot-actions.githubapp.com`) and for runners in an Azure virtual network. Everything else GitHub still lists is in it, since a runner or a common action needs it.

The preset alone does not keep a job from sending data out. `github.com` accepts a push to any repository, someone else's included when a job is handed its token. `github-cloud.s3.amazonaws.com`, for Git LFS, resolves to Amazon S3's shared front ends in us-east-1, which serve any bucket there, so a job can upload to a bucket of its own through a presigned URL. Names under `*.actions.githubusercontent.com` can resolve to shared Microsoft edge addresses that serve other customers too. An allowlist limits where a job can connect, not what a job with credentials sends to a service it may reach. A class that does not use Git LFS or GitHub Packages can list the preset's other hosts in place of `github` and leave those out.

### Changing a list

Edit the file and restart the agent. A class whose entries changed loses the addresses its old entries opened. A class that loses its `egress` key, or leaves the config, runs its next jobs on the job network, and the agent removes its network. Until the restart, `doctor` says the config's list is not the one the agent runs.

### When the agent stops, and when the rules go

No job of an egress class runs without the agent, because nobody else puts its rules back:

- **Stopping, restarting or upgrading the agent stops the class's running jobs.** Each gets 30 seconds to cancel its job and tell GitHub, while the forwarder still answers. A job that survives a crash of the agent is removed when the agent starts again, before it builds the rules. Drain the box first to let jobs finish ([upgrading](upgrading.md#rebooting-the-box)).
- **The rules stay while the agent is stopped**, with no DNS for the class, so a container someone starts by hand on the class's network opens nothing new.
- **Deleting the nftables table** leaves the class closed: the host's iptables rules drop what leaves an egress bridge without the mark the table sets on what it lets through.
- **`nft flush ruleset` deletes the table and those iptables rules together** on a host whose iptables is iptables-nft, the default on Ubuntu and Debian, and `systemctl restart nftables` runs it with their default `/etc/nftables.conf`. Nothing then judges what the class's jobs send, and they can reach the host and the box's other containers until the agent notices. It checks the table every ten seconds, and when the table is gone it puts back the table and the iptables rules, and removes every job of an egress class, since a connection opened meanwhile would outlive the rules. The agent puts back the job network's rules with them. A flush also deletes Docker's rules, which come back only when Docker restarts. Do not flush the ruleset on a box with egress classes.
- **The class's Docker network removed**, as `docker network prune` does while no job runs on it: the class is not served until the agent makes it again on the same subnet, within ten seconds.

The agent checks the rules before it starts each runner of an egress class, and starts none while they are not in place, while the class's network is missing or while its forwarder does not listen. `status`, the `boxrunner_egress_ready` metric and `doctor` say why.

### What it does not stop

- **A destination that is allowed.** A job can send anything to an address the list opens, `github.com` included, and a job with credentials can store data at a service it reaches. The `github` preset reaches a few such services ([above](#the-github-preset)). Allow only what the class needs.
- **Shared addresses.** The rules judge addresses, not names inside TLS. A name whose address also serves other names, as CDNs do, opens the address for all of them, and a job that knows the address can send a request for any of them. A tunnel over an allowed host works.
- **A wildcard over a domain that others can register under**, such as `*.github.io`, lets a job resolve and reach any such name, and its lookups reach that domain's name server.
- **Jobs of one class share a bridge**, so they are no better separated from each other than jobs on the job network are.
- **The registry mirror.** With `docker = true` the mirror serves a job all of Docker Hub, whether or not the list names it, and the image names and tags a job asks for reach Docker Hub, so a job can carry data out in them.
- **The Actions cache.** With `cache = true` a job of the class saves cache entries that the jobs of the same repository in other classes restore, and the cache passes calls that are not cache calls on to GitHub's results service, listed or not. Leave `cache` off for a class that runs code you do not trust.
- **About ten seconds after a flush of the whole ruleset**, as [above](#when-the-agent-stops-and-when-the-rules-go).

### Seeing what was refused

`sudo boxrunner status` has a table per class with the packets dropped and the lookups refused since the agent started, and the latest refusals with the job that made each, the address or name and, when the address was opened for a name, that name. The agent logs each distinct refusal at most once a minute, and for each class no more than thirty a minute of each kind (dropped packets, refused lookups, addresses withheld from an answer and lookups it could not answer); the counters count them all. The metrics have `boxrunner_egress_blocked_total` by class and kind (`packet` or `lookup`) and `boxrunner_egress_ready`. `sudo boxrunner doctor` shows the same summary and proves the policy from a container on each class's network: an allowed host answers, a name outside the list does not resolve, an address of one is unreachable by number, and the host, the metadata service and the box's resolver are unreachable.

## Extending the job image

The job image is GitHub's official runner image plus common build tools. A class that needs more, such as browsers for end-to-end tests, gets its own Dockerfile:

```toml
[[class]]
name = "web"
cpus = 4
memory = "5GiB"
max = 1
dockerfile = "/etc/boxrunner/images/web/Dockerfile"
```

```dockerfile
ARG BASE
FROM ${BASE}
RUN npx -y playwright@1.56.0 install --with-deps chromium
```

The job image starts as root, and its entrypoint runs the job as `runner`. A class Dockerfile can end with `USER runner`, but then the entrypoint cannot start Docker, so a class with `docker = true` must end as root (`USER root`) and keep the entrypoint. The directory that holds the Dockerfile is the build context. The agent passes the job image as `BASE` and tags the result `boxrunner/class-<name>:local`. It rebuilds the class image when the job image is rebuilt for a new runner release, and when any file in that directory changes. After a rebuild it removes the old class image, unless a container still uses it.

## Metrics

With `metrics` set, the agent serves Prometheus metrics at `/metrics` on that address, in the text format, version 0.0.4. Without it, the agent opens no port.

```toml
metrics = "127.0.0.1:9464"
```

A Prometheus on the box scrapes it with:

```yaml
scrape_configs:
  - job_name: boxrunner
    static_configs:
      - targets: ["127.0.0.1:9464"]
```

The values come from the same snapshot as `sudo boxrunner status`, which the agent refreshes at least every five seconds. Counters and histograms start at zero when the agent starts, as actions-runner-controller's do when its listener restarts; `rate()` and `increase()` handle that.

### Who can read them

The endpoint has no authentication, and the metrics name your repositories, classes, box and versions. So `metrics` must be a loopback address unless the config also sets `metrics_public = true`. Before you set it, prefer a scraper on the box that forwards the samples, or an SSH tunnel to `127.0.0.1:9464`. If another machine must scrape the port directly, listen on the one address it reaches, such as a VPN address, rather than `0.0.0.0`, and let only that machine in through the host's firewall. boxrunner does not open the port in any firewall.

Jobs cannot reach the port, whatever the address. boxrunner's firewall drops everything the host receives from the job bridge, on every address and port, except DNS, the [registry mirror](#docker-inside-jobs) and the [Actions cache](#actions-cache), and a job's `127.0.0.1` is its own container. `sudo boxrunner doctor` tries the metrics port from a job container and fails if anything answers.

### The metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `boxrunner_class_runners` | gauge | `class`, `phase` | Runners of the class in each phase: `requested`, `starting`, `running`, `removing` or `exited`. |
| `boxrunner_class_max_runners` | gauge | `class` | The class's `max`. |
| `boxrunner_class_held` | gauge | `class` | 1 while the class holds its starts because its runners exit before taking a job, else 0. |
| `boxrunner_egress_ready` | gauge | `class` | 1 while the agent serves the class with its [egress allowlist](#egress-allowlist) in force, else 0. Only for classes with `egress`. |
| `boxrunner_egress_blocked_total` | counter | `class`, `kind` | What the class's allowlist refused since the agent started: `packet`s dropped and `lookup`s refused. |
| `boxrunner_refused_jobs_total` | counter | `name`, `organization`, `repository` | Jobs an [organization scale set](#on-the-box) was offered from a repository this box does not serve, since the agent started. They wait at GitHub. Only once there is one. |
| `gha_assigned_jobs` | gauge | `name`, `organization`, `repository` | Jobs GitHub assigned to the scale set. |
| `gha_completed_jobs_total` | counter | `name`, `organization`, `repository`, `job_result` | Jobs that finished since the agent started. |
| `gha_job_execution_duration_seconds` | histogram | `name`, `organization`, `repository`, `job_result` | From when a runner took the job to when it finished. |
| `boxrunner_job_queue_duration_seconds` | histogram | `name`, `organization`, `repository`, `job_result` | From when the job was queued to when a runner took it, counted when the job finishes. |
| `boxrunner_disk_free_bytes` | gauge | | Free space on Docker's or Sysbox's filesystem, whichever has less. |
| `boxrunner_disk_floor_bytes` | gauge | | The `disk_floor`. |
| `boxrunner_images_bytes` | gauge | | What the job, class and runner images take after the last prune; absent until measured. |
| `boxrunner_runner_image_info` | gauge | `version` | 1, with the Actions runner version the job image was built from; absent until built. |
| `boxrunner_runner_image_building_info` | gauge | `version` | 1, with the job image the agent is building, or whose class images it is building; absent while it builds nothing. |
| `boxrunner_agent_info` | gauge | `box`, `version` | 1, with the agent's version. |
| `boxrunner_worker_info` | gauge | `version` | 1, with the Worker's version; absent until the Worker answers with one. |
| `boxrunner_draining` | gauge | | 1 while the box drains after `sudo boxrunner drain`, else 0. |
| `boxrunner_heartbeat_last_success_timestamp_seconds` | gauge | | When the Worker last took a heartbeat, in seconds since 1970; 0 before the first. |

Three metrics mean the same as [actions-runner-controller's listener metrics](https://docs.github.com/en/actions/tutorials/use-actions-runner-controller/deploy-runner-scale-sets#enabling-metrics) and take their names and labels ([source](https://github.com/actions/actions-runner-controller/blob/f0d0b1d4399189c42954262be5defb22edc94a44/cmd/ghalistener/metrics/metrics.go)): `gha_assigned_jobs`, `gha_completed_jobs_total` and `gha_job_execution_duration_seconds`. In them, `name` is the class, which is the scale set's name, as in ARC, `organization` the repository's owner and `repository` its name: `octo/site` is `organization="octo"`, `repository="site"`. They leave out the labels boxrunner has no value for (`enterprise`, `namespace`, `event_name`) and the job's name and workflow, which would add a series for every job. The others have names of their own because they differ: ARC's `gha_job_startup_duration_seconds` counts from when the scale set got the job, not from when the job was queued, and its `gha_max_runners` is a scale set's, while a class's `max` holds for all its repositories together.

The durations are histograms, not summaries. A summary computes its quantiles on the box, over a window it chooses, and quantiles from several boxes or repositories cannot be added up. A histogram is a set of counters that Prometheus adds up and turns into any quantile with `histogram_quantile`, and it is what ARC uses. The buckets, in seconds, are 1, 5, 10, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200 and 21600, which is GitHub's six-hour limit on a job.

For example, the time that nine jobs in ten waited for a runner over the last day, per class, and a box whose heartbeats stopped:

```promql
histogram_quantile(0.9, sum by (name, le) (increase(boxrunner_job_queue_duration_seconds_bucket[1d])))
time() - boxrunner_heartbeat_last_success_timestamp_seconds > 300
```
