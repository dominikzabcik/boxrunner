# Alerts

While the box is down, its jobs wait in GitHub's queue for up to 24 hours, and nothing tells you. With alerts on, the Worker posts a short message to a webhook of yours when every box has gone silent, and one more when a box is back. ntfy, Discord and Slack read the same message. Alerts are off until you set a webhook.

## How it works

The agent sends the Worker a heartbeat every 30 seconds. When no box has sent one for `ALERT_MINUTES` (10 by default), the Worker posts:

> boxrunner: no box has reported since 2026-10-14 10:00 UTC. Queued jobs wait for it.

Once a box has reported for two minutes with no three heartbeats in a row missing, it posts:

> boxrunner: box-1 reports again.

Each change is posted once. An outage that ends before its alert got through sends nothing, so you never get a recovery message for an outage you did not hear of.

Ten minutes is long enough that an agent upgrade, which restarts the agent in seconds, or a reboot of the box usually passes without an alert, and short against the 24 hours a job may wait in the queue. Set `ALERT_MINUTES` for another threshold.

### With fallback on

Alerts and [fallback](fallback.md) share one notion of down. While fallback is on, `FALLBACK_MINUTES` decides when the box is down and `ALERT_MINUTES` is not used, so the alert goes out at the moment the Worker sets `BOXRUNNER_DOWN`, and the messages say where jobs go:

> boxrunner: no box has reported since 2026-10-14 10:00 UTC. Workflows that read BOXRUNNER_DOWN run on GitHub’s runners.

> boxrunner: box-1 reports again. Jobs go to the box again.

## Turn it on

1. **Make a webhook.**
   - **ntfy.** Use the topic's address, for example `https://ntfy.sh/<topic>`. Anyone who knows a topic on ntfy.sh can read it, so pick a long random name, or use your own server. The server must run ntfy 2.10 or later, which added templating.
   - **Discord.** In the channel's **Edit Channel**, **Integrations**, **Webhooks**, choose **New Webhook** and **Copy Webhook URL**.
   - **Slack.** Create an app with **Incoming Webhooks** turned on and add a webhook to the channel.
2. **Set `ALERT_WEBHOOK`** on the Worker as a secret, since whoever has the address can post to your channel: `bunx wrangler secret put ALERT_WEBHOOK`, or in the Cloudflare dashboard under the Worker's **Settings**, **Variables and Secrets**, with type **Secret**. It must start with `https://`.
3. **Optionally set `ALERT_MINUTES`**: a whole number of minutes from 2 to 1440. Set it in `wrangler.jsonc` and run `bunx wrangler deploy`, or in the Cloudflare dashboard.

The dashboard then shows the webhook's host and after how many minutes the Worker posts. It never shows the rest of the address.

To check the webhook before an outage does, send it what the Worker sends:

```sh
curl -H 'content-type: application/json' -H 'x-template: yes' -H 'x-message: {{.text}}' \
  -d '{"text":"boxrunner: test","content":"boxrunner: test"}' "<your webhook>"
```

## The message

The Worker posts JSON with the message in two fields, plus two headers:

```http
POST <ALERT_WEBHOOK>
Content-Type: application/json
X-Template: yes
X-Message: {{.text}}

{"text": "boxrunner: box-1 reports again.", "content": "boxrunner: box-1 reports again."}
```

- **Slack** shows `text`, the field its [incoming webhooks](https://docs.slack.dev/messaging/sending-messages-using-incoming-webhooks/) require.
- **Discord** shows `content`, the message of its [Execute Webhook](https://docs.discord.com/developers/resources/webhook#execute-webhook) call, which allows up to 2,000 characters.
- **ntfy** publishes a topic's body as the message, and reads JSON fields only from a post to the server's root address. `X-Template: yes` turns on its [inline templating](https://docs.ntfy.sh/publish/#message-templating), and `X-Message: {{.text}}` takes the message from the body's `text`. Tested against ntfy 2.30: without the headers, the notification shows the raw JSON.

Not yet tested against Slack and Discord themselves: their documentation does not say what they do with a field they do not know, which each gets here (Slack gets `content`, Discord gets `text`). If one refuses the post, the dashboard shows its answer.

## When the webhook fails

A post that fails or takes more than 10 seconds is retried after 1, 2, 4, 8 minutes and so on, up to once an hour. The Worker logs `Could not send the down alert (attempt 1): hooks.slack.com answered 400: no_text` (see the Worker's logs in the Cloudflare dashboard) and the dashboard shows the reason in red. Neither ever shows more of the address than its host. A heartbeat never posts, so a slow or broken webhook never delays the box.

## What it does not see

An alert means every box stopped reporting. A box that reports but starts no runners, for example because Docker on it is broken, sends no alert, and neither does a job that waits for a class no box serves. The Worker does not see the queue: the agent reads it from GitHub and reports runners and finished jobs, not waiting ones.

## Turn it off

Delete the `ALERT_WEBHOOK` secret. An address that does not start with `https://`, or an unreadable `ALERT_MINUTES` such as `10m`, also turns alerts off, and the dashboard says why.

## What it costs

- **Cloudflare.** Alerts wake the Worker with fallback's Durable Object alarm, never a cron trigger. While the box reports, the alarm fires once every `ALERT_MINUTES`: 144 times a day at 10 minutes, each one request and one row written. While the box is down it fires once an hour. With both alerts and fallback off it never fires. Each alert is one outbound request.
- **The webhook.** One post when the box goes down and one when it is back, plus retries while the webhook fails.
