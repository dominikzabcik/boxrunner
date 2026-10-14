import { afterEach, describe, expect, it, vi } from 'vitest';
import { agent, expectError, fakeGitHub, heartbeatBody, job, send, setUp, signIn, time } from './harness';

afterEach(() => {
	vi.useRealTimers();
});

async function ready() {
	time('2026-10-14T10:00:00Z');
	const github = await fakeGitHub();
	const token = await setUp(github);
	const dashboard = async () => {
		const { cookie } = await signIn(github);
		const response = await send('/dashboard.json', { headers: { cookie: cookie! } });
		expect(response.status).toBe(200);
		return (await response.json()) as {
			boxes: { name: string; version: string; online: boolean }[];
			jobs: { runner: string; result: string; minutes: number }[];
			totals: { today: { jobs: number; minutes: number; dollars: number }; last30Days: { jobs: number; minutes: number; dollars: number } };
		};
	};
	return { call: agent(token), token, dashboard };
}

describe('POST /v1/heartbeat', () => {
	it('answers with the paused classes and keeps the latest status per box', async () => {
		const { call, dashboard } = await ready();
		const response = await call('POST', '/v1/heartbeat', heartbeatBody());
		expect(response.status).toBe(200);
		expect(await response.json()).toEqual({ paused: [] });
		await call('POST', '/v1/heartbeat', heartbeatBody({ version: '0.2.1' }));
		const { boxes } = await dashboard();
		expect(boxes.map(({ name, online, version }) => ({ name, online, version }))).toEqual([
			{ name: 'box-1', online: true, version: '0.2.1' },
		]);
	});

	it('needs the box token and a version like every /v1 call', async () => {
		const { call } = await ready();
		await expectError(await agent('A'.repeat(43))('POST', '/v1/heartbeat', heartbeatBody()), 401, 'unauthorized');
		await expectError(await agent('A'.repeat(43), '9.0.0')('POST', '/v1/heartbeat', heartbeatBody()), 426, 'upgrade_required');
		expect((await call('POST', '/v1/heartbeat', heartbeatBody())).status).toBe(200);
	});

	it('names the first malformed field', async () => {
		const { call } = await ready();
		const cases: [Record<string, unknown>, string][] = [
			[{ box: 'box 1' }, 'box'],
			[{ version: 'v0.2' }, 'version'],
			[{ disk: { free: -1, floor: 0 } }, 'disk.free'],
			[{ classes: [{ name: 'boxrunner', priority: 'high', max: 1 }] }, 'classes[0].priority'],
			[{ classes: [{ name: 'boxrunner', priority: 'idle', max: 0 }] }, 'classes[0].max'],
			[{ runners: [{ name: 'r-1', repo: 'octo/site', class: 'boxrunner', state: 'paused', since: '2026-10-14T09:00:00Z' }] }, 'runners[0].state'],
			[{ runners: [{ name: 'r-1', repo: 'octo/site', class: 'boxrunner', state: 'running', since: 'yesterday' }] }, 'runners[0].since'],
			[{ jobs: [job('r-1', '2026-10-14T09:00:00Z', null, '2026-10-14T09:01:00Z', { name: '' })] }, 'jobs[0].name'],
			[{ jobs: Array.from({ length: 51 }, (_, i) => job(`r-${i}`, '2026-10-14T09:00:00Z', null, '2026-10-14T09:01:00Z')) }, 'jobs'],
		];
		for (const [override, field] of cases) {
			await expectError(await call('POST', '/v1/heartbeat', heartbeatBody(override)), 400, 'bad_request', `"${field}" is missing or malformed.`);
		}
	});

	it('refuses a body over 16 KB, also when it is sent without a length', async () => {
		const { call, token } = await ready();
		const large = JSON.stringify(heartbeatBody({ padding: 'x'.repeat(16 * 1024) }));
		await expectError(await call('POST', '/v1/heartbeat', JSON.parse(large)), 400, 'bad_request', 'The body is larger than 16 KB.');
		const chunked = new ReadableStream({
			start(controller) {
				controller.enqueue(new TextEncoder().encode(large));
				controller.close();
			},
		});
		const response = await send('/v1/heartbeat', {
			method: 'POST',
			headers: { authorization: `Bearer ${token}`, 'boxrunner-version': '0.2.0' },
			body: chunked,
		});
		await expectError(response, 400, 'bad_request', 'The body is larger than 16 KB.');
	});

	it('upserts jobs by runner name, so a job sent in every heartbeat counts once', async () => {
		const { call, dashboard } = await ready();
		const first = job('box-1-boxrunner-aaaaaa', '2026-10-14T09:00:00Z', '2026-10-14T09:00:10Z', '2026-10-14T09:02:10Z');
		await call('POST', '/v1/heartbeat', heartbeatBody({ jobs: [first] }));
		await call('POST', '/v1/heartbeat', heartbeatBody({ jobs: [first] }));
		await call('POST', '/v1/heartbeat', heartbeatBody({ jobs: [{ ...first, result: 'failed' }] }));
		const { jobs, totals } = await dashboard();
		expect(jobs.map(({ runner, result, minutes }) => ({ runner, result, minutes }))).toEqual([
			{ runner: 'box-1-boxrunner-aaaaaa', result: 'failed', minutes: 2 },
		]);
		expect(totals.today).toEqual({ jobs: 1, minutes: 2, dollars: 0.01 });
	});

	it('accepts a job an older agent logged without its queue time, and shows no wait for it', async () => {
		const { call, dashboard } = await ready();
		const old = { ...job('box-1-boxrunner-bbbbbb', '2026-10-14T09:00:00Z', '2026-10-14T09:00:10Z', '2026-10-14T09:02:10Z'), queued: null };
		expect((await call('POST', '/v1/heartbeat', heartbeatBody({ jobs: [old] }))).status).toBe(200);
		const { jobs } = (await dashboard()) as unknown as { jobs: { runner: string; queued: string | null; waitSeconds: number | null }[] };
		expect(jobs.map(({ runner, queued, waitSeconds }) => ({ runner, queued, waitSeconds }))).toEqual([
			{ runner: 'box-1-boxrunner-bbbbbb', queued: null, waitSeconds: null },
		]);
	});

	it('rounds each job up to whole minutes, as GitHub bills them', async () => {
		const { call, dashboard } = await ready();
		await call(
			'POST',
			'/v1/heartbeat',
			heartbeatBody({
				jobs: [
					job('r-exactly-one', '2026-10-14T09:00:00Z', '2026-10-14T09:00:00Z', '2026-10-14T09:01:00Z'),
					job('r-one-and-a-bit', '2026-10-14T09:00:00Z', '2026-10-14T09:00:00Z', '2026-10-14T09:01:01Z'),
					job('r-half-second', '2026-10-14T09:00:00Z', '2026-10-14T09:00:00.000Z', '2026-10-14T09:00:00.500Z'),
					job('r-never-started', '2026-10-14T09:00:00Z', null, '2026-10-14T09:05:00Z', { result: 'canceled' }),
					job('r-yesterday', '2026-10-13T09:00:00Z', '2026-10-13T09:00:00Z', '2026-10-13T10:39:59Z'),
				],
			}),
		);
		const { jobs, totals } = await dashboard();
		expect(Object.fromEntries(jobs.map((j) => [j.runner, j.minutes]))).toEqual({
			'r-exactly-one': 1,
			'r-one-and-a-bit': 2,
			'r-half-second': 1,
			'r-never-started': 0,
			'r-yesterday': 100,
		});
		expect(totals).toEqual({ today: { jobs: 4, minutes: 4, dollars: 0.02 }, last30Days: { jobs: 5, minutes: 104, dollars: 0.62 } });
	});

	it('keeps jobs for 30 days', async () => {
		const { call, dashboard } = await ready();
		await call(
			'POST',
			'/v1/heartbeat',
			heartbeatBody({
				jobs: [
					job('r-31-days-ago', '2026-09-13T09:00:00Z', '2026-09-13T09:00:00Z', '2026-09-13T09:10:00Z'),
					job('r-29-days-ago', '2026-09-15T09:00:00Z', '2026-09-15T09:00:00Z', '2026-09-15T09:10:00Z'),
					job('r-today', '2026-10-14T09:00:00Z', '2026-10-14T09:00:00Z', '2026-10-14T09:10:00Z'),
				],
			}),
		);
		expect((await dashboard()).jobs.map((j) => j.runner)).toEqual(['r-today', 'r-29-days-ago']);
		expect((await dashboard()).totals.last30Days).toEqual({ jobs: 2, minutes: 20, dollars: 0.12 });

		time('2026-10-15T10:00:00Z');
		await call('POST', '/v1/heartbeat', heartbeatBody());
		expect((await dashboard()).jobs.map((j) => j.runner)).toEqual(['r-today']);
		expect((await dashboard()).totals).toEqual({ today: { jobs: 0, minutes: 0, dollars: 0 }, last30Days: { jobs: 1, minutes: 10, dollars: 0.06 } });
	});
});
