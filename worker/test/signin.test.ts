import { afterEach, describe, expect, it, vi } from 'vitest';
import type { FakeGitHub } from './fake-github';
import { fakeGitHub, postForm, send, setUp, signIn, time, withEnv } from './harness';
import { setupSecret } from './secret';

afterEach(() => {
	vi.useRealTimers();
});

async function ready(): Promise<FakeGitHub> {
	const github = await fakeGitHub();
	await setUp(github);
	return github;
}

const page = async (cookie?: string) => {
	const response = await send('/', { headers: cookie ? { cookie } : {} });
	return { status: response.status, h1: /<h1>(.*?)<\/h1>/.exec(await response.text())?.[1] };
};

const signedIn = { status: 200, h1: 'Dashboard' };
const signedOut = { status: 200, h1: 'boxrunner' };

describe('GET / without a session', () => {
	it('offers to sign in with GitHub', async () => {
		await ready();
		const response = await send('/');
		const html = await response.text();
		expect(response.status).toBe(200);
		expect(html).toContain('<a class="button" href="/login">Sign in with GitHub</a>');
		expect(response.headers.get('content-security-policy')).toMatch(
			/^default-src 'none'; style-src 'sha256-[A-Za-z0-9+/=]+'; img-src data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'$/,
		);
	});

	it('points to setup before setup', async () => {
		await fakeGitHub();
		const response = await send('/');
		expect(response.status).toBe(503);
		expect(await response.text()).toContain('<p><a href="/setup">Set up boxrunner</a></p>');
	});
});

describe('GET /login', () => {
	it('sends the browser to GitHub with a state tied to a 10-minute cookie', async () => {
		await ready();
		const response = await send('/login');
		const location = new URL(response.headers.get('location')!);
		expect(response.status).toBe(302);
		expect(`${location.origin}${location.pathname}`).toBe('https://github.com/login/oauth/authorize');
		expect([...location.searchParams.keys()]).toEqual(['client_id', 'redirect_uri', 'state']);
		expect(location.searchParams.get('client_id')).toBe('Iv1.test');
		expect(location.searchParams.get('redirect_uri')).toBe('https://box.test/login/callback');
		expect(location.searchParams.get('state')).toMatch(/^[A-Za-z0-9_-]{43}$/);
		expect(response.headers.get('set-cookie')).toMatch(
			/^__Host-boxrunner-login=\d{13}\.[A-Za-z0-9_-]{43}; Path=\/; Secure; HttpOnly; SameSite=Lax; Max-Age=600$/,
		);
	});
});

describe('GET /login/callback', () => {
	it('refuses a state that does not match the cookie, and asks GitHub nothing', async () => {
		const github = await ready();
		const first = await send('/login');
		const second = await send('/login');
		const state = new URL(first.headers.get('location')!).searchParams.get('state')!;
		const otherCookie = second.headers.get('set-cookie')!.split(';')[0]!;
		for (const cookie of [otherCookie, '']) {
			const response = await send(`/login/callback?code=${github.authorize('octo')}&state=${state}`, { headers: { cookie } });
			expect(response.status).toBe(400);
			expect(await response.text()).toContain('<h1>This sign-in did not start here</h1>');
			expect(response.headers.getSetCookie()).toEqual(['__Host-boxrunner-login=; Path=/; Secure; HttpOnly; SameSite=Lax; Max-Age=0']);
		}
		expect(github.count('POST', '/login/oauth/access_token')).toBe(0);
	});

	it('refuses a state older than 10 minutes', async () => {
		const github = await ready();
		time('2026-10-14T10:00:00Z');
		const start = await send('/login');
		time('2026-10-14T10:10:00Z');
		const state = new URL(start.headers.get('location')!).searchParams.get('state')!;
		const response = await send(`/login/callback?code=${github.authorize('octo')}&state=${state}`, {
			headers: { cookie: start.headers.get('set-cookie')!.split(';')[0]! },
		});
		expect(response.status).toBe(400);
	});

	it('signs the owner in for 12 hours with a strict cookie, and keeps no GitHub token', async () => {
		const github = await ready();
		const { response, cookie } = await signIn(github, 'octo');
		const html = await response.text();
		expect(response.status).toBe(200);
		expect(html).toContain('<meta http-equiv="refresh" content="0; url=/">');
		expect(html).toContain('<h1>Signed in as octo</h1>');
		const [clearLogin, session] = response.headers.getSetCookie();
		expect(clearLogin).toBe('__Host-boxrunner-login=; Path=/; Secure; HttpOnly; SameSite=Lax; Max-Age=0');
		expect(session).toMatch(/^__Host-boxrunner-session=octo\.\d{13}\.[A-Za-z0-9_-]{43}; Path=\/; Secure; HttpOnly; SameSite=Strict; Max-Age=43200$/);
		expect(session).not.toContain('ghu_');
		expect(github.calls.find((call) => call.path === '/login/oauth/access_token')?.body).toEqual({
			client_id: 'Iv1.test',
			client_secret: 'client-secret',
			code: expect.stringMatching(/^oauth-code-\d+$/),
			redirect_uri: 'https://box.test/login/callback',
		});
		expect(await page(cookie)).toEqual(signedIn);
	});

	it('lets in the accounts in ADMINS, whatever their case', async () => {
		const github = await ready();
		for (const login of ['hubot', 'MONA']) {
			const { response, cookie } = await signIn(github, login);
			expect(response.status).toBe(200);
			expect(await page(cookie)).toEqual(signedIn);
		}
	});

	it('refuses any other account and says which one signed in', async () => {
		const github = await ready();
		const { response, cookie } = await signIn(github, 'mallory');
		expect(response.status).toBe(403);
		expect(cookie).toBeUndefined();
		expect(await response.text()).toContain('<p>You signed in to GitHub as mallory. Only the account that owns this Worker’s GitHub App');
	});

	it('says that only ADMINS can sign in when an organization owns the App', async () => {
		const github = await ready();
		const hint = async () => /<p class="hint">(.*?)<\/p>/.exec(await (await send('/')).text())?.[1];
		await withEnv({ APP_OWNER: 'octo' }, async () => {
			const owned = 'This Worker’s GitHub App belongs to the organization octo, which cannot sign in, so only the accounts in its ADMINS variable can open the dashboard.';
			expect(await hint()).toBe(owned);
			const { response } = await signIn(github, 'mallory');
			expect(response.status).toBe(403);
			expect(await response.text()).toContain(`<p>You signed in to GitHub as mallory. ${owned} To use another account, sign out of GitHub first.</p>`);
			expect((await signIn(github, 'mona')).response.status).toBe(200);
			await withEnv({ ADMINS: '' }, async () => {
				expect(await hint()).toBe(`${owned} ADMINS is empty, so nobody can open it until you set it and deploy the Worker again.`);
			});
		});
		expect(await hint()).toBe('Only the account that owns this Worker’s GitHub App, and the accounts in its ADMINS variable, can open the dashboard.');
	});

	it('refuses a code GitHub does not accept', async () => {
		const github = await ready();
		const start = await send('/login');
		const state = new URL(start.headers.get('location')!).searchParams.get('state')!;
		const response = await send(`/login/callback?code=made-up&state=${state}`, { headers: { cookie: start.headers.get('set-cookie')!.split(';')[0]! } });
		expect(response.status).toBe(400);
		expect(await response.text()).toContain('<h1>GitHub did not accept this sign-in</h1>');
		expect(response.headers.getSetCookie()).toEqual(['__Host-boxrunner-login=; Path=/; Secure; HttpOnly; SameSite=Lax; Max-Age=0']);
	});
});

describe('a session', () => {
	it('lasts 12 hours', async () => {
		const github = await ready();
		time('2026-10-14T10:00:00Z');
		const { cookie } = await signIn(github);
		time('2026-10-14T21:59:59Z');
		expect(await page(cookie)).toEqual(signedIn);
		time('2026-10-14T22:00:00Z');
		expect(await page(cookie)).toEqual(signedOut);
	});

	it('is refused when its login, expiry or signature changed', async () => {
		const github = await ready();
		const { cookie } = await signIn(github);
		const [name, value] = cookie!.split('=') as [string, string];
		const [login, expiry, mac] = value.split('.') as [string, string, string];
		const flipped = mac.at(-1) === 'A' ? 'B' : 'A';
		for (const forged of [`hubot.${expiry}.${mac}`, `${login}.${Number(expiry) - 1}.${mac}`, `${login}.${expiry}.${mac.slice(0, -1)}${flipped}`]) {
			expect(await page(`${name}=${forged}`)).toEqual(signedOut);
		}
		expect(await page(cookie)).toEqual(signedIn);
	});

	it('ends when ADMINS no longer lists the account', async () => {
		const github = await ready();
		const { cookie } = await signIn(github, 'hubot');
		await withEnv({ ADMINS: 'mona' }, async () => {
			expect(await page(cookie)).toEqual(signedOut);
		});
	});

	it('ends when setup is reset, even under the same App owner', async () => {
		const github = await ready();
		const { cookie } = await signIn(github);
		await postForm('/setup/reset', { secret: setupSecret });
		await setUp(github);
		expect(await page(cookie)).toEqual(signedOut);
	});
});

describe('POST /logout', () => {
	const csrfOf = async (cookie: string) => /name="csrf" value="([^"]+)"/.exec(await (await send('/', { headers: { cookie } })).text())?.[1];

	it("needs the session's CSRF token, then clears the session", async () => {
		const github = await ready();
		const { cookie } = await signIn(github);
		const { cookie: other } = await signIn(github, 'hubot');
		for (const fields of [{}, { csrf: 'forged' }, { csrf: (await csrfOf(other!))! }]) {
			const refused = await postForm('/logout', fields, { cookie: cookie! });
			expect(refused.status).toBe(403);
			expect(refused.headers.get('set-cookie')).toBeNull();
		}
		expect(await page(cookie)).toEqual(signedIn);

		const response = await postForm('/logout', { csrf: (await csrfOf(cookie!))! }, { cookie: cookie! });
		expect(response.status).toBe(303);
		expect(response.headers.get('location')).toBe('/');
		expect(response.headers.get('set-cookie')).toBe('__Host-boxrunner-session=; Path=/; Secure; HttpOnly; SameSite=Strict; Max-Age=0');
	});
});

describe('an App set up before the Worker kept its client secret', () => {
	async function legacy() {
		const github = await fakeGitHub();
		github.legacyApp = true;
		await setUp(github);
		return github;
	}

	it('explains how to give the Worker the secret', async () => {
		await legacy();
		for (const [path, status] of [
			['/', 200],
			['/login', 503],
		] as const) {
			const response = await send(path);
			const html = await response.text();
			expect(response.status).toBe(status);
			expect(html).toContain('<h1>Signing in needs the App’s client secret</h1>');
			expect(html).toContain('<a href="https://github.com/settings/apps/boxrunner-test">the App’s settings</a>');
			expect(html).toContain('add <code>https://box.test/login/callback</code>');
			expect(html).toContain('<code>npx wrangler secret put GITHUB_CLIENT_SECRET</code>');
		}
	});

	it('signs in with GITHUB_CLIENT_SECRET, reading the client id and owner from GitHub once', async () => {
		const github = await legacy();
		github.clientSecret = 'secret-from-wrangler';
		await withEnv({ GITHUB_CLIENT_SECRET: 'secret-from-wrangler' }, async () => {
			const first = await signIn(github);
			const second = await signIn(github);
			expect(await page(first.cookie)).toEqual(signedIn);
			expect(await page(second.cookie)).toEqual(signedIn);
			expect((await signIn(github, 'mallory')).response.status).toBe(403);
		});
		expect(github.count('GET', '/app')).toBe(1);
	});
});
