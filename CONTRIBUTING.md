# Contributing

Thank you for helping. Bug reports with `boxrunner doctor` output, fixes, docs and tests are all welcome. For a new feature or a change to the design, open a [Discussion](../../discussions) first, so we agree on the shape before you write the code.

## What lives where

| Path | What it is |
|---|---|
| `cmd/boxrunner`, `internal/` | The agent: one Go binary that installs the box, runs jobs and checks itself |
| `image/` | The job image, embedded in the binary and built on the box |
| `worker/` | The Cloudflare Worker that holds the GitHub App key |
| `install.sh` | The installer; releases write the version and checksums into it |
| `docs/` | Architecture, the Worker API, configuration and the decisions behind them |

Read [docs/architecture.md](docs/architecture.md) before changing how the parts talk to each other. The design is a contract, so a change to it comes with a change to that page.

## Running the checks

The agent needs Go (the version in `go.mod`):

```sh
gofmt -l .                                              # prints nothing
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@2026.2.2 ./...
go test -race ./...
```

The Worker needs [Bun](https://bun.sh):

```sh
cd worker
bun install
bun run check                                           # types and tests, with a real Durable Object
```

Shell scripts pass `shellcheck`. CI runs all of this, plus gitleaks and zizmor, on every pull request.

## Trying a change on a real box

Any Ubuntu 24.04 VM or spare server will do. On a Mac, [OrbStack](https://orbstack.dev) gives you one in seconds (`orb create ubuntu:noble test`).

1. Deploy your own Worker from `worker/` with `bunx wrangler deploy`, set `SETUP_SECRET` with `bunx wrangler secret put SETUP_SECRET`, and finish setup at `/setup`. Install the App on one private test repository.
2. Build the agent: `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/boxrunner` (or `amd64`).
3. Copy it to the VM and run `sudo ./boxrunner install --worker <your worker> --token <box token>`, then `sudo boxrunner doctor`.
4. Push a workflow with `runs-on: boxrunner` to the test repository.

`sudo boxrunner uninstall` removes everything again.

## Pull requests

- One change per pull request, with tests that call the code the way its users do and compare against literal expected values.
- Title in the [Conventional Commits](https://www.conventionalcommits.org/) form, `fix(agent): …` or `feat(worker): …`, since it becomes the squashed commit and the changelog line.
- Say what changed for someone running boxrunner, and how you tested it.
- Comments explain why, not what.

By contributing, you agree your work is licensed under the [Apache License 2.0](LICENSE), as the license's section 5 says.

## Conduct

Everyone here follows the [Code of Conduct](CODE_OF_CONDUCT.md).
