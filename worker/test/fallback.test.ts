import { runDurableObjectAlarm, runInDurableObject } from 'cloudflare:test';
import { env } from 'cloudflare:workers';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { FakeGitHub } from './fake-github';
import { agent, fakeGitHub, heartbeatBody, send, setUp, signIn, time, withEnv } from './harness';

afterEach(() => {
	vi.useRealTimers();
});

// Far in the future, so an alarm the Worker sets never fires on its own; each test fires it.
const day = '2036-10-14';
const at = (clock: string) => time(`${day}T${clock}Z`);
const iso = (clock: string) => `${day}T${clock}.000Z`;

async function ready(github: FakeGitHub) {
	at('10:00:00');
	const call = agent(await setUp(github));
	const box = env.BOX.getByName('box');
	const beat = async () => expect((await call('POST', '/v1/heartbeat', heartbeatBody())).status).toBe(200);
	return {
		call,
		beat,
		/** Beats every 30 seconds from `from` to `to`, as a running agent does. */
		beatEvery30s: async (from: string, to: string) => {
			for (let t = Date.parse(`${day}T${from}Z`); t <= Date.parse(`${day}T${to}Z`); t += 30_000) {
				vi.setSystemTime(t);
				await beat();
			}
		},
		fire: () => runDurableObjectAlarm(box),
		nextAlarm: async () => {
			const alarm = await runInDurableObject(box, (_, state) => state.storage.getAlarm());
			return alarm === null ? null : new Date(alarm).toISOString();
		},
		dashboard: async () => {
			const { cookie } = await signIn(github);
			return (await (await send('/dashboard.json', { headers: { cookie: cookie! } })).json()) as {
				fallback: unknown;
				alerts: unknown;
				waiting: { box: string; class: string; cause: string | null }[];
				html: string;
			};
		},
	};
}

const variables = (github: FakeGitHub) =>
	Object.fromEntries(github.repos.map((repo) => [repo.full_name, github.variables.get(repo.id)?.get('BOXRUNNER_DOWN') ?? null]));

const text = (html: string) => html.replace(/<[^>]+>/g, '').replace(/\s+/g, ' ');

const tokenRequests = (github: FakeGitHub) =>
	github.calls.filter(
		(call) => call.path.endsWith('/access_tokens') && (call.body as { permissions: Record<string, string> }).permissions.actions_variables === 'write',
	).length;

describe('fallback to GitHub-hosted runners', () => {
	it('is off unless FALLBACK_MINUTES is set: a heartbeat sets no alarm', async () => {
		const github = await fakeGitHub();
		const { beat, nextAlarm, dashboard } = await ready(github);
		await beat();
		expect(await nextAlarm()).toBe(null);
		expect(github.count('POST', /\/actions\/variables$/)).toBe(0);
		expect((await dashboard()).fallback).toEqual({ minutes: null, configProblem: null, down: false, since: null, secondsSince: null, written: false, problem: null });
	});

	it('sets BOXRUNNER_DOWN in every served repository once the box is silent, and deletes it once the box is back for two minutes', async () => {
		const github = await fakeGitHub();
		github.granted.actions_variables = 'write';
		github.variables.set(124, new Map([['BOXRUNNER_DOWN', 'set by hand']]));
		await withEnv({ FALLBACK_MINUTES: '5' }, async () => {
			const { beat, beatEvery30s, fire, nextAlarm, dashboard } = await ready(github);
			await beat();
			expect(await nextAlarm()).toBe(iso('10:05:00'));

			at('10:04:59');
			await fire();
			expect(variables(github)).toEqual({ 'octo/site': null, 'octo/docs': 'set by hand', 'octo/old': null, 'octo/open': null });
			expect(await nextAlarm()).toBe(iso('10:05:00'));

			at('10:05:00');
			await fire();
			expect(variables(github)).toEqual({ 'octo/site': iso('10:05:00'), 'octo/docs': iso('10:05:00'), 'octo/old': null, 'octo/open': null });
			expect(await nextAlarm()).toBe(iso('11:05:00'));
			at('10:06:00');
			const { fallback, html } = await dashboard();
			expect(fallback).toEqual({
				minutes: 5,
				configProblem: null,
				down: true,
				since: iso('10:05:00'),
				secondsSince: 60,
				written: true,
				problem: null,
			});
			expect(text(html)).toContain(
				'Fallback to GitHub’s runners The box is down, so workflows that read BOXRUNNER_DOWN run on GitHub’s runners. The Worker switched 1 minute ago.',
			);

			await beatEvery30s('10:20:00', '10:21:30');
			expect(await nextAlarm()).toBe(iso('11:05:00'));
			expect(variables(github)['octo/site']).toBe(iso('10:05:00'));

			await beatEvery30s('10:22:00', '10:22:00');
			expect(await nextAlarm()).toBe(iso('10:22:00'));
			await fire();
			expect(variables(github)).toEqual({ 'octo/site': null, 'octo/docs': null, 'octo/old': null, 'octo/open': null });
			expect(await nextAlarm()).toBe(iso('10:27:00'));
		});
	});

	it('keeps the jobs on GitHub’s runners while the box keeps going silent again', async () => {
		const github = await fakeGitHub();
		github.granted.actions_variables = 'write';
		await withEnv({ FALLBACK_MINUTES: '5' }, async () => {
			const { beat, beatEvery30s, fire, nextAlarm } = await ready(github);
			await beat();
			at('10:05:00');
			await fire();

			await beatEvery30s('10:20:00', '10:21:30');
			await beatEvery30s('10:23:01', '10:24:31');
			expect(await nextAlarm()).toBe(iso('11:05:00'));
			expect(variables(github)['octo/site']).toBe(iso('10:05:00'));

			await beatEvery30s('10:25:01', '10:25:01');
			expect(await nextAlarm()).toBe(iso('10:25:01'));
		});
	});

	it('without the Variables permission, logs why, keeps going and waits longer after each failure', async () => {
		const github = await fakeGitHub();
		const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
		await withEnv({ FALLBACK_MINUTES: '5' }, async () => {
			const { beat, fire, nextAlarm, dashboard } = await ready(github);
			await beat();
			at('10:05:00');
			await fire();
			expect(variables(github)['octo/site']).toBe(null);
			expect(tokenRequests(github)).toBe(2);
			expect(await nextAlarm()).toBe(iso('10:06:00'));
			const problem = 'the App lacks the Variables permission (read and write), or an installation has not approved it';
			expect(warn).toHaveBeenLastCalledWith(`Could not set BOXRUNNER_DOWN (attempt 1): ${problem}`);
			const { fallback, html } = await dashboard();
			expect(fallback).toMatchObject({ down: true, written: false, problem });
			expect(text(html)).toContain(`The Worker could not set BOXRUNNER_DOWN: ${problem}. It tries again.`);

			at('10:06:00');
			await fire();
			expect(await nextAlarm()).toBe(iso('10:08:00'));
			at('10:08:00');
			await fire();
			expect(await nextAlarm()).toBe(iso('10:12:00'));
			expect(tokenRequests(github)).toBe(6);

			github.granted.actions_variables = 'write';
			at('10:12:00');
			await fire();
			expect(variables(github)['octo/site']).toBe(iso('10:05:00'));
			expect(await nextAlarm()).toBe(iso('11:12:00'));
			expect((await dashboard()).fallback).toMatchObject({ down: true, written: true, problem: null });
		});
	});

	it('deletes the variable once fallback is turned off, even while the box stays silent', async () => {
		const github = await fakeGitHub();
		github.granted.actions_variables = 'write';
		const { beat, fire, nextAlarm, dashboard } = await ready(github);
		await withEnv({ FALLBACK_MINUTES: '5' }, async () => {
			await beat();
			at('10:05:00');
			await fire();
			expect(variables(github)['octo/site']).toBe(iso('10:05:00'));
		});
		await withEnv({ FALLBACK_MINUTES: '5m' }, async () => {
			at('11:05:00');
			await fire();
			expect(variables(github)).toEqual({ 'octo/site': null, 'octo/docs': null, 'octo/old': null, 'octo/open': null });
			expect(await nextAlarm()).toBe(null);
			expect((await dashboard()).fallback).toMatchObject({
				minutes: null,
				configProblem: 'FALLBACK_MINUTES is "5m", not a whole number of minutes from 2 to 1440',
				down: false,
				written: false,
			});
		});
	});
});

const webhook = 'https://hooks.test/services/T000/B000/XXXX';

/** Answers posts to `webhook` with the next of `answers`, 200 once they run out, and records each; everything else goes to GitHub. */
function fakeWebhook(github: FakeGitHub) {
	const hook = { posts: [] as { headers: Record<string, string | null>; body: unknown }[], answers: [] as (number | Error)[] };
	vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
		const request = new Request(input, init);
		if (request.url !== webhook) return github.fetch(request);
		const headers = Object.fromEntries(['content-type', 'x-template', 'x-message'].map((name) => [name, request.headers.get(name)]));
		hook.posts.push({ headers, body: await request.json() });
		const answer = hook.answers.shift() ?? 200;
		if (answer instanceof Error) throw answer;
		return new Response(answer === 200 ? 'ok' : 'no_text\n', { status: answer });
	});
	return hook;
}

const said = (hook: ReturnType<typeof fakeWebhook>) => hook.posts.map((post) => (post.body as { text: string }).text);

describe('alerts', () => {
	it('posts once when the box goes silent for 10 minutes and once when it is back, in a body ntfy, Discord and Slack read', async () => {
		const github = await fakeGitHub();
		const hook = fakeWebhook(github);
		await withEnv({ ALERT_WEBHOOK: webhook }, async () => {
			const { beat, beatEvery30s, fire, nextAlarm, dashboard } = await ready(github);
			await beat();
			expect(await nextAlarm()).toBe(iso('10:10:00'));

			at('10:10:00');
			await fire();
			const down = 'boxrunner: no box has reported since 2036-10-14 10:00 UTC. Queued jobs wait for it.';
			expect(hook.posts).toEqual([
				{ headers: { 'content-type': 'application/json', 'x-template': 'yes', 'x-message': '{{.text}}' }, body: { text: down, content: down } },
			]);
			expect(github.count('POST', /\/actions\/variables$/)).toBe(0);
			expect(await nextAlarm()).toBe(iso('11:10:00'));
			at('11:10:00');
			await fire();
			expect(said(hook)).toEqual([down]);

			const { alerts, html } = await dashboard();
			expect(alerts).toEqual({ host: 'hooks.test', minutes: 10, configProblem: null, sentDown: true, problem: null });
			expect(text(html)).toContain('Alerts After 10 minutes without a heartbeat, the Worker posts to hooks.test, and again once the box is back.');
			expect(html).not.toContain('B000');

			await beatEvery30s('11:20:00', '11:22:00');
			expect(await nextAlarm()).toBe(iso('11:22:00'));
			await fire();
			expect(said(hook)).toEqual([down, 'boxrunner: box-1 reports again.']);
			expect(await nextAlarm()).toBe(iso('11:32:00'));
		});
	});

	it('waits ALERT_MINUTES without fallback, and FALLBACK_MINUTES with it', async () => {
		const github = await fakeGitHub();
		github.granted.actions_variables = 'write';
		const hook = fakeWebhook(github);
		const { beat, fire, nextAlarm, dashboard } = await ready(github);
		await withEnv({ ALERT_WEBHOOK: webhook, ALERT_MINUTES: '3' }, async () => {
			await beat();
			expect(await nextAlarm()).toBe(iso('10:03:00'));
			expect((await dashboard()).alerts).toMatchObject({ minutes: 3 });
		});
		await withEnv({ ALERT_WEBHOOK: webhook, ALERT_MINUTES: '3', FALLBACK_MINUTES: '5' }, async () => {
			at('10:03:00');
			await fire();
			expect(said(hook)).toEqual([]);
			expect(await nextAlarm()).toBe(iso('10:05:00'));
			at('10:05:00');
			await fire();
			expect(variables(github)['octo/site']).toBe(iso('10:05:00'));
			expect(said(hook)).toEqual(['boxrunner: no box has reported since 2036-10-14 10:00 UTC. Workflows that read BOXRUNNER_DOWN run on GitHub’s runners.']);
			expect((await dashboard()).alerts).toMatchObject({ minutes: 5, sentDown: true });
		});
	});

	it('retries a failed post after 1 minute, then 2, and shows why on the dashboard', async () => {
		const github = await fakeGitHub();
		const hook = fakeWebhook(github);
		hook.answers.push(400, new Error(`fetch ${webhook} failed: Network connection lost.`));
		const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
		await withEnv({ ALERT_WEBHOOK: webhook }, async () => {
			const { beat, fire, nextAlarm, dashboard } = await ready(github);
			await beat();
			at('10:10:00');
			await fire();
			expect(warn).toHaveBeenLastCalledWith('Could not send the down alert (attempt 1): hooks.test answered 400: no_text');
			expect(await nextAlarm()).toBe(iso('10:11:00'));
			const { alerts, html } = await dashboard();
			expect(alerts).toMatchObject({ sentDown: false, problem: 'hooks.test answered 400: no_text' });
			expect(text(html)).toContain('The Worker could not post the down alert: hooks.test answered 400: no_text. It tries again.');

			at('10:11:00');
			await fire();
			expect(warn).toHaveBeenLastCalledWith('Could not send the down alert (attempt 2): hooks.test could not be reached: fetch <webhook> failed: Network connection lost.');
			expect(await nextAlarm()).toBe(iso('10:13:00'));

			at('10:13:00');
			await fire();
			expect(hook.posts.length).toBe(3);
			expect(await nextAlarm()).toBe(iso('11:13:00'));
			expect((await dashboard()).alerts).toMatchObject({ sentDown: true, problem: null });
		});
	});

	it('sends nothing for an outage that ended before its alert got through', async () => {
		const github = await fakeGitHub();
		const hook = fakeWebhook(github);
		hook.answers.push(500);
		vi.spyOn(console, 'warn').mockImplementation(() => {});
		await withEnv({ ALERT_WEBHOOK: webhook }, async () => {
			const { beat, beatEvery30s, fire, nextAlarm, dashboard } = await ready(github);
			await beat();
			at('10:10:00');
			await fire();
			await beatEvery30s('10:10:30', '10:12:30');
			expect(await nextAlarm()).toBe(iso('10:12:30'));
			await fire();
			expect(hook.posts.length).toBe(1);
			expect(await nextAlarm()).toBe(iso('10:22:30'));
			expect((await dashboard()).alerts).toMatchObject({ sentDown: false, problem: null });
		});
	});

	it('reads the state a Worker from before alerts stored, and alerts for an outage it was already in', async () => {
		const github = await fakeGitHub();
		github.granted.actions_variables = 'write';
		const hook = fakeWebhook(github);
		const { beat, fire } = await ready(github);
		await beat();
		const before = { down: true, since: Date.parse(iso('10:05:00')), backSince: null, written: true, failures: 0, problem: null };
		await runInDurableObject(env.BOX.getByName('box'), async (_, state) => {
			state.storage.sql.exec('INSERT OR REPLACE INTO fallback (singleton, state) VALUES (1, ?)', JSON.stringify(before));
			await state.storage.setAlarm(Date.parse(iso('11:05:00')));
		});
		await withEnv({ ALERT_WEBHOOK: webhook, FALLBACK_MINUTES: '5' }, async () => {
			at('11:05:00');
			await fire();
			expect(said(hook)).toEqual(['boxrunner: no box has reported since 2036-10-14 10:00 UTC. Workflows that read BOXRUNNER_DOWN run on GitHub’s runners.']);
		});
	});

	it('stays off with a webhook that is not https, and the dashboard says why without the address', async () => {
		const github = await fakeGitHub();
		const hook = fakeWebhook(github);
		const { beat, nextAlarm, dashboard } = await ready(github);
		await withEnv({ ALERT_WEBHOOK: webhook.replace('https:', 'http:') }, async () => {
			await beat();
			expect(await nextAlarm()).toBe(null);
			const { alerts, html } = await dashboard();
			expect(alerts).toEqual({ host: null, minutes: null, configProblem: 'ALERT_WEBHOOK is not an https:// address', sentDown: false, problem: null });
			expect(text(html)).toContain('The Worker’s ALERT_WEBHOOK is not an https:// address, so alerts are off.');
			expect(html).not.toContain('B000');
		});
		await withEnv({ ALERT_WEBHOOK: webhook, ALERT_MINUTES: '10m' }, async () => {
			expect((await dashboard()).alerts).toMatchObject({ configProblem: 'ALERT_MINUTES is "10m", not a whole number of minutes from 2 to 1440' });
		});
		expect(hook.posts).toEqual([]);
	});
});

describe('alerts for waiting jobs', () => {
	const site = { repo: 'octo/site', class: 'boxrunner-light', assigned: 2 };
	const stillWaiting = 'boxrunner: octo/site has had jobs for class boxrunner-light waiting since 2036-10-14 10:00 UTC with no runner starting.';

	it('posts once when a repository’s jobs have waited ALERT_MINUTES with no runner starting, and once when one starts', async () => {
		const github = await fakeGitHub();
		const hook = fakeWebhook(github);
		await withEnv({ ALERT_WEBHOOK: webhook }, async () => {
			const { call, fire, nextAlarm, dashboard } = await ready(github);
			const beats = async (from: string, to: string, body: Record<string, unknown>) => {
				for (let t = Date.parse(iso(from)); t <= Date.parse(iso(to)); t += 30_000) {
					vi.setSystemTime(t);
					expect((await call('POST', '/v1/heartbeat', heartbeatBody(body))).status).toBe(200);
				}
			};
			await beats('10:00:00', '10:09:30', { scaleSets: [site] });
			expect(await nextAlarm()).toBe(iso('10:10:00'));
			at('10:05:00');
			expect((await dashboard()).waiting).toEqual([
				{ repo: 'octo/site', class: 'boxrunner-light', box: 'box-1', since: iso('10:00:00'), seconds: 300, overdue: false, cause: null },
			]);

			await beats('10:10:00', '10:10:00', { scaleSets: [site] });
			await fire();
			expect(said(hook)).toEqual([stillWaiting]);
			const { waiting, html } = await dashboard();
			expect(waiting).toMatchObject([{ repo: 'octo/site', overdue: true }]);
			expect(text(html)).toContain('Jobs waiting for a runner RepositoryClassBoxWaitingLikely cause octo/siteboxrunner-lightbox-110 min 0 sUnknown');

			await beats('10:10:30', '10:12:00', { scaleSets: [site] });
			await fire();
			expect(said(hook)).toEqual([stillWaiting]);

			const starting = { name: 'box-1-boxrunner-light-4d5e6f', repo: 'octo/site', class: 'boxrunner-light', state: 'starting', since: iso('10:12:30') };
			await beats('10:12:30', '10:12:30', { scaleSets: [site], runners: [starting] });
			expect(await nextAlarm()).toBe(iso('10:12:30'));
			await fire();
			expect(said(hook)).toEqual([stillWaiting, 'boxrunner: octo/site no longer has jobs for class boxrunner-light waiting without a runner.']);
			expect((await dashboard()).waiting).toEqual([]);
		});
	});

	it('names the likely cause: a paused class, a draining or silent box, a class held back or at its maximum', async () => {
		const github = await fakeGitHub();
		const hook = fakeWebhook(github);
		await withEnv({ ALERT_WEBHOOK: webhook }, async () => {
			const { call, fire, dashboard } = await ready(github);
			const box1 = (scaleSets: unknown[]) =>
				heartbeatBody({
					classes: [
						{ name: 'boxrunner', priority: 'normal', max: 1 },
						{ name: 'boxrunner-light', priority: 'idle', max: 2, heldUntil: iso('10:00:30') },
					],
					runners: [{ name: 'box-1-boxrunner-3f9a2c', repo: 'octo/docs', class: 'boxrunner', state: 'running', since: iso('09:55:00') }],
					scaleSets,
				});
			const box2 = (scaleSets: unknown[]) =>
				heartbeatBody({ box: 'box-2', classes: [{ name: 'boxrunner-light', priority: 'idle', max: 2 }], runners: [], draining: true, scaleSets });
			const box1Waits = [
				{ repo: 'octo/site', class: 'boxrunner', assigned: 1 },
				{ repo: 'octo/site', class: 'boxrunner-light', assigned: 3 },
			];
			const post = async (body: unknown) => expect((await call('POST', '/v1/heartbeat', body)).status).toBe(200);

			at('10:00:00');
			await post(box1([...box1Waits, { repo: 'octo/docs', class: 'boxrunner-gpu', assigned: 1 }]));
			await post(box2([{ repo: 'octo/docs', class: 'boxrunner-light', assigned: 1 }]));
			await post(heartbeatBody({ box: 'box-3', runners: [], scaleSets: [{ repo: 'octo/site', class: 'boxrunner-arm', assigned: 4 }] }));
			await runInDurableObject(env.BOX.getByName('box'), async (_, state) => {
				state.storage.sql.exec("INSERT INTO paused_classes (class, since, by_login) VALUES ('boxrunner-gpu', ?, 'octo')", Date.now());
			});
			for (let t = Date.parse(iso('10:00:30')); t <= Date.parse(iso('10:10:00')); t += 30_000) {
				vi.setSystemTime(t);
				await post(box1(box1Waits));
				await post(box2([]));
			}
			await fire();
			const since = 'waiting since 2036-10-14 10:00 UTC with no runner starting. Likely cause:';
			expect(said(hook)).toEqual([
				[
					`boxrunner: octo/docs has had jobs for class boxrunner-gpu ${since} the class is paused on the dashboard.`,
					`boxrunner: octo/docs has had jobs for class boxrunner-light ${since} box-2 is draining.`,
					`boxrunner: octo/site has had jobs for class boxrunner ${since} box-1 already runs the class’s maximum of 1 runner for other repositories.`,
					`boxrunner: octo/site has had jobs for class boxrunner-arm ${since} box-3 stopped reporting.`,
					`boxrunner: octo/site has had jobs for class boxrunner-light ${since} box-1 holds the class back because its runners failed before taking a job.`,
				].join('\n'),
			]);
			expect((await dashboard()).waiting.map((wait) => `${wait.box} ${wait.class}: ${wait.cause}`)).toEqual([
				'box-1 boxrunner-gpu: the class is paused on the dashboard',
				'box-2 boxrunner-light: box-2 is draining',
				'box-1 boxrunner: box-1 already runs the class’s maximum of 1 runner for other repositories',
				'box-3 boxrunner-arm: box-3 stopped reporting',
				'box-1 boxrunner-light: box-1 holds the class back because its runners failed before taking a job',
			]);

			at('10:10:30');
			await post(box2([]));
			await post(heartbeatBody({ box: 'box-2', classes: [{ name: 'boxrunner-light', priority: 'idle', max: 2 }], runners: [], scaleSets: [] }));
			await fire();
			expect(said(hook).at(-1)).toBe('boxrunner: octo/docs no longer has jobs for class boxrunner-light waiting without a runner.');
		});
	});

	it('reads the box statuses a Worker from before waits stored', async () => {
		const github = await fakeGitHub();
		const { call, dashboard } = await ready(github);
		const before = { box: 'box-0', version: '0.3.0', latest: null, image: 'not built', disk: { free: 1, floor: 0 }, classes: [{ name: 'boxrunner', priority: 'normal', max: 1 }], runners: [] };
		await runInDurableObject(env.BOX.getByName('box'), async (_, state) => {
			state.storage.sql.exec('INSERT INTO boxes (name, seen_at, status) VALUES (?, ?, ?)', 'box-0', Date.now(), JSON.stringify(before));
		});
		expect((await call('POST', '/v1/heartbeat', heartbeatBody({ scaleSets: [site] }))).status).toBe(200);
		expect((await dashboard()).waiting).toMatchObject([{ repo: 'octo/site', box: 'box-1', overdue: false }]);
	});

	it('retries a failed post like the down alert, and sends nothing for jobs that stopped waiting before it got through', async () => {
		const github = await fakeGitHub();
		const hook = fakeWebhook(github);
		hook.answers.push(500, 500);
		const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
		await withEnv({ ALERT_WEBHOOK: webhook, ALERT_MINUTES: '2' }, async () => {
			const { call, fire, nextAlarm, dashboard } = await ready(github);
			const beat = async (clock: string, scaleSets: unknown[]) => {
				at(clock);
				expect((await call('POST', '/v1/heartbeat', heartbeatBody({ scaleSets }))).status).toBe(200);
			};
			await beat('10:00:00', [site]);
			await beat('10:02:00', [site]);
			await fire();
			expect(warn).toHaveBeenLastCalledWith('Could not send the waiting jobs alert (attempt 1): hooks.test answered 500: no_text');
			expect(await nextAlarm()).toBe(iso('10:03:00'));
			expect(text((await dashboard()).html)).toContain('The Worker could not post the waiting jobs alert: hooks.test answered 500: no_text. It tries again.');

			at('10:03:00');
			await fire();
			expect(await nextAlarm()).toBe(iso('10:05:00'));
			await beat('10:03:30', []);
			expect(await nextAlarm()).toBe(iso('10:05:00'));
			at('10:05:00');
			await fire();
			expect(hook.posts.length).toBe(2);
			expect((await dashboard()).alerts).toMatchObject({ problem: null });
		});
	});
});
