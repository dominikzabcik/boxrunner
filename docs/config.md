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
| `disk_floor` | `10GB` | No runner starts while Docker's data directory has less free space than this. Below half of it, the agent stops the job container that wrote the most. |

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
| `repos` | every repository | Only these repositories, as `["owner/name", …]`. Each class opens one scale set in every repository it serves, so a class meant for one project should name it. |

Memory is not prioritised the way CPU is. Give the classes limits that fit side by side: the defaults give the normal class about 75% of the box's memory and each idle runner about 40%.

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
USER root
RUN npx -y playwright@1.56.0 install --with-deps chromium
USER runner
```

The directory that holds the Dockerfile is the build context. The agent passes the job image as `BASE` and tags the result `boxrunner/class-<name>:local`. It rebuilds the class image when the job image is rebuilt for a new runner release, and when any file in that directory changes.
