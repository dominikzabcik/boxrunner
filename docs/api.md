# Worker API

The agent talks to its Worker over HTTPS with JSON bodies. This page is the contract both sides test against.

## Conventions

- Every request carries `Authorization: Bearer <box token>` and `Boxrunner-Version: <agent version>`.
- Every response carries `Boxrunner-Protocol: 1` and `Cache-Control: no-store`.
- Errors have one shape, `{"error": "<code>", "message": "<human sentence>"}`.

| Status | `error` | Meaning | The agent |
|---|---|---|---|
| 400 | `bad_request` | Malformed body or a missing field | Logs it as a bug and does not retry |
| 400 | `unknown_class` | The class is not in the Worker's `CLASSES` | Stops serving that class and logs which classes exist |
| 401 | `unauthorized` | Missing or wrong box token | Stops and logs that the token is wrong |
| 404 | `not_found` | Unknown scale set, session or runner for this repository | Treats it as already gone |
| 409 | `session_exists` | Another session holds the scale set | Retries with backoff |
| 409 | `runner_busy` | The runner already took a job | Leaves the runner alone |
| 410 | `repo_ineligible` | Public, archived, or the App is no longer installed | Stops serving the repository |
| 426 | `upgrade_required` | The agent's major version differs from the Worker's | Stops and logs the upgrade command |
| 429 | `rate_limited` | Too many requests for this box token | Backs off |
| 502 | `github_error` | GitHub answered with an error; `message` names the call and status | Backs off |
| 503 | `not_configured` | Setup has not finished | Stops and logs the setup URL |

Repositories are named `owner/name`. Scale set ids are numbers. Session ids are UUIDs.

## `GET /v1/repos`

The private repositories the App is installed on, and the classes the Worker serves.

```json
{
  "repos": [{ "name": "octo/site", "id": 123 }],
  "classes": ["boxrunner", "boxrunner-light"]
}
```

## `POST /v1/scalesets`

Finds the repository's scale set for a class, or creates it. Its name and its single label are the class name. Runner self-update stays on.

Request:

```json
{ "repo": "octo/site", "class": "boxrunner" }
```

Response:

```json
{ "id": 7, "name": "boxrunner" }
```

## `POST /v1/sessions`

Opens a message session for a scale set. `owner` is the box name. The response holds everything the agent needs to talk to the queue directly.

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

`409 session_exists` while an earlier session is still open.

## `PATCH /v1/sessions/{sessionId}`

Refreshes a session whose queue token expired or will soon. The body names the repository and scale set, and the response has the same shape as opening one.

```json
{ "repo": "octo/site", "scaleSetId": 7 }
```

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

Removes a runner that has not taken a job. Answers 204, including when the runner was already gone, and `409 runner_busy` when it is running a job.

## What the Worker checks on every call

- The box token, compared in constant time.
- The repository is in the list `GET /v1/repos` would return.
- The class is in `CLASSES`.
- The scale set id belongs to that repository and was created for one of the Worker's classes.
- Before a session opens, is refreshed or a runner is minted: the repository is private. If it is not, the answer is `410 repo_ineligible` and the scale set is deleted.

## Talking to the queue

These calls go from the agent straight to GitHub with the session's `queueToken`, never through the Worker.

- **Get a message.** `GET {queueUrl}` with the query parameter `lastMessageId={n}` added, `Authorization: Bearer {queueToken}`, `Accept: application/json; api-version=6.0-preview` and `X-ScaleSetMaxCapacity: {free capacity}`. It holds for up to about 50 seconds. 200 returns a message. 202 means none arrived. 401 means the token expired, so refresh the session.
- **Acknowledge a message.** `DELETE` the queue URL with `/{messageId}` appended to its path, after the message's jobs are acquired. 204 means done.
- **Acquire jobs.** `POST {acquireJobsUrl}` with a JSON array of the offered jobs' `runnerRequestId` values.

The Worker builds `acquireJobsUrl` from the Actions service URL of the repository's admin connection, as `actions/scaleset` does. The message format follows `actions/scaleset`'s `RunnerScaleSetMessage`.
