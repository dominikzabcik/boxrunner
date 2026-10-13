import type { Box } from './box';
import { appKeyFromPem, boxTokenHash, hmac, randomBytes, hex, randomToken, sameSecret, sha256Base64 } from './crypto';
import { convertManifest, GitHubError } from './github';
import type { SetupRoute } from './routes';

/** Placeholder until the installer is published; the one place its address lives. */
const installerUrl = 'https://boxrunner.invalid/install.sh';

const cookie = '__Host-boxrunner-setup';
const nonceLifetime = 60 * 60 * 1000;
const appNamePattern = /^[A-Za-z0-9][A-Za-z0-9 _.-]{0,33}$/;

export async function handleSetup(box: Box, request: Request, route: SetupRoute): Promise<Response> {
	const origin = new URL(request.url).origin;
	if (route.name === 'setup.rotate') return rotate(box, request, origin);
	if (route.name === 'setup.reset') return reset(box, request);
	if (box.store.app()) return donePage(410);
	const secret = box.setupSecret;
	if (!secret) {
		return messagePage(503, 'Setup is locked', 'Set the SETUP_SECRET secret to at least 32 characters, then reload this page.');
	}
	switch (route.name) {
		case 'setup.form':
			return formPage(200, defaultAppName());
		case 'setup.start':
			return start(request, secret, origin);
		case 'setup.callback':
			return callback(box, request, secret, origin);
	}
}

const defaultAppName = () => `boxrunner-${hex(randomBytes(3))}`;

async function form(request: Request): Promise<FormData | undefined> {
	return request.formData().catch(() => undefined);
}

const formText = (data: FormData | undefined, name: string) => {
	const value = data?.get(name);
	return typeof value === 'string' ? value : '';
};

async function start(request: Request, secret: string, origin: string) {
	const data = await form(request);
	const name = formText(data, 'name').trim();
	if (!(await sameSecret(formText(data, 'secret'), secret))) {
		return formPage(401, name || defaultAppName(), 'That is not the setup secret.');
	}
	if (!appNamePattern.test(name)) {
		return formPage(400, name, 'The App name must be 1 to 34 letters, digits, spaces, dots, dashes or underscores.');
	}
	const nonce = `${Date.now()}.${randomToken()}`;
	const state = await hmac(secret, nonce);
	const manifest = {
		name,
		url: origin,
		redirect_url: `${origin}/setup/callback`,
		public: false,
		hook_attributes: { url: origin, active: false },
		default_permissions: { administration: 'write', metadata: 'read' },
		default_events: [],
	};
	return page({
		status: 200,
		title: 'Sending you to GitHub',
		script: autoSubmit,
		headers: { 'set-cookie': `${cookie}=${nonce}; Path=/; Secure; HttpOnly; SameSite=Lax; Max-Age=3600` },
		body: `<h1>Sending you to GitHub</h1>
<p>GitHub will ask you to confirm the App <strong>${escape(name)}</strong>, then send you back here.</p>
<form id="manifest" method="post" action="https://github.com/settings/apps/new?state=${encodeURIComponent(state)}">
<input type="hidden" name="manifest" value="${escape(JSON.stringify(manifest))}">
<button type="submit">Continue to GitHub</button>
</form>`,
	});
}

function cookieNonce(request: Request): string | undefined {
	for (const part of (request.headers.get('cookie') ?? '').split(';')) {
		const [key, ...value] = part.trim().split('=');
		if (key === cookie) return value.join('=');
	}
	return undefined;
}

async function callback(box: Box, request: Request, secret: string, origin: string) {
	const params = new URL(request.url).searchParams;
	const code = params.get('code');
	const state = params.get('state');
	const nonce = cookieNonce(request);
	const issued = Number(nonce?.split('.')[0]);
	const valid =
		code && state && nonce && Date.now() - issued < nonceLifetime && (await sameSecret(state, await hmac(secret, nonce)));
	if (!valid) {
		return messagePage(400, 'This link is not from your setup', 'It does not match the setup started in this browser, or it is more than an hour old.', true);
	}
	if (box.setupRunning) return messagePage(409, 'Setup is already finishing', 'Another tab is finishing setup. Reload this page in a moment.');
	box.setupRunning = true;
	try {
		const created = await convertManifest(code);
		const key = await appKeyFromPem(created.pem);
		const token = randomToken();
		box.store.saveApp({ id: created.id, slug: created.slug, key, webhookSecret: created.webhookSecret }, await boxTokenHash(token));
		return tokenPage(200, 'boxrunner is ready', created.slug, origin, token, true);
	} catch (error) {
		const why = error instanceof GitHubError ? `${error.message}.` : 'GitHub sent a key the Worker cannot read.';
		return messagePage(502, 'GitHub did not finish creating the App', why, true);
	} finally {
		box.setupRunning = false;
	}
}

async function rotate(box: Box, request: Request, origin: string) {
	const stored = box.store.app();
	if (!stored) return messagePage(503, 'Setup has not finished', 'There is no box token to replace yet.', true);
	const secret = box.setupSecret;
	if (!secret) return messagePage(503, 'Setup is locked', 'Set the SETUP_SECRET secret to at least 32 characters.');
	if (!(await sameSecret(formText(await form(request), 'secret'), secret))) return donePage(401, 'That is not the setup secret.');
	const token = randomToken();
	box.store.replaceBoxToken(await boxTokenHash(token));
	box.resetBudget();
	return tokenPage(200, 'New box token', stored.app.slug, origin, token, false);
}

async function reset(box: Box, request: Request) {
	if (!box.store.app()) return messagePage(409, 'Nothing to reset', 'This Worker has no GitHub App yet.', true);
	const secret = box.setupSecret;
	if (!secret) return messagePage(503, 'Setup is locked', 'Set the SETUP_SECRET secret to at least 32 characters.');
	if (!(await sameSecret(formText(await form(request), 'secret'), secret))) return donePage(401, 'That is not the setup secret.');
	box.store.reset();
	box.resetBudget();
	return messagePage(
		200,
		'The Worker forgot its App',
		'Its box token no longer works. The App still exists on GitHub: delete it in your GitHub settings if you will not use it again.',
		true,
	);
}

// ---- Pages ----

const style = `
:root { color-scheme: light dark; --bg: #fff; --fg: #1a1a1a; --muted: #555; --line: #c8c8c8; --accent: #0b57d0; --alert: #b3261e; }
@media (prefers-color-scheme: dark) { :root { --bg: #141414; --fg: #ececec; --muted: #a8a8a8; --line: #444; --accent: #8ab4f8; --alert: #f2b8b5; } }
body { margin: 0; background: var(--bg); color: var(--fg); font: 16px/1.5 system-ui, sans-serif; }
main { max-width: 40rem; margin: 0 auto; padding: 2rem 1rem; }
h1 { font-size: 1.6rem; line-height: 1.2; }
h2 { font-size: 1.1rem; margin-top: 2rem; }
p, label { max-width: 36rem; }
.hint { color: var(--muted); font-size: 0.9rem; margin-top: 0.25rem; }
label { display: block; font-weight: 600; margin-top: 1.25rem; }
input { display: block; width: 100%; box-sizing: border-box; margin-top: 0.25rem; padding: 0.5rem; font: inherit; color: inherit; background: transparent; border: 1px solid var(--line); border-radius: 4px; }
button { margin-top: 1.5rem; padding: 0.6rem 1rem; font: inherit; font-weight: 600; color: var(--bg); background: var(--fg); border: 0; border-radius: 4px; cursor: pointer; }
:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
a { color: var(--accent); }
pre, code { font-family: ui-monospace, monospace; font-size: 0.9rem; }
pre { padding: 0.75rem; border: 1px solid var(--line); border-radius: 4px; white-space: pre-wrap; word-break: break-all; }
[role=alert] { color: var(--alert); font-weight: 600; }
`;
const autoSubmit = `document.getElementById('manifest').submit();`;

let hashes: Promise<{ style: string; script: string }> | undefined;
const sourceHashes = () => (hashes ??= Promise.all([sha256Base64(style), sha256Base64(autoSubmit)]).then(([s, j]) => ({ style: s, script: j })));

const escape = (text: string) =>
	text.replace(/[&<>"']/g, (char) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[char]!);

async function page({
	status,
	title,
	body,
	script,
	headers = {},
}: {
	status: number;
	title: string;
	body: string;
	script?: string;
	headers?: Record<string, string>;
}) {
	const hash = await sourceHashes();
	const csp = [
		"default-src 'none'",
		`style-src 'sha256-${hash.style}'`,
		script ? `script-src 'sha256-${hash.script}'` : '',
		"form-action 'self' https://github.com",
		"base-uri 'none'",
		"frame-ancestors 'none'",
	]
		.filter(Boolean)
		.join('; ');
	const html = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="robots" content="noindex">
<title>${escape(title)} · boxrunner</title>
<style>${style}</style>
</head>
<body>
<main>
${body}
</main>
${script ? `<script>${script}</script>` : ''}
</body>
</html>
`;
	return new Response(html, {
		status,
		headers: {
			'content-type': 'text/html; charset=utf-8',
			'cache-control': 'no-store',
			'content-security-policy': csp,
			'x-frame-options': 'DENY',
			'x-content-type-options': 'nosniff',
			'referrer-policy': 'no-referrer',
			...headers,
		},
	});
}

const alert = (message?: string) => (message ? `<p role="alert">${escape(message)}</p>` : '');

const secretField = `<label for="secret">Setup secret</label>
<input id="secret" name="secret" type="password" required autocomplete="current-password" aria-describedby="secret-hint">
<p class="hint" id="secret-hint">The SETUP_SECRET you set when you deployed this Worker.</p>`;

function formPage(status: number, name: string, error?: string) {
	return page({
		status,
		title: 'Set up boxrunner',
		body: `<h1>Set up boxrunner</h1>
<p>This creates a private GitHub App under your account. GitHub sends its key straight to this Worker; it never reaches your browser or your box.</p>
${alert(error)}
<form method="post" action="/setup">
${secretField}
<label for="name">App name</label>
<input id="name" name="name" required maxlength="34" value="${escape(name)}" aria-describedby="name-hint">
<p class="hint" id="name-hint">App names are unique across all of GitHub, so the suggestion ends in a random suffix.</p>
<button type="submit">Create the GitHub App</button>
</form>`,
	});
}

function donePage(status: number, error?: string) {
	return page({
		status,
		title: 'boxrunner is set up',
		body: `<h1>boxrunner is set up</h1>
<p>This Worker already has its GitHub App.</p>
<h2>Replace the box token</h2>
<p>A new token stops the old one at once. The box needs the new one to keep working.</p>
${alert(error)}
<form method="post" action="/setup/token">
${secretField}
<button type="submit">Make a new box token</button>
</form>
<h2>Start over</h2>
<p>The Worker forgets its App and box token, and setup can run again. Runners on the box stop getting jobs.</p>
<form method="post" action="/setup/reset">
${secretField.replace(/id="secret"/, 'id="reset-secret"').replace(/for="secret"/, 'for="reset-secret"').replace(/secret-hint/g, 'reset-secret-hint')}
<button type="submit">Forget the App</button>
</form>`,
	});
}

function messagePage(status: number, title: string, text: string, restart = false) {
	return page({
		status,
		title,
		body: `<h1>${escape(title)}</h1>
<p>${escape(text)}</p>
${restart ? '<p><a href="/setup">Start again</a></p>' : ''}`,
	});
}

const installCommand = (origin: string, token: string) =>
	`curl -fsSL ${installerUrl} | sudo sh -s -- --worker ${origin} --token ${token}`;

function tokenPage(status: number, title: string, slug: string, origin: string, token: string, firstTime: boolean) {
	const install = `https://github.com/apps/${encodeURIComponent(slug)}/installations/new`;
	return page({
		status,
		title,
		headers: { 'set-cookie': `${cookie}=; Path=/; Secure; HttpOnly; SameSite=Lax; Max-Age=0` },
		body: `<h1>${escape(title)}</h1>
<p>Copy the box token now. The Worker keeps only its hash, so no page can show it again.</p>
${
	firstTime
		? `<h2>1. Install the App</h2>
<p><a href="${escape(install)}">Install ${escape(slug)} on your private repositories</a>. Public repositories are never served.</p>
<h2>2. Install the agent</h2>`
		: '<h2>Install the agent again</h2>'
}
<p>Run this on the box:</p>
<pre><code>${escape(installCommand(origin, token))}</code></pre>
<p>Box token: <code>${escape(token)}</code></p>`,
	});
}
