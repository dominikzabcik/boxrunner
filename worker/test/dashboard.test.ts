import { afterEach, describe, expect, it, vi } from 'vitest';
import { agent, expectError, fakeGitHub, heartbeatBody, job, send, setUp, signIn, time } from './harness';

afterEach(() => {
	vi.useRealTimers();
});

/** A box that reported at 09:59:30 with one finished job, viewed at 10:00:00. */
async function ready() {
	time('2026-10-14T09:59:30Z');
	const github = await fakeGitHub();
	const call = agent(await setUp(github));
	const { cookie } = await signIn(github);
	const finished = job('box-1-boxrunner-aaaaaa', '2026-10-14T09:50:00Z', '2026-10-14T09:50:20Z', '2026-10-14T09:53:05Z');
	expect((await call('POST', '/v1/heartbeat', heartbeatBody({ jobs: [finished] }))).status).toBe(200);
	time('2026-10-14T10:00:00Z');
	const view = (path = '/') => send(path, { headers: { cookie: cookie! } });
	return { github, call, view };
}

async function sha256(text: string) {
	const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text)));
	return btoa(String.fromCharCode(...digest));
}

describe('GET / signed in', () => {
	it('shows the box, its classes and runners, recent jobs and minutes not billed', async () => {
		const { view } = await ready();
		const html = await (await view()).text();
		for (const fragment of [
			'<span>Signed in as octo</span>',
			'<h2 id="box-0">box-1</h2>\n<p>Online, last seen <time datetime="2026-10-14T09:59:30.000Z">30 seconds ago</time></p>',
			'<dt>Agent</dt><dd>0.2.0</dd>',
			'<dt>Image</dt><dd><code>2.338.0+3f9a2c0b1d4e</code></dd>',
			'<dt>Disk</dt><dd>26.0 GB free, floor 10.0 GB</dd>',
			'<tr><td>boxrunner</td><td>normal</td><td class="number">1</td><td class="number">0</td><td class="number">1</td></tr>',
			'<tr><td>boxrunner-light</td><td>idle</td><td class="number">0</td><td class="number">1</td><td class="number">2</td></tr>',
			'<tr><td>box-1-boxrunner-3f9a2c</td><td>octo/site</td><td>boxrunner</td><td>running</td><td class="number">4 min 30 s</td></tr>',
			'<tr><td>octo/site</td><td>boxrunner</td><td>test</td><td>succeeded</td><td class="number">20 s</td><td class="number">2 min 45 s</td><td><time datetime="2026-10-14T09:53:05.000Z">6 minutes ago</time></td></tr>',
			'<tr><th scope="row">Today (UTC)</th><td class="number">1</td><td class="number">3</td><td class="number">$0.02</td></tr>',
			'<p>The box has not asked for its repositories yet.</p>',
			'<tr><td>boxrunner</td><td>Serving</td><td><form method="post" action="/classes/pause">',
			'<input type="hidden" name="class" value="boxrunner-light">\n<button type="submit" aria-label="Pause boxrunner-light">Pause</button>',
			'<form method="post" action="/box-token">',
		]) {
			expect(html).toContain(fragment);
		}
	});

	it('says the box is offline, and when it was last seen, after two minutes without a heartbeat', async () => {
		const { view } = await ready();
		time('2026-10-14T10:01:30Z');
		expect(await (await view()).text()).toContain('<p>Online, last seen <time datetime="2026-10-14T09:59:30.000Z">2 minutes ago</time></p>');
		time('2026-10-14T10:04:31Z');
		expect(await (await view()).text()).toContain('<p class="alert">Offline, last seen <time datetime="2026-10-14T09:59:30.000Z">5 minutes ago</time></p>');
	});

	it('flags free disk below the floor', async () => {
		const { call, view } = await ready();
		await call('POST', '/v1/heartbeat', heartbeatBody({ disk: { free: 4_000_000_000, floor: 10_000_000_000 } }));
		expect(await (await view()).text()).toContain('<dt>Disk</dt><dd class="alert">4.0 GB free, floor 10.0 GB, so no runner starts</dd>');
	});

	it('allows exactly its own inline style and script, fetches only from its own origin, and is never framed or indexed', async () => {
		const { view } = await ready();
		const response = await view();
		const html = await response.text();
		const style = /<style>([\s\S]*?)<\/style>/.exec(html)![1]!;
		const script = /<script>([\s\S]*?)<\/script>/.exec(html)![1]!;
		expect(script).toContain("fetch('/dashboard.json', { cache: 'no-store' })");
		expect(response.headers.get('content-security-policy')).toBe(
			`default-src 'none'; style-src 'sha256-${await sha256(style)}'; script-src 'sha256-${await sha256(script)}'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'`,
		);
		expect(response.headers.get('x-frame-options')).toBe('DENY');
		expect(response.headers.get('cache-control')).toBe('no-store');
		expect(html).toContain('<meta name="robots" content="noindex">');
	});

	it('never calls GitHub', async () => {
		const { github, view } = await ready();
		const before = github.calls.length;
		await view();
		await view('/dashboard.json');
		expect(github.calls.length).toBe(before);
	});

	it('lists the repositories as the agent last fetched them', async () => {
		const { call, view } = await ready();
		await call('GET', '/v1/repos');
		time('2026-10-14T10:30:00Z');
		expect(await (await view()).text()).toContain('<ul>\n<li>octo/site</li>\n<li>octo/docs</li>\n</ul>');
		expect(((await (await view('/dashboard.json')).json()) as { repos: string[] }).repos).toEqual(['octo/site', 'octo/docs']);
	});
});

describe('GET /dashboard.json', () => {
	it('answers 401 without a session', async () => {
		await ready();
		await expectError(await send('/dashboard.json'), 401, 'unauthorized', 'Sign in to the dashboard first.');
	});

	it("holds the page's data and the same part of the page rendered", async () => {
		const { view } = await ready();
		const { html, ...data } = (await (await view('/dashboard.json')).json()) as Record<string, unknown> & { html: string };
		expect(data).toEqual({
			generatedAt: '2026-10-14T10:00:00.000Z',
			boxes: [
				{
					name: 'box-1',
					version: '0.2.0',
					image: '2.338.0+3f9a2c0b1d4e',
					lastSeen: '2026-10-14T09:59:30.000Z',
					secondsSinceSeen: 30,
					online: true,
					disk: { free: 26_000_000_000, floor: 10_000_000_000 },
					classes: [
						{ name: 'boxrunner', priority: 'normal', max: 1, running: 1, starting: 0, paused: false },
						{ name: 'boxrunner-light', priority: 'idle', max: 2, running: 0, starting: 1, paused: false },
					],
					runners: [
						{ name: 'box-1-boxrunner-3f9a2c', repo: 'octo/site', class: 'boxrunner', state: 'running', since: '2026-10-14T09:55:00.000Z', seconds: 270 },
						{
							name: 'box-1-boxrunner-light-1b2c3d',
							repo: 'octo/docs',
							class: 'boxrunner-light',
							state: 'starting',
							since: '2026-10-14T09:59:30.000Z',
							seconds: 0,
						},
					],
				},
			],
			jobs: [
				{
					runner: 'box-1-boxrunner-aaaaaa',
					repo: 'octo/site',
					class: 'boxrunner',
					name: 'test',
					result: 'succeeded',
					queued: '2026-10-14T09:50:00.000Z',
					started: '2026-10-14T09:50:20.000Z',
					finished: '2026-10-14T09:53:05.000Z',
					secondsSinceFinished: 415,
					waitSeconds: 20,
					durationSeconds: 165,
					minutes: 3,
				},
			],
			totals: { today: { jobs: 1, minutes: 3, dollars: 0.02 }, last30Days: { jobs: 1, minutes: 3, dollars: 0.02 } },
			dollarsPerMinute: 0.006,
			repos: null,
			classes: ['boxrunner', 'boxrunner-light'],
			classesProblem: null,
			paused: [],
			fallback: { minutes: null, configProblem: null, down: false, since: null, secondsSince: null, written: false, problem: null },
		});
		const page = await (await view()).text();
		expect(/<div id="live">\n([\s\S]*)\n<\/div>/.exec(page)?.[1]).toBe(html);
	});
});
