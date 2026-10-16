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
- **The egress allowlist**, when a class has `egress`. Its jobs reach only the addresses its entries name, or that an allowed name resolved to for the answer's TTL, on the ports the entry gives, and nothing on the host but the class's DNS forwarder and the services it uses. A way for a job of such a class to open a connection to a destination its entries do not allow, to reach the host, another class, a private range it does not list or the metadata service, or to make the forwarder open an address for a name the entries do not allow, is a vulnerability. So is a way to do it that survives the agent stopping or someone deleting the nftables table. See [docs/config.md](docs/config.md#egress-allowlist) for what it covers and [docs/decisions.md](docs/decisions.md#an-egress-allowlist-per-class) for how.
- **The webhook.** `POST /webhook` acts only on deliveries signed with the App's webhook secret. A way to make the Worker keep or list a job with an unsigned or forged delivery, or to make it log a delivery's body, is a vulnerability.
- **The setup page.** A way to complete setup without the setup secret, or to replace an existing App, is a vulnerability.
- **The runner image.** The agent builds the job image only from a `ghcr.io/actions/actions-runner` digest that boxrunner's CI listed after verifying its Sigstore provenance attestation from actions/runner's release workflow for the same minor release, and only while the version's tag points to it ([docs/architecture.md](docs/architecture.md#image)). A way to make the agent build from another image is a vulnerability.
- **The Actions cache**, when a class has `cache = true`. It trusts only GitHub's signature on a job's runtime token, checked against the keys GitHub publishes at `token.actions.githubusercontent.com`: the token's repository id and cache scopes decide which entries a call may save and restore. It accepts only runtime tokens, which carry the `Actions.Results` scope, not the OIDC ID tokens the same issuer signs. It trusts upload and download URLs by their 256 random bits, and it answers only on the job bridge. A way for a job to read or write another repository's entries, to save into a scope its token may not write, such as a pull request's into the default branch's, to read or change an upload or download it was not given, or to change an entry once it is saved, is a vulnerability. So is a way for a job to make the cache hold more than `cache`, take the disk below `disk_floor` or exceed the limits in [docs/config.md](docs/config.md#actions-cache). Every call that is not a cache call goes on to GitHub's results service with the job's own token, as it would without the cache.

With [unserved jobs](docs/unserved-jobs.md) on, the App also holds the **Actions** permission (read), which lets it read the workflow runs, jobs, logs, artifacts and caches of the repositories it is installed on. The Worker needs it only to receive the `workflow_job` event and never puts it in a token: every token it makes names one repository and only the permission its call needs. Whoever obtained the App key could read that Actions data, on top of what the App can do without it.

Not defended:

- **Public repositories.** Anyone who can open a pull request can run code on a self-hosted runner. The Worker refuses public repositories; there is no switch.
- **Fork pull requests in private repositories.** Anyone who can read a private repository can fork it and open a pull request. When the repository runs workflows from fork pull requests, the fork's code runs on the box, unchecked unless each run needs approval, and with a write token or the repository's secrets if those settings send them. boxrunner leaves these settings to you; `boxrunner doctor` and the dashboard warn about each served repository that runs such workflows without approval, with write tokens or with secrets.
- **What an allowed destination does.** A job of an egress class can send anything to an address its list opens, `github.com` included. The rules judge addresses, so an allowed name whose address also serves other names, a tunnel over an allowed host, and a wildcard over a domain that others can register under are ways out that the list does not close.
- **Kernel isolation.** Jobs share the host's kernel, as every container does. A kernel exploit escapes any container. Once Sysbox is installed (`install --sysbox` or `--docker`), every class runs under it unless it sets `isolation = "runc"`, and Sysbox maps a job's root to an unprivileged user on the host. That narrows the risk but does not remove it. Under `runc`, Docker's default runtime, a step that becomes root through the job image's `sudo` is root on the host, held back only by the container's namespaces and Docker's default capabilities.
- **A job's own secrets.** A job can read its own `GITHUB_TOKEN`, workflow secrets and runner credentials, as on GitHub's runners.
- **Cache entries within a repository.** As on GitHub, any job that may write a scope can save what later runs of that scope restore. A workflow that restores a cache trusts the jobs that saved it.
- **Root on the box.** Whoever is root on the box controls the box token and every job on it.
- **The list's channel.** The agent trusts `runner-digests.json` as GitHub serves it over HTTPS from this repository's `digests` branch, and does not check the list's own attestation. Whoever can push to this repository can change the list, as they can change the agent.
- **What the job image adds.** Its packages come from Ubuntu's and GitHub's package repositories, which apt checks against their signing keys, and the Docker Compose plugin is checked by its SHA-256. The provenance check covers only the runner image underneath.
