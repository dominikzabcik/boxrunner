# Security

boxrunner runs other people's code next to a GitHub App key, so its security model is the product. [docs/architecture.md](docs/architecture.md) describes what each part holds and can do.

## Reporting a vulnerability

Report it privately through GitHub: the **Report a vulnerability** button on this repository's [Security tab](../../security/advisories/new). Please do not open a public issue.

Include what an attacker needs, what they get, and how to reproduce it. A rough report is better than none.

The maintainer works on boxrunner alone. Expect a first answer within seven days, and a fix or a plan within thirty. Once a fix is released, the advisory is published with credit, unless you prefer otherwise.

## Supported versions

Only the latest release gets security fixes. The installer always installs it. `sudo boxrunner upgrade` upgrades the agent, and [docs/upgrading.md](docs/upgrading.md) says how to upgrade the Worker. `boxrunner status`, `boxrunner doctor` and the dashboard say when a newer release is out.

## What boxrunner defends, and what it does not

Defended, and in scope for reports:

- **The GitHub App key.** It lives only in your Worker. A way for the box, a job or anyone on the internet to obtain it, or to make the Worker act beyond the private repositories and classes it serves, is a vulnerability.
- **The box token.** It can only ask the Worker for scale sets, sessions and one-job runner configs. A way to do more with it is a vulnerability.
- **One job, one container.** A way for a job to reach a later job, read another job's files, or reach the agent, is a vulnerability.
- **The firewall.** A way for a job to reach the host, private networks, a tailnet or the cloud metadata service through `boxrunner0` is a vulnerability.
- **The setup page.** A way to complete setup without the setup secret, or to replace an existing App, is a vulnerability.
- **The runner image.** The job image is built only from a `ghcr.io/actions/actions-runner` digest that has a Sigstore provenance attestation from actions/runner's release workflow for the same minor release ([docs/architecture.md](docs/architecture.md#image)). A way to make the agent build from an image without one is a vulnerability.

Not defended:

- **Public repositories.** Anyone who can open a pull request can run code on a self-hosted runner. The Worker refuses public repositories; there is no switch.
- **Kernel isolation.** Jobs share the host's kernel, as every container does. A kernel exploit escapes any container. Once Sysbox is installed (`install --sysbox` or `--docker`), every class runs under it unless it sets `isolation = "runc"`, and Sysbox maps a job's root to an unprivileged user on the host. That narrows the risk but does not remove it. Under `runc`, Docker's default runtime, a step that becomes root through the job image's `sudo` is root on the host, held back only by the container's namespaces and Docker's default capabilities.
- **A job's own secrets.** A job can read its own `GITHUB_TOKEN`, workflow secrets and runner credentials, as on GitHub's runners.
- **Root on the box.** Whoever is root on the box controls the box token and every job on it.
- **What the job image adds.** Its packages come from Ubuntu's and GitHub's package repositories, which apt checks against their signing keys, and the Docker Compose plugin is checked by its SHA-256. The provenance check covers only the runner image underneath.
