import { afterEach, describe, expect, it, vi } from 'vitest';
import { agent, fakeGitHub, heartbeatBody, postForm, send, setUp, signIn, time } from './harness';

afterEach(() => {
	vi.useRealTimers();
});

async function ready() {
	time('2026-10-14T10:00:00Z');
	const github = await fakeGitHub();
	return agent(await setUp(github));
}

const runner = (box: string, n: number, state = 'running', repo = 'octo/site') => ({
	name: `${box}-boxrunner-${n}`,
	repo,
	class: 'boxrunner',
	state,
	since: '2026-10-14T09:55:00Z',
});

const box = (name: string, overrides: Record<string, unknown> = {}) =>
	heartbeatBody({ box: name, classes: [{ name: 'boxrunner', priority: 'normal', max: 2 }], runners: [], ...overrides });

async function beat(call: ReturnType<typeof agent>, body: Record<string, unknown>) {
	const response = await call('POST', '/v1/heartbeat', body);
	expect(response.status).toBe(200);
	return (await response.json()) as { paused: string[]; limits: Record<string, number>; fleet?: { repo: string; class: string; jobs: number; repos: string[] }[] };
}

describe('fleet shares', () => {
	it('sends no share while one box serves a class', async () => {
		const call = await ready();
		const reply = await beat(call, box('vm', { scaleSets: [{ repo: 'octo/site', class: 'boxrunner', assigned: 3 }] }));
		expect(reply).toEqual({ paused: [], limits: {} });
	});

	it('gives the jobs the session holder cannot run to the next box by rank', async () => {
		const call = await ready();
		await beat(call, box('mac-a', { rank: 10 }));
		await beat(call, box('mac-b', { rank: 20 }));
		const vm = await beat(
			call,
			box('vm', { rank: 0, runners: [runner('vm', 1), runner('vm', 2)], scaleSets: [{ repo: 'octo/site', class: 'boxrunner', assigned: 5 }] }),
		);
		expect(vm.fleet).toEqual([{ repo: 'octo/site', class: 'boxrunner', jobs: 2, repos: [] }]);
		expect((await beat(call, box('mac-a', { rank: 10 }))).fleet).toEqual([{ repo: 'octo/site', class: 'boxrunner', jobs: 2, repos: [] }]);
		expect((await beat(call, box('mac-b', { rank: 20 }))).fleet).toEqual([{ repo: 'octo/site', class: 'boxrunner', jobs: 1, repos: [] }]);
	});

	it('counts a box’s runners toward its share, so a job it took is not given out again', async () => {
		const call = await ready();
		await beat(call, box('vm', { runners: [runner('vm', 1), runner('vm', 2)], scaleSets: [{ repo: 'octo/site', class: 'boxrunner', assigned: 3 }] }));
		const mac = await beat(call, box('mac', { rank: 10, runners: [runner('mac', 1, 'starting')] }));
		expect(mac.fleet).toEqual([{ repo: 'octo/site', class: 'boxrunner', jobs: 1, repos: [] }]);
	});

	it('skips a box that drains, lacks disk, holds its starts or stopped reporting', async () => {
		const call = await ready();
		const blocked = {
			a: box('a', { rank: 1, draining: true }),
			b: box('b', { rank: 2, disk: { free: 1, floor: 10 } }),
			c: box('c', { rank: 3, classes: [{ name: 'boxrunner', priority: 'normal', max: 2, heldUntil: '2026-10-14T10:05:00Z' }] }),
		};
		for (const body of Object.values(blocked)) await beat(call, body);
		time('2026-10-14T09:57:00Z');
		await beat(call, box('d', { rank: 4 }));
		time('2026-10-14T10:00:00Z');
		await beat(call, box('e', { rank: 5 }));
		await beat(call, box('vm', { scaleSets: [{ repo: 'octo/site', class: 'boxrunner', assigned: 4 }], runners: [runner('vm', 1), runner('vm', 2)] }));
		for (const body of Object.values(blocked)) {
			expect((await beat(call, body)).fleet).toEqual([{ repo: 'octo/site', class: 'boxrunner', jobs: 0, repos: [] }]);
		}
		expect((await beat(call, box('e', { rank: 5 }))).fleet).toEqual([{ repo: 'octo/site', class: 'boxrunner', jobs: 2, repos: [] }]);
	});

	it('names the waiting jobs’ repositories for an organization scale set', async () => {
		const call = await ready();
		await beat(call, box('vm', { runners: [runner('vm', 1, 'running', 'octo'), runner('vm', 2, 'running', 'octo')], scaleSets: [{ repo: 'octo', class: 'boxrunner', assigned: 3, pending: ['octo/site'] }] }));
		expect((await beat(call, box('mac', { rank: 10 }))).fleet).toEqual([{ repo: 'octo', class: 'boxrunner', jobs: 1, repos: ['octo/site'] }]);
	});

	it('gives a box no more than the limit set for it on the dashboard', async () => {
		time('2026-10-14T10:00:00Z');
		const github = await fakeGitHub();
		const token = await setUp(github);
		const { cookie } = await signIn(github);
		const call = agent(token);
		await beat(call, box('mac', { rank: 10 }));
		const csrf = /name="csrf" value="([^"]+)"/.exec(await (await send('/', { headers: { cookie: cookie! } })).text())![1]!;
		expect((await postForm('/boxes/limit', { csrf, box: 'mac', class: 'boxrunner', max: '1' }, { cookie: cookie! })).status).toBe(303);
		await beat(call, box('vm', { runners: [runner('vm', 1), runner('vm', 2)], scaleSets: [{ repo: 'octo/site', class: 'boxrunner', assigned: 5 }] }));
		const mac = await beat(call, box('mac', { rank: 10 }));
		expect(mac.limits).toEqual({ boxrunner: 1 });
		expect(mac.fleet).toEqual([{ repo: 'octo/site', class: 'boxrunner', jobs: 1, repos: [] }]);
	});

	it('tells a draining box its share is 0, so it drops what an earlier answer gave it', async () => {
		const call = await ready();
		await beat(call, box('vm', { runners: [runner('vm', 1), runner('vm', 2)], scaleSets: [{ repo: 'octo/site', class: 'boxrunner', assigned: 3 }] }));
		const mac = await beat(call, box('mac', { rank: 10, draining: true }));
		expect(mac.fleet).toEqual([{ repo: 'octo/site', class: 'boxrunner', jobs: 0, repos: [] }]);
	});
});
