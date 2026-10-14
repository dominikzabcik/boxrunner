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

The Worker uses `GITHUB_CLIENT_SECRET` only when setup did not keep a secret. It reads the App's client id and owner from `GET /app` with the App's JWT the first time someone signs in, and keeps them.

## What it shows

- **Each box.** Its name, agent version, job image and free disk against the floor. "Online, last seen 12 seconds ago" while heartbeats arrive. "Offline, last seen 5 minutes ago" in red once the last one is more than two minutes old.
- **Its classes.** Priority, how many runners are running and how many are starting (requested or starting), against `max`.
- **Its runners.** Name, repository, class, state and for how long it has been in that state.
- **Recent jobs.** The last 30: repository, class, job, result, how long the job waited in the queue, how long it took and when it finished.
- **GitHub minutes not billed.** Jobs and minutes today (UTC) and in the last 30 days, each job rounded up to whole minutes as GitHub bills them. The dollar value uses $0.006 a minute, the price GitHub publishes for its standard 2-core Linux runner. Minutes within your plan's free allowance would not have cost money, so this is what the minutes are worth, not necessarily what you saved.
- **What the Worker serves.** The repositories as the agent last fetched them with `GET /v1/repos`, and the Worker's `CLASSES`.

The page works without JavaScript; reload it to update it. With JavaScript, a small inline script fetches `GET /dashboard.json` every 10 seconds while the tab is visible, and stops while it is hidden. `dashboard.json` holds the same data and, under `html`, the same part of the page rendered by the Worker, which the script puts in place. The page has one renderer, on the Worker. While the keyboard focus is inside that part of the page (for example on a table that scrolls sideways on a phone), the script leaves it alone.

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
- **Actions.** Every form that changes something carries a CSRF token, an HMAC of the session. The only action today is signing out (`POST /logout`).
- **The page.** Server-rendered HTML with no requests to other origins. The CSP allows exactly the page's inline style and script by their hashes, `connect-src 'self'` for the refresh, and forms only to the Worker. `X-Frame-Options: DENY`, `noindex` and `Cache-Control: no-store`.

## Endpoints

| Endpoint | Does |
|---|---|
| `GET /` | The dashboard with a session; otherwise the sign-in page, or a link to `/setup` before setup |
| `GET /dashboard.json` | The dashboard's data with a session; `401 unauthorized` without one |
| `GET /login` | Sends the browser to GitHub to sign in |
| `GET /login/callback` | Where GitHub sends the browser back |
| `POST /logout` | Ends the session; needs the CSRF token |
