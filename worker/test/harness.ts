import { abortAllDurableObjects, runInDurableObject } from 'cloudflare:test';
import { env, exports } from 'cloudflare:workers';
import { afterEach, expect, vi } from 'vitest';
import { appKeyPair, FakeGitHub } from './fake-github';
import { setupSecret } from './secret';

export const origin = 'https://box.test';
const keys = appKeyPair();

// reset() from cloudflare:test leaves this Durable Object's SQLite rows in
// place, so each test wipes them and restarts the object, which then
// recreates its tables and forgets its in-memory caches and budget.
afterEach(async () => {
	vi.restoreAllMocks();
	await runInDurableObject(env.BOX.getByName('box'), async (_, state) => {
		await state.storage.deleteAlarm();
		await state.storage.deleteAll();
	});
	await abortAllDurableObjects();
});

/** Points every outbound fetch of the Worker at a fresh fake GitHub. */
export async function fakeGitHub() {
	const { publicKey, pem } = await keys;
	const github = new FakeGitHub(publicKey, pem);
	vi.spyOn(globalThis, 'fetch').mockImplementation(github.fetch);
	return github;
}

/** Sends a request as a browser's first hop: redirects come back as they are. */
export const send = (path: string, init?: RequestInit) => exports.default.fetch(new Request(`${origin}${path}`, { redirect: 'manual', ...init }));

export const postForm = (path: string, fields: Record<string, string>, headers: Record<string, string> = {}) =>
	send(path, { method: 'POST', body: new URLSearchParams(fields), headers });

/** Starts setup in a "browser": returns the cookie it set and the state it sent to GitHub. */
export async function startSetup(name = 'boxrunner-test') {
	const response = await postForm('/setup', { secret: setupSecret, name });
	expect(response.status).toBe(200);
	const nonce = /__Host-boxrunner-setup=([^;]+)/.exec(response.headers.get('set-cookie') ?? '')?.[1];
	const html = await response.text();
	const state = /action="https:\/\/github\.com\/settings\/apps\/new\?state=([^"]+)"/.exec(html)?.[1];
	const manifest = /name="manifest" value="([^"]+)"/.exec(html)?.[1];
	if (!nonce || !state || !manifest) throw new Error(`setup did not start: ${html}`);
	return { cookie: `__Host-boxrunner-setup=${nonce}`, state: decodeURIComponent(state), manifest: unescapeHtml(manifest), html };
}

const unescapeHtml = (text: string) =>
	text.replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&');

export const callback = (code: string, state: string, cookie?: string) =>
	send(`/setup/callback?code=${encodeURIComponent(code)}&state=${encodeURIComponent(state)}`, {
		headers: cookie ? { cookie } : {},
	});

export const tokenIn = (html: string) => /Box token: <code>([A-Za-z0-9_-]{43})<\/code>/.exec(html)?.[1];

/** Runs the whole browser setup against the fake GitHub and returns the box token the page showed. */
export async function setUp(github: FakeGitHub): Promise<string> {
	const { cookie, state } = await startSetup();
	const response = await callback(github.manifestCode, state, cookie);
	const token = tokenIn(await response.text());
	if (response.status !== 200 || !token) throw new Error(`setup failed with ${response.status}`);
	return token;
}

export type Agent = (method: string, path: string, body?: unknown) => Promise<Response>;

export const agent =
	(token: string, version = '0.1.0'): Agent =>
	(method, path, body) =>
		send(path, {
			method,
			headers: { authorization: `Bearer ${token}`, 'boxrunner-version': version, 'content-type': 'application/json' },
			body: body === undefined ? null : JSON.stringify(body),
		});

export async function expectError(response: Response, status: number, error: string, message?: string) {
	const body = (await response.json()) as { error: string; message: string };
	expect({ status: response.status, error: body.error }).toEqual({ status, error });
	if (message !== undefined) expect(body.message).toBe(message);
	expect(response.headers.get('boxrunner-protocol')).toBe('1');
	expect(response.headers.get('cache-control')).toBe('no-store');
}

/** Signs in through /login and GitHub's authorize page as `login`; `cookie` is the session, if one was set. */
export async function signIn(github: FakeGitHub, login = 'octo') {
	const start = await send('/login');
	const state = new URL(start.headers.get('location') ?? 'https://invalid').searchParams.get('state') ?? '';
	const nonce = /__Host-boxrunner-login=([^;]+)/.exec(start.headers.get('set-cookie') ?? '')?.[1];
	const response = await send(`/login/callback?code=${github.authorize(login)}&state=${encodeURIComponent(state)}`, {
		headers: { cookie: `__Host-boxrunner-login=${nonce}` },
	});
	const session = response.headers
		.getSetCookie()
		.map((cookie) => /^__Host-boxrunner-session=([^;]+)/.exec(cookie)?.[1])
		.find(Boolean);
	return { response, cookie: session ? `__Host-boxrunner-session=${session}` : undefined };
}

/** Sets Worker variables or secrets for one test, as `wrangler secret put` would. */
export async function withEnv(vars: Record<string, string>, run: () => Promise<void>) {
	const stub = env.BOX.getByName('box');
	const set = (values: Record<string, string | undefined>) =>
		runInDurableObject(stub, (instance) => {
			Object.assign((instance as unknown as { env: Record<string, unknown> }).env, values);
		});
	const before = await runInDurableObject(stub, (instance) => {
		const current = (instance as unknown as { env: Record<string, string | undefined> }).env;
		return Object.fromEntries(Object.keys(vars).map((name) => [name, current[name]]));
	});
	await set(vars);
	try {
		await run();
	} finally {
		await set(before);
	}
}

export const time = (iso: string) => {
	vi.useFakeTimers({ toFake: ['Date'] });
	vi.setSystemTime(new Date(iso));
};

export const heartbeatBody = (overrides: Record<string, unknown> = {}) => ({
	box: 'box-1',
	version: '0.2.0',
	image: '2.338.0+3f9a2c0b1d4e',
	disk: { free: 26_000_000_000, floor: 10_000_000_000 },
	classes: [
		{ name: 'boxrunner', priority: 'normal', max: 1 },
		{ name: 'boxrunner-light', priority: 'idle', max: 2 },
	],
	runners: [
		{ name: 'box-1-boxrunner-3f9a2c', repo: 'octo/site', class: 'boxrunner', state: 'running', since: '2026-10-14T09:55:00Z' },
		{ name: 'box-1-boxrunner-light-1b2c3d', repo: 'octo/docs', class: 'boxrunner-light', state: 'starting', since: '2026-10-14T09:59:30Z' },
	],
	jobs: [],
	...overrides,
});

export const job = (runner: string, queued: string, started: string | null, finished: string, extra: Record<string, unknown> = {}) => ({
	runner,
	repo: 'octo/site',
	class: 'boxrunner',
	name: 'test',
	result: 'succeeded',
	queued,
	started,
	finished,
	...extra,
});
