import type { Box } from './box';
import { hmac, randomToken, sameSecret } from './crypto';
import { dashboardData, refreshScript, renderLive } from './dashboard';
import * as github from './github';
import { ApiError, errorResponse, json } from './http';
import { parse, type App } from './model';
import { escape, messagePage, page, readCookie } from './page';
import type { WebRoute } from './routes';
import { endSession, readSession, startSession, type Session } from './session';

const loginCookie = '__Host-boxrunner-login';
const loginLifetime = 10 * 60 * 1000;
const signInAgain = { href: '/login', text: 'Sign in again' };

export async function handleWeb(box: Box, request: Request, route: WebRoute): Promise<Response> {
	const origin = new URL(request.url).origin;
	const stored = box.store.app();
	if (!stored) {
		if (route.name === 'web.data') return errorResponse(new ApiError('not_configured', `Setup has not finished. Open ${origin}/setup.`));
		return messagePage(503, 'boxrunner is not set up yet', 'Create its GitHub App on the setup page first.', { href: '/setup', text: 'Set up boxrunner' });
	}
	const { app } = stored;
	const key = box.sessionKey();
	const now = Date.now();
	const session = await readSession(key, request, now, box.mayOpenDashboard(app.owner));
	try {
		switch (route.name) {
			case 'web.dashboard':
				return session ? dashboardPage(box, session, now) : landingPage(box, app, origin, 200);
			case 'web.data':
				if (!session) return errorResponse(new ApiError('unauthorized', 'Sign in to the dashboard first.'));
				return json(dashboardJson(box, session, now));
			case 'web.login':
				return await login(box, app, origin, key, now);
			case 'web.callback':
				return await callback(box, app, request, origin, key, now);
			case 'web.logout':
				return await logout(request, session);
			case 'web.token':
			case 'web.pause':
			case 'web.resume':
				return await act(box, request, route.name, session, now);
		}
	} catch (error) {
		if (!(error instanceof github.GitHubError)) throw error;
		return messagePage(502, 'GitHub did not finish the sign-in', `${error.message}.`, signInAgain);
	}
}

function dashboardJson(box: Box, session: Session, now: number) {
	const data = dashboardData(box.store, box.classes(), box.fallback(), now);
	return { ...data, html: renderLive(data, session.csrf) };
}

function landingPage(box: Box, app: App, origin: string, status: number) {
	if (box.canSignIn(app)) {
		return page({
			status,
			title: 'Sign in',
			body: `<h1>boxrunner</h1>
<p>This Worker runs your GitHub Actions jobs on your own box. Its dashboard shows the box, its runners and recent jobs.</p>
<p><a class="button" href="/login">Sign in with GitHub</a></p>
<p class="hint">Only the account that owns the GitHub App, and the accounts in the Worker’s ADMINS variable, can open it.</p>`,
		});
	}
	return page({
		status,
		title: 'Signing in needs a client secret',
		body: `<h1>Signing in needs the App’s client secret</h1>
<p>This Worker’s GitHub App was set up before boxrunner kept the App’s client secret, so it cannot sign you in yet.</p>
<ol>
<li>Open <a href="https://github.com/settings/apps/${encodeURIComponent(app.slug)}">the App’s settings</a> on GitHub.</li>
<li>Under <strong>Client secrets</strong>, generate a new client secret and copy it.</li>
<li>Under <strong>Callback URL</strong>, add <code>${escape(origin)}/login/callback</code> and save the changes.</li>
<li>In the Worker’s directory, run <code>npx wrangler secret put GITHUB_CLIENT_SECRET</code> and paste the secret.</li>
<li>Run <code>npx wrangler deploy</code>: the Worker reads its secrets when it starts.</li>
</ol>
<p>Then reload this page.</p>`,
	});
}

async function login(box: Box, app: App, origin: string, key: string, now: number) {
	const oauth = await box.oauthClient(app);
	if (!oauth) return landingPage(box, app, origin, 503);
	const nonce = `${now}.${randomToken()}`;
	const authorize = new URL('https://github.com/login/oauth/authorize');
	authorize.search = new URLSearchParams({
		client_id: oauth.client.id,
		redirect_uri: `${origin}/login/callback`,
		state: await hmac(key, `login:${nonce}`),
	}).toString();
	return new Response(null, {
		status: 302,
		headers: {
			location: authorize.toString(),
			'cache-control': 'no-store',
			'set-cookie': `${loginCookie}=${nonce}; Path=/; Secure; HttpOnly; SameSite=Lax; Max-Age=${loginLifetime / 1000}`,
		},
	});
}

async function callback(box: Box, app: App, request: Request, origin: string, key: string, now: number) {
	const clearLogin = { 'set-cookie': `${loginCookie}=; Path=/; Secure; HttpOnly; SameSite=Lax; Max-Age=0` };
	const params = new URL(request.url).searchParams;
	const nonce = readCookie(request, loginCookie);
	const state = params.get('state');
	const issued = Number(nonce?.split('.')[0]);
	const fromHere = nonce && state && now - issued < loginLifetime && (await sameSecret(state, await hmac(key, `login:${nonce}`)));
	if (!fromHere) {
		return messagePage(400, 'This sign-in did not start here', 'It does not match a sign-in this browser started in the last 10 minutes.', signInAgain, clearLogin);
	}
	const code = params.get('code');
	if (!code) return messagePage(400, 'Sign-in was cancelled', 'GitHub sent no code, so nobody is signed in.', signInAgain, clearLogin);
	const oauth = await box.oauthClient(app);
	if (!oauth) return landingPage(box, app, origin, 503);
	const token = await github.userToken(oauth.client, code, `${origin}/login/callback`);
	if (!token) {
		return messagePage(400, 'GitHub did not accept this sign-in', 'Its code expired or was already used.', signInAgain, clearLogin);
	}
	const login = await github.userLogin(token);
	if (!box.mayOpenDashboard(oauth.owner)(login)) {
		return messagePage(
			403,
			'This account may not open the dashboard',
			`You signed in to GitHub as ${login}. Only the account that owns this Worker’s GitHub App, and the accounts in its ADMINS variable, may open the dashboard. To use another account, sign out of GitHub first.`,
			undefined,
			clearLogin,
		);
	}
	// The session cookie is SameSite=Strict, and a redirect chain that began at
	// github.com would not send it; a page that moves on by itself does.
	const response = await page({
		status: 200,
		title: 'Signed in',
		next: '/',
		body: `<h1>Signed in as ${escape(login)}</h1>
<p><a href="/">Open the dashboard</a></p>`,
	});
	response.headers.append('set-cookie', clearLogin['set-cookie']);
	response.headers.append('set-cookie', await startSession(key, login, now));
	return response;
}

const openDashboard = { href: '/', text: 'Open the dashboard' };

/** The form, if its CSRF token is the session's. */
async function formOf(request: Request, session: Session): Promise<FormData | undefined> {
	const form = await request.formData().catch(() => undefined);
	const token = form?.get('csrf');
	return typeof token === 'string' && (await sameSecret(token, session.csrf)) ? form : undefined;
}

async function logout(request: Request, session: Session | undefined) {
	if (session && !(await formOf(request, session))) {
		return messagePage(403, 'This sign-out did not come from your dashboard', 'Sign out with the button on the dashboard.', openDashboard);
	}
	return new Response(null, { status: 303, headers: { location: '/', 'cache-control': 'no-store', 'set-cookie': endSession } });
}

/** The dashboard's actions. Each is a form post carrying the session's CSRF token. */
async function act(box: Box, request: Request, action: 'web.token' | 'web.pause' | 'web.resume', session: Session | undefined, now: number) {
	if (!session) return messagePage(401, 'Sign in first', 'This browser is not signed in to the dashboard, so nothing changed.', { href: '/login', text: 'Sign in' });
	const form = await formOf(request, session);
	if (!form) return messagePage(403, 'This action did not come from your dashboard', 'Nothing changed. Use the buttons on the dashboard.', openDashboard);
	if (action === 'web.token') return tokenPage(await box.replaceBoxToken());
	const classes = box.classes();
	const className = parse.className(form.get('class'));
	if (!className || !classes.ok || !classes.classes.includes(className)) {
		return messagePage(400, 'No such class', 'That is not one of this Worker’s classes, so nothing changed.', openDashboard);
	}
	if (action === 'web.pause') box.store.pause(className, session.login, now);
	else box.store.resume(className);
	return new Response(null, { status: 303, headers: { location: '/#classes', 'cache-control': 'no-store' } });
}

function tokenPage(token: string) {
	return page({
		status: 200,
		title: 'New box token',
		body: `<h1>New box token</h1>
<p>The old token stopped working. A box cannot reach this Worker until it has the new one.</p>
<p>Copy the token now. The Worker keeps only its hash, so no page can show it again.</p>
<p>Run this on each box:</p>
<pre><code>sudo boxrunner install --token ${escape(token)}</code></pre>
<p class="hint">It writes the token to <code>/etc/boxrunner/token</code> and restarts the agent. Jobs that are running keep running.</p>
<p>Box token: <code>${escape(token)}</code></p>
<p><a href="/">Back to the dashboard</a></p>`,
	});
}

function dashboardPage(box: Box, session: Session, now: number) {
	return page({
		status: 200,
		title: 'Dashboard',
		wide: true,
		script: refreshScript,
		connect: true,
		header: `<p><strong>boxrunner</strong></p>
<form method="post" action="/logout">
<span>Signed in as ${escape(session.login)}</span>
<input type="hidden" name="csrf" value="${escape(session.csrf)}">
<button type="submit">Sign out</button>
</form>`,
		body: `<h1>Dashboard</h1>
<p class="hint" id="refreshing" hidden>Updates every 10 seconds.</p>
<noscript><p class="hint">Reload the page to update it.</p></noscript>
<div id="live">
${renderLive(dashboardData(box.store, box.classes(), box.fallback(), now), session.csrf)}
</div>
<section aria-labelledby="token">
<h2 id="token">Box token</h2>
<p>Every box uses it to reach this Worker. A new token stops the old one at once, so each box stops working until it has the new one.</p>
<form method="post" action="/box-token">
<input type="hidden" name="csrf" value="${escape(session.csrf)}">
<button type="submit">Replace the box token</button>
</form>
</section>`,
	});
}
