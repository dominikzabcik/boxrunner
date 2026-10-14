import { afterEach, describe, expect, it, vi } from 'vitest';
import { agent, callback, fakeGitHub, origin, postForm, send, setUp, startSetup, time, tokenIn } from './harness';
import { setupSecret } from './secret';

afterEach(() => {
	vi.useRealTimers();
});

describe('GET /setup before setup', () => {
	it('shows the form with a random App name, under a strict CSP', async () => {
		await fakeGitHub();
		const response = await send('/setup');
		const html = await response.text();
		expect(response.status).toBe(200);
		expect(html).toMatch(/<input id="name" name="name" required maxlength="34" value="boxrunner-[0-9a-f]{6}"/);
		expect(html).toContain('<input id="secret" name="secret" type="password"');
		expect(response.headers.get('x-frame-options')).toBe('DENY');
		expect(response.headers.get('content-security-policy')).toMatch(
			/^default-src 'none'; style-src 'sha256-[A-Za-z0-9+/=]+'; form-action 'self' https:\/\/github\.com; base-uri 'none'; frame-ancestors 'none'$/,
		);
	});
});

async function sha256(text: string) {
	const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text)));
	return btoa(String.fromCharCode(...digest));
}

describe('POST /setup', () => {
	it('allows exactly its own inline style and script, so a browser runs the auto-submit', async () => {
		await fakeGitHub();
		const response = await postForm('/setup', { secret: setupSecret, name: 'boxrunner-test' });
		const html = await response.text();
		const style = /<style>([\s\S]*?)<\/style>/.exec(html)![1]!;
		const script = /<script>([\s\S]*?)<\/script>/.exec(html)![1]!;
		expect(script).toBe("document.getElementById('manifest').submit();");
		expect(response.headers.get('content-security-policy')).toBe(
			`default-src 'none'; style-src 'sha256-${await sha256(style)}'; script-src 'sha256-${await sha256(script)}'; form-action 'self' https://github.com; base-uri 'none'; frame-ancestors 'none'`,
		);
	});

	it('refuses a missing or wrong secret', async () => {
		await fakeGitHub();
		for (const fields of [{ name: 'boxrunner-x' }, { secret: 'wrong', name: 'boxrunner-x' }]) {
			const response = await postForm('/setup', fields);
			expect(response.status).toBe(401);
			expect(response.headers.get('set-cookie')).toBeNull();
			expect(await response.text()).toContain('<p role="alert">That is not the setup secret.</p>');
		}
	});

	it('sends the manifest to GitHub with a state signed for the cookie', async () => {
		await fakeGitHub();
		const response = await postForm('/setup', { secret: setupSecret, name: 'boxrunner-test' });
		expect(response.headers.get('set-cookie')).toMatch(
			/^__Host-boxrunner-setup=\d+\.[A-Za-z0-9_-]{43}; Path=\/; Secure; HttpOnly; SameSite=Lax; Max-Age=3600$/,
		);
		expect(response.headers.get('content-security-policy')).toContain("script-src 'sha256-");
		const { manifest } = await startSetup();
		expect(JSON.parse(manifest)).toEqual({
			name: 'boxrunner-test',
			url: origin,
			redirect_url: `${origin}/setup/callback`,
			callback_urls: [`${origin}/login/callback`],
			public: false,
			hook_attributes: { url: origin, active: false },
			default_permissions: { administration: 'write', metadata: 'read' },
			default_events: [],
		});
	});
});

describe('GET /setup/callback', () => {
	it('refuses a state that does not match the cookie, and a missing cookie', async () => {
		const github = await fakeGitHub();
		const first = await startSetup();
		const second = await startSetup();
		for (const response of [
			await callback(github.manifestCode, first.state, second.cookie),
			await callback(github.manifestCode, first.state),
			await callback(github.manifestCode, 'forged', first.cookie),
		]) {
			expect(response.status).toBe(400);
			expect(await response.text()).toContain('<h1>This link is not from your setup</h1>');
		}
		expect(github.count('POST', `/app-manifests/${github.manifestCode}/conversions`)).toBe(0);
		expect((await send('/setup')).status).toBe(200);
	});

	it('shows a box token once with the install link and command, and the token works', async () => {
		const github = await fakeGitHub();
		const { cookie, state } = await startSetup();
		const response = await callback(github.manifestCode, state, cookie);
		const html = await response.text();
		const token = tokenIn(html)!;
		expect(response.status).toBe(200);
		expect(html).toContain('<a href="https://github.com/apps/boxrunner-test/installations/new">');
		expect(html).toContain(
			`<pre><code>curl -fsSL https://boxrunner.zabcik.me/install.sh | sudo sh -s -- --worker ${origin} --token ${token}</code></pre>`,
		);
		expect(response.headers.get('set-cookie')).toBe('__Host-boxrunner-setup=; Path=/; Secure; HttpOnly; SameSite=Lax; Max-Age=0');
		expect((await agent(token)('GET', '/v1/repos')).status).toBe(200);
	});

	it('answers 502 when GitHub refuses the code, and setup can start again', async () => {
		const github = await fakeGitHub();
		const { cookie, state } = await startSetup();
		const response = await callback('expired-code', state, cookie);
		expect(response.status).toBe(502);
		expect(await response.text()).toContain('GitHub answered 404 to POST /app-manifests/{code}/conversions.');
		expect((await send('/setup')).status).toBe(200);
		expect(await setUp(github)).toMatch(/^[A-Za-z0-9_-]{43}$/);
	});
});

describe('after setup', () => {
	it('answers 410 to every setup step', async () => {
		const github = await fakeGitHub();
		const { cookie, state } = await startSetup();
		await setUp(github);
		expect((await send('/setup')).status).toBe(410);
		expect((await postForm('/setup', { secret: setupSecret, name: 'again' })).status).toBe(410);
		expect((await callback(github.manifestCode, state, cookie)).status).toBe(410);
		expect(github.count('POST', `/app-manifests/${github.manifestCode}/conversions`)).toBe(1);
	});

	it('rotates the box token with the setup secret and sends the browser to a page that shows the new one once', async () => {
		const github = await fakeGitHub();
		const old = await setUp(github);
		const refused = await postForm('/setup/token', { secret: 'wrong' });
		expect(refused.status).toBe(401);
		expect((await agent(old)('GET', '/v1/repos')).status).toBe(200);

		const response = await postForm('/setup/token', { secret: setupSecret });
		const location = response.headers.get('location')!;
		expect(response.status).toBe(303);
		expect(response.headers.get('cache-control')).toBe('no-store');
		expect(location).toMatch(/^\/setup\/token\/[A-Za-z0-9_-]{43}$/);
		expect((await agent(old)('GET', '/v1/repos')).status).toBe(401);

		const shown = await send(location);
		const html = await shown.text();
		const fresh = tokenIn(html)!;
		expect(shown.status).toBe(200);
		expect(shown.headers.get('cache-control')).toBe('no-store');
		expect(html).toContain(
			`<pre><code>curl -fsSL https://boxrunner.zabcik.me/install.sh | sudo sh -s -- --worker ${origin} --token ${fresh}</code></pre>`,
		);
		expect((await agent(fresh)('GET', '/v1/repos')).status).toBe(200);

		const reloaded = await send(location);
		const again = await reloaded.text();
		expect(reloaded.status).toBe(410);
		expect(reloaded.headers.get('cache-control')).toBe('no-store');
		expect(tokenIn(again)).toBeUndefined();
		expect(again).toContain('<h1>This box token was already shown</h1>');
		expect(again).toContain('<p><a href="/setup">Replace it on the setup page</a></p>');
		expect((await agent(fresh)('GET', '/v1/repos')).status).toBe(200);
	});

	it('shows the rotated token only within a minute', async () => {
		const github = await fakeGitHub();
		time('2026-10-14T10:00:00Z');
		await setUp(github);
		const late = (await postForm('/setup/token', { secret: setupSecret })).headers.get('location')!;
		time('2026-10-14T10:01:00Z');
		const expired = await send(late);
		expect(expired.status).toBe(410);
		expect(await expired.text()).toContain('<h1>This box token was already shown</h1>');

		const onTime = (await postForm('/setup/token', { secret: setupSecret })).headers.get('location')!;
		time('2026-10-14T10:01:59Z');
		expect((await send(onTime)).status).toBe(200);
	});

	it('has nothing to rotate before setup', async () => {
		await fakeGitHub();
		expect((await postForm('/setup/token', { secret: setupSecret })).status).toBe(503);
	});

	it('forgets the App with the setup secret, so setup can run again', async () => {
		const github = await fakeGitHub();
		const old = await setUp(github);
		expect((await postForm('/setup/reset', { secret: 'wrong' })).status).toBe(401);
		expect((await agent(old)('GET', '/v1/repos')).status).toBe(200);

		const response = await postForm('/setup/reset', { secret: setupSecret });
		expect(response.status).toBe(200);
		expect(await response.text()).toContain('<h1>The Worker forgot its App</h1>');
		expect((await agent(old)('GET', '/v1/repos')).status).toBe(503);
		expect((await send('/setup')).status).toBe(200);
		expect(await setUp(github)).toMatch(/^[A-Za-z0-9_-]{43}$/);
	});

	it('has nothing to reset before setup', async () => {
		await fakeGitHub();
		expect((await postForm('/setup/reset', { secret: setupSecret })).status).toBe(409);
	});
});
