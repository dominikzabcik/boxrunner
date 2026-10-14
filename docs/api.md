# Worker API

The agent talks to its Worker over HTTPS with JSON bodies. This page is the contract both sides test against.

## Conventions

- Every request carries `Authorization: Bearer <box token>` and `Boxrunner-Version: <agent version>`.
- Every response carries `Boxrunner-Protocol: 1` and `Cache-Control: no-store`.
- Errors have one shape, `{"error": "<code>", "message": "<human sentence>"}`.
- Any other method or path answers `404 not_found` in the same shape.

**Versions.** `Boxrunner-Version` is the agent's semver (`1.4.2`, `0.3.0-rc.1`). The Worker speaks protocol 1 and serves agent majors 0 and 1. Any other major, a missing header or one that is not semver gets `426 upgrade_required`, whose message names what the agent sent.

**Order of checks.** Version (426), setup finished and `CLASSES` readable (503), box token (401), budget (429), then the body or query (400) and the endpoint's own checks.

**Budget.** Each box token may make 120 requests at once and then one every two seconds, about 1,800 an hour, under half of the free plan's 100,000 requests a day. A request over the budget gets `429 rate_limited` with `Retry-After` in seconds. The budget lives in the Durable Object's memory, so a restart refills it, which only happens after the Worker sat idle. Requests with a wrong token are refused before they spend it. A new box token starts with a full budget.

| Status | `error` | Meaning | The agent |
|---|---|---|---|
| 400 | `bad_request` | Malformed body or a missing field | Logs it as a bug and does not retry |
| 400 | `unknown_class` | The class, or the class a scale set was made for, is not in the Worker's `CLASSES` | Stops serving that class and logs which classes exist |
| 401 | `unauthorized` | Missing or wrong box token | Stops and logs that the token is wrong |
| 404 | `not_found` | Unknown scale set, session or runner for this repository, or a scale set GitHub no longer has | Treats it as already gone; for a scale set, calls `POST /v1/scalesets` again |
| 409 | `session_exists` | Another session holds the scale set | Retries with backoff |
| 409 | `runner_busy` | The runner already took a job | Leaves the runner alone |
| 410 | `repo_ineligible` | Not in the list `GET /v1/repos` returns: public, archived, or the App is no longer installed | Stops serving the repository |
| 426 | `upgrade_required` | The agent's major version differs from the Worker's | Stops and logs the upgrade command |
| 429 | `rate_limited` | Too many requests for this box token | Backs off |
| 502 | `github_error` | GitHub answered with an error; `message` names the call and status | Backs off |
| 503 | `not_configured` | Setup has not finished, or `CLASSES` is unreadable (the message says why) | Stops and logs the setup URL |

Repositories are named `owner/name` and matched case-insensitively. Scale set ids are numbers. Session ids are UUIDs.

`CLASSES` is a comma-separated list of lower-case letters, digits and dashes. A label GitHub's own runners answer to (`ubuntu-*`, `windows-*`, `macos-*`, `self-hosted`) makes it unreadable, because a scale set with that label would take jobs meant for GitHub's machines.

## `GET /v1/repos`

The private, unarchived repositories the App is installed on, narrowed to the `REPOS` variable when it is set, and the classes the Worker serves. The Worker caches the list for a minute. Listing needs a token for each whole installation, so that token gets `metadata: read` and nothing else; every other installation token is narrowed to one repository.

```json
{
  "repos": [{ "name": "octo/site", "id": 123 }],
  "classes": ["boxrunner", "boxrunner-light"]
}
```

## `POST /v1/scalesets`

Finds the repository's scale set for a class, or creates it. Its name and its single label are the class name, in the default runner group. Runner self-update stays on. A scale set that already exists at GitHub under that name is adopted rather than duplicated, and concurrent calls create one.

Request:

```json
{ "repo": "octo/site", "class": "boxrunner" }
```

Response:

```json
{ "id": 7, "name": "boxrunner" }
```

## `POST /v1/sessions`

Opens a message session for a scale set. `owner` is the box name: up to 64 letters, digits, dots, dashes or underscores, starting with a letter or digit. The response holds everything the agent needs to talk to the queue directly.

Request:

```json
{ "repo": "octo/site", "scaleSetId": 7, "owner": "box-1" }
```

Response:

```json
{
  "sessionId": "5d6e…",
  "queueUrl": "https://…",
  "queueToken": "eyJ…",
  "acquireJobsUrl": "https://…/_apis/runtime/runnerscalesets/7/acquirejobs?api-version=6.0-preview",
  "statistics": {
    "totalAvailableJobs": 0,
    "totalAcquiredJobs": 0,
    "totalAssignedJobs": 1,
    "totalRunningJobs": 0,
    "totalRegisteredRunners": 0,
    "totalBusyRunners": 0,
    "totalIdleRunners": 0
  }
}
```

`409 session_exists` while an earlier session is still open. `404 not_found` when GitHub no longer has the scale set; the Worker forgets it, so the next `POST /v1/scalesets` creates a new one.

## `PATCH /v1/sessions/{sessionId}`

Refreshes a session whose queue token expired or will soon. The body names the repository and scale set, and the response has the same shape as opening one.

```json
{ "repo": "octo/site", "scaleSetId": 7 }
```

`404 not_found` when the session is gone; open a new one.

## `DELETE /v1/sessions/{sessionId}?repo=octo/site&scaleSetId=7`

Closes a session. Answers 204, including when the session was already gone.

## `POST /v1/runners`

Mints a one-job runner config for a scale set. The Worker checks the repository's visibility first. `name` is unique on the box and at most 64 characters of `[a-z0-9-]`.

Request:

```json
{ "repo": "octo/site", "scaleSetId": 7, "name": "box-1-boxrunner-3f9a2c" }
```

Response:

```json
{ "runnerId": 41, "jitConfig": "eyIucnVubmVyIjoi…" }
```

The agent passes `jitConfig` to the container as `ACTIONS_RUNNER_INPUT_JITCONFIG`, never on a command line.

## `DELETE /v1/runners/{runnerId}?repo=octo/site&scaleSetId=7`

Removes a runner that has not taken a job. Answers 204, including when the runner was already gone, `409 runner_busy` when it is running a job, and `404 not_found` when the runner belongs to another scale set.

## `POST /v1/heartbeat`

Reports the box's state for the dashboard ([dashboard.md](dashboard.md)). The agent sends one every 30 seconds: 2,880 requests a day per box, 120 an hour of the token's 1,800, and under 3% of the free plan's 100,000 requests a day. Answers 204.

```json
{
  "box": "box-1",
  "version": "0.2.0",
  "image": "2.338.0+3f9a2c0b1d4e",
  "disk": { "free": 26000000000, "floor": 10000000000 },
  "classes": [{ "name": "boxrunner", "priority": "normal", "max": 1 }],
  "runners": [
    { "name": "box-1-boxrunner-3f9a2c", "repo": "octo/site", "class": "boxrunner", "state": "running", "since": "2026-10-14T09:00:00Z" }
  ],
  "jobs": [
    {
      "runner": "box-1-boxrunner-3f9a2c",
      "repo": "octo/site",
      "class": "boxrunner",
      "name": "test",
      "result": "succeeded",
      "queued": "2026-10-14T08:59:40Z",
      "started": "2026-10-14T09:00:05Z",
      "finished": "2026-10-14T09:03:10Z"
    }
  ]
}
```

| Field | Rule |
|---|---|
| `box` | The box name, as `owner` in `POST /v1/sessions` |
| `version` | The agent's semver, at most 64 characters |
| `image` | The job image's identity, 1 to 128 characters without control characters |
| `disk.free`, `disk.floor` | Bytes, whole numbers of 0 or more |
| `classes` | At most 32. `priority` is `normal` or `idle`; `max` is a whole number of 1 or more |
| `runners` | At most 128. `state` is `requested`, `starting`, `running`, `removing` or `exited`; `since` is when it entered that state |
| `jobs` | The most recently finished jobs, at most 50. `name` is 1 to 200 characters; `result` is GitHub's, in lower case (`succeeded`, `failed`, …); `started` is `null` for a job canceled before it started |

Times are RFC 3339. Repositories and classes are stored as the box sends them: the box is trusted with its own state, so the Worker does not check them against `GET /v1/repos` or `CLASSES`. Fields the Worker does not know are ignored, so a newer agent can add some. A malformed field gets `400 bad_request` naming it, as in `"runners[0].state" is missing or malformed.`

The body must fit in 16 KB like every other call. Fifty jobs with long repository and job names can pass that, so the agent leaves out the oldest jobs until it fits.

The Worker keeps the latest heartbeat of each box with the time it arrived. It stores jobs by runner name, so a job sent in every heartbeat is stored once and a changed one replaces it. Jobs are kept for 30 days, and so is a box that stopped reporting. A heartbeat that repeats the previous one writes 2 rows in the Durable Object (*measured*); the free plan allows 100,000 a day.

## What the Worker checks on every call

The box token, compared in constant time. Then, on every call but `POST /v1/heartbeat`, which only reports the box's own state:

- The repository is in the list `GET /v1/repos` would return.
- The class is in `CLASSES`.
- The scale set id belongs to that repository and was created for one of the Worker's classes.
- Before a session opens, is refreshed or a runner is minted: the repository is still private, read fresh from GitHub. If it turned public, the Worker deletes every scale set it made for the repository and answers `410 repo_ineligible`; if deleting fails, it answers `502 github_error` and deletes on the next call. An archived repository or one the App left also gets `410 repo_ineligible`.
- Installation tokens name one repository and only the permission the call needs: `metadata: read` to read visibility, `administration: write` to register runners.

## Talking to the queue

These calls go from the agent straight to GitHub with the session's `queueToken`, never through the Worker.

- **Get a message.** `GET {queueUrl}` with the query parameter `lastMessageId={n}` added, `Authorization: Bearer {queueToken}`, `Accept: application/json; api-version=6.0-preview` and `X-ScaleSetMaxCapacity: {free capacity}`. It holds for up to about 50 seconds. 200 returns a message. 202 means none arrived. 401 means the token expired, so refresh the session.
- **Acknowledge a message.** `DELETE` the queue URL with `/{messageId}` appended to its path, after the message's jobs are acquired. 204 means done.
- **Acquire jobs.** `POST {acquireJobsUrl}` with a JSON array of the offered jobs' `runnerRequestId` values.

The Worker builds `acquireJobsUrl` from the Actions service URL of the repository's admin connection, as `actions/scaleset` does. The message format follows `actions/scaleset`'s `RunnerScaleSetMessage`.
