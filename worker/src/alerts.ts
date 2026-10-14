import { variable } from './fallback';

const timeoutMs = 10_000;

const minuteUtc = (time: number) => `${new Date(time).toISOString().slice(0, 16).replace('T', ' ')} UTC`;

/** What the owner reads. `lastSeen` and `back` come from the boxes' heartbeats. */
export function alertText(down: boolean, fallback: boolean, lastSeen: number | undefined, back: string | undefined): string {
	if (!down) return `boxrunner: ${back ?? 'the box'} reports again.${fallback ? ' Jobs go to the box again.' : ''}`;
	const since = lastSeen === undefined ? '' : ` since ${minuteUtc(lastSeen)}`;
	const jobs = fallback ? `Workflows that read ${variable} run on GitHub’s runners.` : 'Queued jobs wait for it.';
	return `boxrunner: no box has reported${since}. ${jobs}`;
}

/**
 * Posts `text` to the webhook as `text` for Slack and `content` for Discord.
 * ntfy reads a topic's body as the message, so the headers ask it to take
 * the message from `text` instead; Slack and Discord ignore them. Answers why
 * the post failed, or null. The answer never holds the address.
 */
/** A fetch error's message may quote the URL, whose path is the secret for Discord and Slack. */
function redact(message: string, webhook: URL): string {
	return message.replaceAll(webhook.href, '<webhook>').replaceAll(webhook.pathname, '<path>');
}

export async function postAlert(webhook: URL, text: string): Promise<string | null> {
	let response: Response;
	try {
		response = await fetch(webhook, {
			method: 'POST',
			headers: { 'content-type': 'application/json', 'x-template': 'yes', 'x-message': '{{.text}}' },
			body: JSON.stringify({ text, content: text }),
			signal: AbortSignal.timeout(timeoutMs),
		});
	} catch (error) {
		if (error instanceof Error && error.name === 'TimeoutError') return `${webhook.host} did not answer within ${timeoutMs / 1000} seconds`;
		return `${webhook.host} could not be reached: ${redact(error instanceof Error ? error.message : String(error), webhook)}`;
	}
	if (response.ok) {
		await response.body?.cancel();
		return null;
	}
	const said = (await response.text()).replace(/\s+/g, ' ').trim().slice(0, 200);
	return `${webhook.host} answered ${response.status}${said ? `: ${said}` : ''}`;
}
