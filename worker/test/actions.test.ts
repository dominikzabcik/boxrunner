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
		expect(await (await agent(token)('POST', '/v1/heartbeat', heartbeatBody())).json()).toEqual({ paused: ['boxrunner-light'], limits: {} });

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
		expect(await (await agent(token)('POST', '/v1/heartbeat', heartbeatBody())).json()).toEqual({ paused: [], limits: {} });
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

describe('POST /boxes/limit', () => {
	it('caps a class on one box until the field is left empty, and the dashboard says who set it', async () => {
		const { token, view, post } = await ready();
		const beat = async () => (await (await agent(token)('POST', '/v1/heartbeat', heartbeatBody())).json()) as { limits: Record<string, number> };
		await beat();
		const set = await post('/boxes/limit', { box: 'box-1', class: 'boxrunner-light', max: '0' });
		expect(set.status).toBe(303);
		expect(set.headers.get('location')).toBe('/#box-0');
		expect((await beat()).limits).toEqual({ 'boxrunner-light': 0 });
		expect(((await (await agent(token)('POST', '/v1/heartbeat', heartbeatBody({ box: 'box-2' }))).json()) as { limits: object }).limits).toEqual({});

		time('2026-10-14T10:05:00Z');
		const html = await (await view()).text();
		expect(html).toContain(
			'<td><p>0 by octo, <time datetime="2026-10-14T10:00:00.000Z">5 minutes ago</time></p><form method="post" action="/boxes/limit" class="inline">',
		);
		expect(html).toContain('<input type="number" name="max" min="0" max="2" value="0" aria-label="Limit for boxrunner-light on box-1">');
		const json = (await (await view('/dashboard.json')).json()) as { boxes: { classes: { name: string; limit: unknown }[] }[] };
		expect(json.boxes[0]!.classes.find((c) => c.name === 'boxrunner-light')!.limit).toEqual({ max: 0, by: 'octo', since: '2026-10-14T10:00:00.000Z', seconds: 300 });

		await post('/boxes/limit', { box: 'box-1', class: 'boxrunner-light', max: '' });
		expect((await beat()).limits).toEqual({});
	});

	it('changes nothing for a box or class nobody reported, or a limit outside 0 to the max', async () => {
		const { token, post } = await ready();
		await agent(token)('POST', '/v1/heartbeat', heartbeatBody());
		for (const fields of [
			{ box: 'box-9', class: 'boxrunner', max: '0' },
			{ box: 'box-1', class: 'gpu', max: '0' },
		]) {
			const unknown = await post('/boxes/limit', fields);
			expect(unknown.status).toBe(400);
			expect(await unknown.text()).toContain('<h1>No such class on that box</h1>');
		}
		for (const max of ['2', '-1', '0.5', 'one']) {
			const wrong = await post('/boxes/limit', { box: 'box-1', class: 'boxrunner', max });
			expect(wrong.status).toBe(400);
			expect(await wrong.text()).toContain('A limit is a whole number from 0 to 1');
		}
		expect(((await (await agent(token)('POST', '/v1/heartbeat', heartbeatBody())).json()) as { limits: object }).limits).toEqual({});
	});
});

describe('POST /box-token', () => {
	it('replaces the token and sends the browser to a page that shows the new one once', async () => {
		const { token, view, post } = await ready();
		const replaced = await post('/box-token');
		const location = replaced.headers.get('location')!;
		expect(replaced.status).toBe(303);
		expect(replaced.headers.get('cache-control')).toBe('no-store');
		expect(location).toMatch(/^\/box-token\/[A-Za-z0-9_-]{43}$/);
		await expectError(await agent(token)('GET', '/v1/repos'), 401, 'unauthorized');

		const shown = await view(location);
		const html = await shown.text();
		const fresh = tokenIn(html)!;
		expect(shown.status).toBe(200);
		expect(shown.headers.get('cache-control')).toBe('no-store');
		expect(html).toContain(`<pre><code>sudo boxrunner install --token ${fresh}</code></pre>`);
		expect((await agent(fresh)('GET', '/v1/repos')).status).toBe(200);

		const reloaded = await view(location);
		const again = await reloaded.text();
		expect(reloaded.status).toBe(410);
		expect(reloaded.headers.get('cache-control')).toBe('no-store');
		expect(tokenIn(again)).toBeUndefined();
		expect(again).toContain('<h1>This box token was already shown</h1>');
		expect(again).toContain('<p><a href="/#token">Replace it on the dashboard</a></p>');
		expect((await agent(fresh)('GET', '/v1/repos')).status).toBe(200);
	});

	it('shows the new token only within a minute', async () => {
		const { view, post } = await ready();
		const late = (await post('/box-token')).headers.get('location')!;
		time('2026-10-14T10:01:00Z');
		const expired = await view(late);
		expect(expired.status).toBe(410);
		expect(await expired.text()).toContain('<h1>This box token was already shown</h1>');

		const onTime = (await post('/box-token')).headers.get('location')!;
		time('2026-10-14T10:01:59Z');
		expect((await view(onTime)).status).toBe(200);
	});

	it('shows the new token only to a signed-in browser, at the address it was sent to', async () => {
		const { view, post } = await ready();
		const location = (await post('/box-token')).headers.get('location')!;
		const signedOut = await send(location);
		expect(signedOut.status).toBe(401);
		expect(await signedOut.text()).toContain('<h1>Sign in first</h1>');
		expect((await view(`/box-token/${'A'.repeat(43)}`)).status).toBe(410);
		expect((await view(location.replace('/box-token/', '/setup/token/'))).status).toBe(410);
		expect((await view(location)).status).toBe(200);
	});

	it('keeps the token without a session or without the CSRF token', async () => {
		const { token, cookie, csrf } = await ready();
		expect((await postForm('/box-token', { csrf })).status).toBe(401);
		expect((await postForm('/box-token', {}, { cookie })).status).toBe(403);
		expect((await postForm('/box-token', { csrf: 'forged' }, { cookie })).status).toBe(403);
		expect((await agent(token)('GET', '/v1/repos')).status).toBe(200);
	});
});
