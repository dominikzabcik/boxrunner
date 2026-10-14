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
			return (await (await send('/dashboard.json', { headers: { cookie: cookie! } })).json()) as { fallback: unknown; html: string };
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
