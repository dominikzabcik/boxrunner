import { afterEach, describe, expect, it, vi } from 'vitest';
import { agent, expectError, fakeGitHub, heartbeatBody, postForm, send, setUp, signIn, time, tokenIn } from './harness';
import { setupSecret } from './secret';

afterEach(() => {
	vi.useRealTimers();
});

async function ready() {
	time('2026-10-14T10:00:00Z');
	const github = await fakeGitHub();
	const token = await setUp(github);
	const { cookie } = await signIn(github);
	const view = async (path = '/') => send(path, { headers: { cookie: cookie! } });
	const csrf = /name="csrf" value="([^"]+)"/.exec(await (await view()).text())![1]!;
	const post = (path: string, fields: Record<string, string> = {}) => postForm(path, { csrf, ...fields }, { cookie: cookie! });
	return { github, token, cookie: cookie!, csrf, view, post };
}

async function served(token: string) {
	const { classes, paused } = (await (await agent(token)('GET', '/v1/repos')).json()) as { classes: string[]; paused: string[] };
	return { classes, paused };
}

describe('POST /classes/pause and /classes/resume', () => {
	it('pause a class for every box until it is resumed, and the dashboard says who paused it', async () => {
		const { token, view, post } = await ready();
		const paused = await post('/classes/pause', { class: 'boxrunner-light' });
		expect(paused.status).toBe(303);
		expect(paused.headers.get('location')).toBe('/#classes');

		expect(await served(token)).toEqual({ classes: ['boxrunner'], paused: ['boxrunner-light'] });
		expect(await (await agent(token)('POST', '/v1/heartbeat', heartbeatBody())).json()).toEqual({ paused: ['boxrunner-light'] });

		time('2026-10-14T10:05:00Z');
		await post('/classes/pause', { class: 'boxrunner-light' });
		const html = await (await view()).text();
		expect(html).toContain('<tr><td>boxrunner-light (paused)</td><td>idle</td>');
		expect(html).toContain(
			'<tr><td>boxrunner-light</td><td>Paused by octo<br><time datetime="2026-10-14T10:00:00.000Z">5 minutes ago</time></td><td><form method="post" action="/classes/resume">',
		);
		expect(html).toContain('<button type="submit" aria-label="Resume boxrunner-light">Resume</button>');
		expect(((await (await view('/dashboard.json')).json()) as { paused: unknown }).paused).toEqual([
			{ class: 'boxrunner-light', by: 'octo', since: '2026-10-14T10:00:00.000Z', seconds: 300 },
		]);

		const resumed = await post('/classes/resume', { class: 'boxrunner-light' });
		expect(resumed.status).toBe(303);
		expect(await served(token)).toEqual({ classes: ['boxrunner', 'boxrunner-light'], paused: [] });
		expect(await (await agent(token)('POST', '/v1/heartbeat', heartbeatBody())).json()).toEqual({ paused: [] });
	});

	it('change nothing without a session, without the CSRF token or for a class the Worker does not serve', async () => {
		const { token, cookie, csrf, post } = await ready();
		const signedOut = await postForm('/classes/pause', { csrf, class: 'boxrunner' });
		expect(signedOut.status).toBe(401);
		expect(await signedOut.text()).toContain('<h1>Sign in first</h1>');
		for (const fields of [{ class: 'boxrunner' }, { class: 'boxrunner', csrf: 'forged' }]) {
			const forged = await postForm('/classes/pause', fields, { cookie });
			expect(forged.status).toBe(403);
			expect(await forged.text()).toContain('<h1>This action did not come from your dashboard</h1>');
		}
		for (const name of ['gpu', 'Not A Class', '']) {
			const unknown = await post('/classes/pause', { class: name });
			expect(unknown.status).toBe(400);
			expect(await unknown.text()).toContain('<h1>No such class</h1>');
		}
		expect(await served(token)).toEqual({ classes: ['boxrunner', 'boxrunner-light'], paused: [] });
	});

	it('are forgotten when setup starts over', async () => {
		const { github, post } = await ready();
		await post('/classes/pause', { class: 'boxrunner' });
		await postForm('/setup/reset', { secret: setupSecret });
		expect(await served(await setUp(github))).toEqual({ classes: ['boxrunner', 'boxrunner-light'], paused: [] });
	});
});

describe('POST /box-token', () => {
	it('shows a new token once with the command for the box, and the old token stops working', async () => {
		const { token, post } = await ready();
		const response = await post('/box-token');
		const html = await response.text();
		const fresh = tokenIn(html);
		expect(response.status).toBe(200);
		expect(response.headers.get('cache-control')).toBe('no-store');
		expect(fresh).toMatch(/^[A-Za-z0-9_-]{43}$/);
		expect(html).toContain(`<pre><code>sudo boxrunner install --token ${fresh}</code></pre>`);
		await expectError(await agent(token)('GET', '/v1/repos'), 401, 'unauthorized');
		expect((await agent(fresh!)('GET', '/v1/repos')).status).toBe(200);
	});

	it('keeps the token without a session or without the CSRF token', async () => {
		const { token, cookie, csrf } = await ready();
		expect((await postForm('/box-token', { csrf })).status).toBe(401);
		expect((await postForm('/box-token', {}, { cookie })).status).toBe(403);
		expect((await postForm('/box-token', { csrf: 'forged' }, { cookie })).status).toBe(403);
		expect((await agent(token)('GET', '/v1/repos')).status).toBe(200);
	});
});
