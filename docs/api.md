# Worker API

The agent talks to its Worker over HTTPS with JSON bodies. This page is the contract both sides test against.

## Conventions

- Every request carries `Authorization: Bearer <box token>` and `Boxrunner-Version: <agent version>`.
- Every response carries `Boxrunner-Protocol: 1`, `Boxrunner-Worker-Version: <the Worker's release>` and `Cache-Control: no-store`. The agent shows the Worker's release in `status` and `doctor`; a Worker from before the header sends none.
- Errors have one shape, `{"error": "<code>", "message": "<human sentence>"}`. Only `session_exists` adds a field, `heldBy`.
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
| 409 | `session_exists` | Another session holds the scale set; `heldBy` names the box that opened it when that is another box that still reports | Retries with backoff; with `heldBy`, tries again within five minutes and names that box in `status` and `doctor` |
| 409 | `runner_busy` | The runner already took a job | Leaves the runner alone |
| 410 | `repo_ineligible` | Not in the list `GET /v1/repos` returns: public, archived, or the App is no longer installed | Stops serving the repository |
| 426 | `upgrade_required` | The agent's major version differs from the Worker's | Stops and logs the upgrade command |
| 429 | `rate_limited` | Too many requests for this box token | Backs off |
| 502 | `github_error` | GitHub answered with an error; `message` names the call and status | Backs off |
| 503 | `not_configured` | Setup has not finished, or `CLASSES` is unreadable (the message says why) | Stops and logs the setup URL |

Repositories are named `owner/name` and matched case-insensitively. Scale set ids are numbers. Session ids are UUIDs.

`CLASSES` is a comma-separated list of lower-case letters, digits and dashes. A label GitHub's own runners answer to (`ubuntu-*`, `windows-*`, `macos-*`, `self-hosted`) makes it unreadable, because a scale set with that label would take jobs meant for GitHub's machines.

## `GET /v1/repos`

The private, unarchived repositories the App is installed on, narrowed to the `REPOS` variable when it is set, and the classes the box should serve. The Worker caches the repository list for a minute. Listing needs a token for each whole installation, so that token gets `metadata: read` and nothing else; every other installation token is narrowed to one repository.

```json
{
  "repos": [
    {
      "name": "octo/site",
      "id": 123,
      "forkPullRequests": { "runWorkflows": true, "requireApproval": true, "sendWriteTokens": false, "sendSecrets": false }
    }
  ],
  "classes": ["boxrunner"],
  "paused": ["boxrunner-light"]
}
```

`forkPullRequests` holds the repository's settings for workflows from fork pull requests, which GitHub keeps under Settings, Actions, General, Fork pull request workflows: whether they run at all, whether each run waits for approval, and whether it gets a write token or the repository's secrets. The Worker reads them with `GET /repos/{owner}/{repo}/actions/permissions/fork-pr-workflows-private-repos` and a token narrowed to that repository and `administration: read`. It reads a repository at most once an hour, and ten minutes after a read that failed. Each listing reads at most five of the repositories that are due, so a Worker that serves many spreads the reads over several listings, at two GitHub calls each. Until a read succeeds the field is `null`, and a failed read keeps the last settings it read; a failed read never fails the listing. A Worker from before this field sends none, and an agent that does not know it ignores it. `boxrunner doctor` and the [dashboard](dashboard.md) warn about a repository whose fork pull requests run workflows without approval, with write tokens or with secrets; the agent serves the repository either way.

`classes` is `CLASSES` without the classes paused on the [dashboard](dashboard.md#pausing-a-class), and `paused` lists those. An agent that does not know `paused` still stops serving a paused class, because the class is missing from `classes`; it logs that the class is not one of the Worker's. The agent stops listening to a paused class's scale sets, so it starts no runner for it, and lists the repositories again as soon as a heartbeat's answer shows the paused classes changed.

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

`409 session_exists` while an earlier session is still open. The Worker keeps which box opened or last refreshed each session until that box closes it. When that is a box other than `owner` whose last heartbeat is at most two minutes old, the answer names it:

```json
{ "error": "session_exists", "message": "box-2 holds the session of scale set 7, so it serves octo/site boxrunner.", "heldBy": "box-2" }
```

A session `owner` itself left open, or one of a box that stopped reporting, answers without `heldBy`: GitHub releases such a session on its own. `404 not_found` when GitHub no longer has the scale set; the Worker forgets it, so the next `POST /v1/scalesets` creates a new one.

## `PATCH /v1/sessions/{sessionId}`

Refreshes a session whose queue token expired or will soon. The body names the repository, the scale set and the box that holds the session, and the response has the same shape as opening one.

```json
{ "repo": "octo/site", "scaleSetId": 7, "owner": "box-1" }
```

`owner` is optional. When it is present, the Worker records that box as the session's holder, the same way opening does, so a session opened before the Worker kept holders is named in a `409` after its next refresh. The agent refreshes every hour. Older agents leave `owner` out.

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

Reports the box's state for the dashboard ([dashboard.md](dashboard.md)). The agent sends one every 30 seconds: 2,880 requests a day per box, 120 an hour of the token's 1,800, and under 3% of the free plan's 100,000 requests a day. Answers 200 with the classes paused on the dashboard, as in `GET /v1/repos`:

```json
{ "paused": ["boxrunner-light"] }
```

A Worker from before pausing answers 204 with no body, which the agent reads as nothing paused.

```json
{
  "box": "box-1",
  "version": "0.2.0",
  "latest": "0.2.2",
  "image": "2.338.0+3f9a2c0b1d4e",
  "disk": { "free": 26000000000, "floor": 10000000000 },
  "classes": [
    { "name": "boxrunner", "priority": "normal", "max": 1 },
    { "name": "boxrunner-light", "priority": "idle", "max": 2, "heldUntil": "2026-10-14T09:02:00Z" }
  ],
  "runners": [
    { "name": "box-1-boxrunner-3f9a2c", "repo": "octo/site", "class": "boxrunner", "state": "running", "since": "2026-10-14T09:00:00Z" }
  ],
  "scaleSets": [{ "repo": "octo/site", "class": "boxrunner", "assigned": 2 }],
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
| `latest` | Optional. boxrunner's latest release as the agent last found it, a semver; the dashboard compares the agent's and the Worker's release with it |
| `image` | The job image's identity, with ` from ` and the digest of the runner image it is built from once the agent knows it, 1 to 128 characters without control characters |
| `disk.free`, `disk.floor` | Bytes, whole numbers of 0 or more |
| `classes` | At most 32. `priority` is `normal` or `idle`; `max` is a whole number of 1 or more. Optional `heldUntil`: present while the class's runners fail before taking a job, so the box holds its starts until then and starts one at a time after |
| `runners` | At most 128. `state` is `requested`, `starting`, `running`, `removing` or `exited`; `since` is when it entered that state |
| `draining` | Optional, `false` when missing. `true` while `boxrunner drain` keeps the box from serving |
| `scaleSets` | Optional, empty when missing. At most 128: the scale sets the box listens to, those with jobs assigned first. `assigned` counts the jobs, including the ones runners already took. With `runners`, it tells the Worker which jobs wait with no runner, for [alerts](alerts.md#when-jobs-wait) and the dashboard |
| `jobs` | The most recently finished jobs, at most 50. `name` is 1 to 200 characters; `result` is GitHub's, in lower case (`succeeded`, `failed`, …); `started` is `null` for a job canceled before it started; `queued` is `null` when the agent did not record it |

Times are RFC 3339. Repositories and classes are stored as the box sends them: the box is trusted with its own state, so the Worker does not check them against `GET /v1/repos` or `CLASSES`. Fields the Worker does not know are ignored, so a newer agent can add some. A malformed field gets `400 bad_request` naming it, as in `"runners[0].state" is missing or malformed.`

The body must fit in 16 KB like every other call. Fifty jobs with long repository and job names can pass that, so the agent leaves out the oldest jobs until it fits.

With [fallback](fallback.md) or [alerts](alerts.md) on, the time a heartbeat arrives also decides when the Worker sends jobs to GitHub's runners and back, and when it posts an alert. Its `scaleSets` and `runners` decide which jobs wait, and for how long. A heartbeat never waits for GitHub or the webhook: a Durable Object alarm makes those calls.

The Worker keeps the latest heartbeat of each box with the time it arrived. It stores jobs by runner name, so a job sent in every heartbeat is stored once and a changed one replaces it. Jobs are kept for 30 days, and so is a box that stopped reporting. A heartbeat that repeats the previous one writes 2 rows in the Durable Object (*measured*); the free plan allows 100,000 a day.

## `POST /webhook`

GitHub's webhook deliveries for the App, when [unserved jobs](unserved-jobs.md) are on. The agent never calls it, and it follows GitHub's conventions instead of the ones above: no box token, no `Boxrunner-Version` and no budget, since a delivery without the right signature changes nothing. Its answers carry the same headers and error shape.

**Order of checks.** A webhook secret, `WEBHOOK_SECRET` or the one setup kept (`503 not_configured`), a body of at most 256 KB, by `Content-Length` and by the bytes read (`400 bad_request`), then `X-Hub-Signature-256`, `sha256=` and the hex HMAC-SHA256 of the body with that secret, compared in constant time (`401 unauthorized`).

A signed delivery whose `X-GitHub-Event` is not `workflow_job`, `ping` included, answers 204 and changes nothing. A `workflow_job` delivery answers 204, or `400 bad_request` naming a field it cannot read, as in `"workflow_job.labels" is missing or malformed.`. The Worker reads these fields and ignores the rest:

| Field | Rule |
|---|---|
| `action` | `queued`, `in_progress`, `completed` or `waiting`; any other action answers 204 and changes nothing |
| `workflow_job.id` | A whole number of 1 or more |
| `workflow_job.labels` | At most 32, each 1 to 100 characters |
| `workflow_job.name`, `workflow_job.workflow_name` | 1 to 200 characters; `workflow_name` may be `null` |
| `workflow_job.html_url` | An `https://github.com/` address of at most 512 characters |
| `workflow_job.created_at` | RFC 3339 |
| `repository.full_name`, `repository.private`, `repository.archived` | `owner/name` and two booleans |

## What the Worker checks on every call

The box token, compared in constant time. Then, on every call but `POST /v1/heartbeat`, which only reports the box's own state:

- The repository is in the list `GET /v1/repos` would return.
- The class is in `CLASSES`.
- The scale set id belongs to that repository and was created for one of the Worker's classes.
- Before a session opens, is refreshed or a runner is minted: the repository is still private, read fresh from GitHub. If it turned public, the Worker deletes every scale set it made for the repository and answers `410 repo_ineligible`; if deleting fails, it answers `502 github_error` and deletes on the next call. An archived repository or one the App left also gets `410 repo_ineligible`.
- When `GET /v1/repos` reads the list fresh (at most once a minute), the Worker deletes the scale sets of every repository it holds them for and no longer serves. Where the App was removed it cannot delete them and only forgets them; any other failure waits for the next listing.
- Installation tokens name one repository and only the permission the call needs: `metadata: read` to read visibility, `administration: write` to register runners, `actions_variables: write` to set or delete the [fallback](fallback.md) variable.

## Talking to the queue

These calls go from the agent straight to GitHub with the session's `queueToken`, never through the Worker.

- **Get a message.** `GET {queueUrl}` with the query parameter `lastMessageId={n}` added, `Authorization: Bearer {queueToken}`, `Accept: application/json; api-version=6.0-preview` and `X-ScaleSetMaxCapacity: {free capacity}`. It holds for up to about 50 seconds. 200 returns a message. 202 means none arrived. 401 means the token expired, so refresh the session.
- **Acknowledge a message.** `DELETE` the queue URL with `/{messageId}` appended to its path, after the message's jobs are acquired. 204 means done.
- **Acquire jobs.** `POST {acquireJobsUrl}` with a JSON array of the offered jobs' `runnerRequestId` values.

The Worker builds `acquireJobsUrl` from the Actions service URL of the repository's admin connection, as `actions/scaleset` does. The message format follows `actions/scaleset`'s `RunnerScaleSetMessage`.
