import { afterEach, describe, expect, it, vi } from 'vitest';
import fleetFixture from '../../app/macos/Tests/BoxrunnerBarTests/fleet.json';
import { agent, fakeGitHub, heartbeatBody, job, postForm, send, setUp, signIn, time } from './harness';

afterEach(() => {
	vi.useRealTimers();
});

async function ready() {
	time('2026-10-14T10:00:00Z');
	const github = await fakeGitHub();
	const token = await setUp(github);
	const finished = { ...job('box-1-boxrunner-aaaaaa', '2026-10-14T09:50:00Z', '2026-10-14T09:50:20Z', '2026-10-14T09:53:05Z'), usage: { cpuSeconds: 754.25, memoryPeak: 9812000768 } };
	await agent(token)('POST', '/v1/heartbeat', heartbeatBody({ jobs: [finished] }));
	const { cookie } = await signIn(github);
	const view = async (path: string) => send(path, { headers: { cookie: cookie! } });
	const csrf = /name="csrf" value="([^"]+)"/.exec(await (await view('/')).text())![1]!;
	const post = (path: string, fields: Record<string, string>) => postForm(path, { csrf, ...fields }, { cookie: cookie! });
	return { github, token, cookie: cookie!, view, post };
}

const call = (path: string, body?: unknown, device?: string) =>
	send(path, {
		method: body === undefined ? 'GET' : 'POST',
		headers: { 'content-type': 'application/json', ...(device ? { authorization: `Bearer ${device}` } : {}) },
		...(body === undefined ? {} : { body: JSON.stringify(body) }),
	});

async function pair(view: (path: string) => Promise<Response>, post: (path: string, fields: Record<string, string>) => Promise<Response>) {
	const started = await call('/v1/pair', { name: 'Dominik’s Mac' });
	expect(started.status).toBe(201);
	const { code, secret, url, expiresIn } = (await started.json()) as { code: string; secret: string; url: string; expiresIn: number };
	expect(code).toMatch(/^[A-Z2-9]{4}-[A-Z2-9]{4}$/);
	expect(url).toBe(`https://box.test/pair?code=${code}`);
	expect(expiresIn).toBe(600);
	expect((await call('/v1/pair/token', { secret })).status).toBe(202);
	const page = await (await view(`/pair?code=${code}`)).text();
	expect(page).toContain('<h1>Pair Dominik’s Mac?</h1>');
	expect(page).toContain(`<p class="code"><code>${code}</code></p>`);
	expect((await post('/pair', { code })).status).toBe(200);
	const collected = await call('/v1/pair/token', { secret });
	expect(collected.status).toBe(200);
	const { token, device } = (await collected.json()) as { token: string; device: { name: string; by: string } };
	expect(token).toMatch(/^brd_[A-Za-z0-9_-]{43}$/);
	expect(device).toMatchObject({ name: 'Dominik’s Mac', by: 'octo' });
	expect((await call('/v1/pair/token', { secret })).status).toBe(404);
	return token;
}

describe('pairing a device', () => {
	it('gives an approved device a token that reads the fleet and sets limits, until it is unpaired', async () => {
		const { view, post } = await ready();
		const device = await pair(view, post);

		const fleet = (await (await call('/v1/fleet', undefined, device)).json()) as { boxes: { name: string }[]; device: { name: string } };
		// The menu bar app decodes this answer; its test reads the same file.
		const devices = (fleet as unknown as { devices: { id: string }[] }).devices.map((d) => ({ ...d, id: 'device-id' }));
		expect({ ...fleet, workerVersion: 'worker-version', devices, device: { ...fleet.device, id: 'device-id' } }).toEqual(fleetFixture);
		expect(fleet.boxes.map((b) => b.name)).toEqual(['box-1']);
		expect(fleet.device.name).toBe('Dominik’s Mac');

		const limited = await call('/v1/fleet/limit', { box: 'box-1', class: 'boxrunner', max: 0 }, device);
		expect(await limited.json()).toEqual({ limits: [{ class: 'boxrunner', max: 0 }] });
		const html = await (await view('/')).text();
		expect(html).toContain('<td><p>0 by octo,');
		expect(html).toContain('<tr><td>Dominik’s Mac</td><td>octo</td>');
		expect(((await (await call('/v1/fleet/limit', { box: 'box-1', class: 'boxrunner', max: null }, device)).json()) as { limits: unknown[] }).limits).toEqual([]);
		expect((await call('/v1/fleet/limit', { box: 'box-1', class: 'boxrunner', max: 5 }, device)).status).toBe(400);

		const id = /name="id" value="([^"]+)"/.exec(html)![1]!;
		expect((await post('/devices/unpair', { id })).status).toBe(303);
		expect((await call('/v1/fleet', undefined, device)).status).toBe(401);
	});

	it('refuses the fleet without a device token, and the box token is not one', async () => {
		const { token } = await ready();
		expect((await call('/v1/fleet')).status).toBe(401);
		expect((await call('/v1/fleet', undefined, token)).status).toBe(401);
		expect((await call('/v1/fleet', undefined, 'brd_forged')).status).toBe(401);
	});

	it('expires a pairing after 10 minutes and lets at most 5 wait', async () => {
		const { view } = await ready();
		const { code, secret } = (await (await call('/v1/pair', { name: 'laptop' })).json()) as { code: string; secret: string };
		time('2026-10-14T10:10:01Z');
		expect((await call('/v1/pair/token', { secret })).status).toBe(404);
		expect(await (await view(`/pair?code=${code}`)).text()).toContain('<h1>No such pairing</h1>');
		for (let i = 0; i < 5; i++) expect((await call('/v1/pair', { name: `laptop ${i}` })).status).toBe(201);
		expect((await call('/v1/pair', { name: 'one more' })).status).toBe(429);
	});

	it('approves only from a signed-in dashboard with its CSRF token, and takes only a sensible name', async () => {
		const { cookie } = await ready();
		const { code } = (await (await call('/v1/pair', { name: 'laptop' })).json()) as { code: string };
		expect((await send(`/pair?code=${code}`)).status).toBe(401);
		expect((await postForm('/pair', { code, csrf: 'forged' }, { cookie })).status).toBe(403);
		for (const name of ['', ' ', 'x'.repeat(65), 'bell\u0007', 42]) expect((await call('/v1/pair', { name })).status).toBe(400);
	});
});
