# Configuration

The agent reads `/etc/boxrunner/config.toml` when it starts. After a change, run `sudo systemctl restart boxrunner`. A running job keeps running through a restart.

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
| `repos` | every repository | Only these repositories, as `["owner/name", …]`. Each class opens one scale set in every repository it serves, so a class meant for one project should name it. |
| `warm` | none | Keeps one idle runner of this class ready for each of these repositories, as `["owner/name", …]`, so their jobs skip the cold start. Each must be one the class serves. A warm runner counts toward `max` and is never removed for being idle; when it takes a job, another starts once no other job is waiting for the place. Name fewer repositories than `max`, unless `repos` lists only warm repositories, so the class's other repositories always have a place. |
| `docker` | `false` | Gives each job its own Docker daemon; see below. |

Memory is not prioritised the way CPU is. Give the classes limits that fit side by side: the defaults give the normal class about 75% of the box's memory and each idle runner about 40%.

## Docker inside jobs

With `docker = true`, a class's jobs can use `services:`, `container:` jobs, Docker container actions and `docker build`. It needs Sysbox, which `sudo boxrunner install --docker` installs, as does any `install` while a class has `docker = true`. A new config written with `--docker` turns `docker` on for both classes. Sysbox supports Ubuntu 22.04 or newer and Debian 11 or newer on Linux 5.15 or newer, on amd64 and arm64. Docker keeps running while Sysbox installs: install first writes Docker's current bridge address and address pools into `/etc/docker/daemon.json`, so Sysbox's package only makes Docker reload its configuration.

Each job runs under Sysbox's `sysbox-runc` runtime, where root inside the container is an unprivileged user on the box, and starts its own Docker daemon before the runner. The job's Docker shares the class's CPU, memory and process limits and its priority, and the firewall applies to the containers it starts. The agent does not serve a class with `docker = true` while Docker has no `sysbox-runc` runtime; it logs why.

Every job starts with an empty Docker, so it pulls each image it uses again. Images from Docker Hub come through a registry mirror on the box: the first job that pulls an image fetches it from Docker Hub, and later jobs get it from the box's cache. The mirror is a `registry` container named `boxrunner-mirror` that listens on port 5000 of the `boxrunner0` bridge's own address, which only job containers can reach, and keeps its cache in the volume `boxrunner-mirror`. It forgets an image a week after fetching it. Images from other registries, such as `ghcr.io`, are pulled directly. The mirror runs while a class has `docker = true`; `install` and the agent start it, and `install` without such a class, or `uninstall`, removes it with its cache.

The job image has Docker Compose, so `docker compose up` works in these jobs.

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

The job image starts as root, and its entrypoint runs the job as `runner`. A class Dockerfile can end with `USER runner`, but then the entrypoint cannot start Docker, so a class with `docker = true` must end as root (`USER root`) and keep the entrypoint. The directory that holds the Dockerfile is the build context. The agent passes the job image as `BASE` and tags the result `boxrunner/class-<name>:local`. It rebuilds the class image when the job image is rebuilt for a new runner release, and when any file in that directory changes.
