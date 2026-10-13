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
	await runInDurableObject(env.BOX.getByName('box'), (_, state) => state.storage.deleteAll());
	await abortAllDurableObjects();
});

/** Points every outbound fetch of the Worker at a fresh fake GitHub. */
export async function fakeGitHub() {
	const { publicKey, pem } = await keys;
	const github = new FakeGitHub(publicKey, pem);
	vi.spyOn(globalThis, 'fetch').mockImplementation(github.fetch);
	return github;
}

export const send = (path: string, init?: RequestInit) => exports.default.fetch(new Request(`${origin}${path}`, init));

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
