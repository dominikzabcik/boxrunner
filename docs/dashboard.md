# Dashboard

Your Worker serves a dashboard at its own address, so there is nothing else to deploy. Open the Worker's address (`https://boxrunner.<your-subdomain>.workers.dev/`) and sign in with GitHub.

## Who can sign in

The account that owns the GitHub App, and the accounts listed in the optional `ADMINS` variable (GitHub logins, comma-separated, in `wrangler.jsonc` or the Cloudflare dashboard). Logins are compared case-insensitively. Anyone else who signs in gets a page saying which account they used and that it may not open the dashboard. The Worker checks the list on every request, so removing a login from `ADMINS` ends that account's session at once.

Signing in uses the App's own OAuth client. Setup keeps its client id, client secret and owner, and its manifest registers `<worker>/login/callback` as the App's callback URL.

### An App set up before the Worker kept its client secret

Its Worker shows how to sign in instead of the button:

1. Open the App's settings on GitHub (`https://github.com/settings/apps/<app slug>`).
2. Under **Client secrets**, generate a new client secret and copy it.
3. Under **Callback URL**, add `<worker>/login/callback` and save. Without it, GitHub answers that the `redirect_uri` is not associated with the application.
4. In the Worker's directory, run `npx wrangler secret put GITHUB_CLIENT_SECRET` and paste the secret.
5. Run `npx wrangler deploy`. The Worker reads its secrets when it starts, so a new secret takes effect with the next deployment.

The Worker uses `GITHUB_CLIENT_SECRET` only when setup did not keep a secret. It reads the App's client id and owner from `GET /app` with the App's JWT the first time someone signs in, and keeps them.

## What it shows

- **Each box.** Its name, the agent's and the Worker's release, each with the newer release when one is out, job image and free disk against the floor. "Online, last seen 12 seconds ago" while heartbeats arrive. "Offline, last seen 5 minutes ago" in red once the last one is more than two minutes old.
- **Its classes.** Priority, how many runners are running and how many are starting (requested or starting), against `max`.
- **Its runners.** Name, repository, class, state and for how long it has been in that state.
- **Jobs waiting for a runner.** Only while there are some: each repository and class with jobs assigned and no runner requested, starting or running, the box that reported it, how long it has waited (in red once it has waited long enough for an [alert](alerts.md#when-jobs-wait)) and the likely cause.
- **Jobs nobody here runs.** With [unserved jobs](unserved-jobs.md) on, the jobs GitHub's webhook reported queued that no box here will run: their label or repository is one no box serves, or no box took them within 10 minutes. Unlike the section above, it sees jobs no box was ever assigned. Repository, job with a link to it, `runs-on`, why and how long it has waited. "Off" with a link to that page until GitHub's first webhook delivery arrives, then when the last one did.
- **Recent jobs.** The last 30: repository, class, job, result, how long the job waited in the queue, how long it took and when it finished.
- **GitHub minutes not billed.** Jobs and minutes today (UTC) and in the last 30 days, each job rounded up to whole minutes as GitHub bills them. The dollar value uses $0.006 a minute, the price GitHub publishes for its standard 2-core Linux runner. Minutes within your plan's free allowance would not have cost money, so this is what the minutes are worth, not necessarily what you saved.
- **What the Worker serves.** The repositories as the agent last fetched them with `GET /v1/repos`, and the Worker's `CLASSES`, each serving or paused, with who paused it and when. A paused class also reads "(paused)" in each box's classes. A repository whose fork pull requests run workflows without approval, with write tokens or with secrets gets a warning that names the settings to change ([api.md](api.md#get-v1repos)).
- **Fallback to GitHub's runners.** Off, on and after how many minutes, or that the box is down and when the Worker switched. In red, why the Worker could not set or delete `BOXRUNNER_DOWN`, or why it cannot read `FALLBACK_MINUTES` ([fallback](fallback.md)).
- **Alerts.** Off, or the webhook's host and after how many minutes without a heartbeat, or of jobs waiting, the Worker posts to it. In red, why the last post failed, or why it cannot read `ALERT_WEBHOOK` or `ALERT_MINUTES` ([alerts](alerts.md)).

The page works without JavaScript; reload it to update it. With JavaScript, a small inline script fetches `GET /dashboard.json` every 10 seconds while the tab is visible, and stops while it is hidden. `dashboard.json` holds the same data and, under `html`, the same part of the page rendered by the Worker, which the script puts in place. The page has one renderer, on the Worker. While the keyboard focus is inside that part of the page (for example on a table that scrolls sideways on a phone), the script leaves it alone.

## Actions

Each action is a button that posts a form with the session's CSRF token, so it works without JavaScript.

### Pausing a class

**Pause** next to a class stops every box from starting new runners for it. Jobs that are running finish, and a runner that already started may still take a job. Every other job for the class waits at GitHub until someone presses **Resume**; the box then picks it up as it does after a restart. The pause is the Worker's, so it applies to every box, and it stays until someone resumes the class, across restarts of the box and the Worker. Pausing a paused class changes nothing; the page keeps who paused it first.

The agent stops listening to the paused class's scale sets, which closes their sessions. The Worker names the paused classes in its answer to every heartbeat, so a box learns of a pause or a resume within 30 seconds. An agent from before pausing existed learns of it when it next lists the repositories, within 5 minutes, because `GET /v1/repos` leaves paused classes out of `classes` ([api.md](api.md#get-v1repos)); its log then says the class is not one of the Worker's.

### Replacing the box token

**Replace the box token** makes a new token and sends the browser to a page that shows it once, with the command that puts it on a box:

```sh
sudo boxrunner install --token <new token>
```

It writes the token to `/etc/boxrunner/token` and restarts the agent; jobs that are running keep running, except those of classes with an [egress allowlist](config.md#when-the-agent-stops-and-when-the-rules-go). The old token stops working at once, so every box stops reaching the Worker until it has the new one: run the command on each.

The form's answer is a redirect (303) to `/box-token/{id}`, so a reload loads that page again instead of posting the form again and replacing the token a second time. The id is random and says nothing about the token. The Worker keeps the new token for one view of that page within a minute, in the Durable Object's memory and never in its storage, then keeps only the token's hash. A reload, a second visit or a visit after the minute answers 410 with a link back to the button, and replaces nothing. If the Worker restarts within that minute, the page cannot show the token either: replace it again. `POST /setup/token` does the same with the setup secret instead of a session, and shows the new token at `/setup/token/{id}`.

## Where the data comes from

The agent sends `POST /v1/heartbeat` every 30 seconds ([api.md](api.md#post-v1heartbeat)). The Worker keeps each box's latest heartbeat, the jobs of the last 30 days and a running total per day, all in its Durable Object. A page view reads only the Durable Object and never calls GitHub, so it costs no GitHub quota.

What it costs on Cloudflare's free plan:

| | Per day | Free plan allows |
|---|---|---|
| Heartbeats, one box | 2,880 requests | 100,000 requests |
| Rows written by heartbeats | about 5,800, plus a few per new job (*measured*: 2 for a repeated heartbeat, 6 when it adds one job) | 100,000 |
| A dashboard tab left open all day | 8,640 requests, about 65 rows read each | 100,000 requests, 5 million rows read |

Totals come from one row per day, so a refresh reads at most about 30 of them however many jobs ran.

## Security

- **Sessions.** The session cookie, `__Host-boxrunner-session`, holds the login, an expiry 12 hours out and an HMAC of both. It is `HttpOnly`, `Secure` and `SameSite=Strict`. The HMAC key is 32 random bytes made once and kept in the Durable Object; resetting setup forgets it, which ends every session. The Worker never stores the GitHub user token: it reads the login with it and drops it.
- **Signing in.** `GET /login` sets a `__Host-boxrunner-login` cookie (`SameSite=Lax`, 10 minutes) and sends GitHub a `state` the Worker signs for it. The callback refuses a state that does not match the cookie or is older than 10 minutes, before it asks GitHub anything. A `SameSite=Strict` cookie is not sent on a redirect that began at github.com, so the callback answers with a short page that moves on to the dashboard by itself.
- **Actions.** Every form that changes something carries a CSRF token, an HMAC of the session. Without a session an action answers 401, with a missing or wrong token 403, and nothing changes. The session cookie is `SameSite=Strict` as well, so another site's form arrives without it. There is no place to paste a token or a key: the App key arrives through the manifest flow and stays in the Worker.
- **The page.** Server-rendered HTML with no requests to other origins. The CSP allows exactly the page's inline style and script by their hashes, `connect-src 'self'` for the refresh, and forms only to the Worker. `X-Frame-Options: DENY`, `noindex` and `Cache-Control: no-store`.

## Endpoints

| Endpoint | Does |
|---|---|
| `GET /` | The dashboard with a session; otherwise the sign-in page, or a link to `/setup` before setup |
| `GET /dashboard.json` | The dashboard's data with a session; `401 unauthorized` without one |
| `GET /login` | Sends the browser to GitHub to sign in |
| `GET /login/callback` | Where GitHub sends the browser back |
| `POST /logout` | Ends the session; needs the CSRF token |
| `POST /classes/pause` | Pauses the class named by the form field `class`; needs a session and the CSRF token |
| `POST /classes/resume` | Resumes the class named by `class`; needs a session and the CSRF token |
| `POST /box-token` | Replaces the box token and redirects to `/box-token/{id}`; needs a session and the CSRF token |
| `GET /box-token/{id}` | Shows the new box token on the first view within a minute, then answers 410; needs a session |
| `POST /webhook` | GitHub's webhook deliveries, checked by their signature ([api.md](api.md#post-webhook)) |
