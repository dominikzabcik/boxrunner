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
			return (await (await send('/dashboard.json', { headers: { cookie: cookie! } })).json()) as { fallback: unknown; alerts: unknown; html: string };
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
		hook.answers.push(400, new Error('Network connection lost.'));
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
			expect(warn).toHaveBeenLastCalledWith('Could not send the down alert (attempt 2): hooks.test could not be reached: Network connection lost.');
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
