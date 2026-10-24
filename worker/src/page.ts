import { sha256Base64 } from './crypto';
import { favicon } from './favicon';

// One dark, monospaced look in the manner of a terminal UI, shared with the menu bar app.
const style = `
:root { color-scheme: dark; --bg: #0a0a0a; --panel: #141414; --line: #262626; --fg: #eeeeee; --muted: #808080; --faint: #4a4a4a; --accent: #fab283; --link: #5c9cf5; --ok: #7fd88f; --alert: #e06c75; }
* { box-sizing: border-box; }
body { margin: 0; background: var(--bg); color: var(--fg); font: 13px/1.6 ui-monospace, "SF Mono", "JetBrains Mono", Menlo, Consolas, monospace; -webkit-font-smoothing: antialiased; }
::selection { background: var(--accent); color: var(--bg); }
main { max-width: 44rem; margin: 0 auto; padding: 2.5rem 1rem 4rem; }
main.wide, header { max-width: 76rem; }
header { position: sticky; top: 0; z-index: 1; margin: 0 auto; padding: 0.6rem 1rem; display: flex; flex-wrap: wrap; gap: 0.5rem 1rem; align-items: center; justify-content: space-between; background: var(--bg); border-bottom: 1px solid var(--line); }
header p { margin: 0; }
header strong::after { content: "_"; color: var(--accent); }
header form { display: flex; gap: 0.75rem; align-items: center; color: var(--muted); }
header button { margin-top: 0; color: var(--fg); background: transparent; border: 1px solid var(--line); font-weight: 400; }
header button:hover { border-color: var(--muted); background: transparent; }
h1 { font-size: 1.35rem; line-height: 1.3; margin: 0 0 0.25rem; }
h1::before { content: "❯ "; color: var(--accent); }
h2 { font-size: 0.95rem; margin: 2.75rem 0 0.75rem; padding: 0.15rem 0 0.15rem 0.75rem; border-left: 2px solid var(--accent); }
h3 { display: flex; gap: 0.75rem; align-items: center; font-size: 0.75rem; font-weight: 500; color: var(--muted); text-transform: lowercase; margin: 1.75rem 0 0.25rem; }
h3::after { content: ""; flex: 1; border-top: 1px solid var(--line); }
p, label { max-width: 40rem; }
.hint { color: var(--muted); font-size: 0.8rem; margin-top: 0.4rem; }
label { display: block; font-weight: 600; margin-top: 1.25rem; }
input { display: block; width: 100%; margin-top: 0.35rem; padding: 0.45rem 0.6rem; font: inherit; color: inherit; background: var(--panel); border: 1px solid var(--line); border-radius: 0; }
input:focus { outline: none; border-color: var(--accent); }
input[type=checkbox] { display: inline; width: auto; margin: 0 0.5rem 0 0; accent-color: var(--accent); }
button, .button { display: inline-block; margin-top: 1.5rem; padding: 0.4rem 0.9rem; font: inherit; font-weight: 600; color: var(--bg); background: var(--accent); border: 1px solid var(--accent); border-radius: 0; cursor: pointer; text-decoration: none; }
button:hover, .button:hover { background: #fcc9a5; }
:focus-visible { outline: 1px solid var(--accent); outline-offset: 2px; }
a { color: var(--link); text-underline-offset: 3px; }
a:hover { color: var(--fg); }
pre, code { font: inherit; }
code { color: var(--accent); }
pre { padding: 0.75rem 1rem; background: var(--panel); border: 1px solid var(--line); border-left: 2px solid var(--faint); white-space: pre-wrap; word-break: break-all; }
pre code { color: var(--fg); }
[role=alert], .alert { color: var(--alert); font-weight: 600; }
[role=alert]::before { content: "error "; }
table { width: 100%; min-width: max-content; border-collapse: collapse; margin-top: 0.25rem; font-size: 0.85rem; }
.scroll { overflow-x: auto; }
th, td { text-align: left; padding: 0.4rem 1.25rem 0.4rem 0; border-bottom: 1px solid var(--line); vertical-align: top; }
tbody tr:hover td { background: var(--panel); }
th:last-child, td:last-child { padding-right: 0; }
td form { margin: 0; }
td button { margin-top: 0; padding: 0.15rem 0.6rem; }
.code code { display: block; padding: 1.25rem 0; font-size: 2.5rem; font-weight: 700; letter-spacing: 0.2em; text-align: center; color: var(--accent); background: var(--panel); border: 1px solid var(--line); border-left: 2px solid var(--accent); }
form.inline { display: flex; gap: 0.5rem; align-items: center; }
form.inline input[type=number] { width: 4.5rem; margin-top: 0; padding: 0.15rem 0.4rem; }
th { color: var(--muted); font-weight: 500; text-transform: lowercase; }
.number { text-align: right; font-variant-numeric: tabular-nums; }
dl { display: grid; grid-template-columns: max-content 1fr; gap: 0.15rem 1.5rem; margin: 0.5rem 0 0; }
dt { color: var(--muted); }
dd { margin: 0; overflow-wrap: anywhere; }
`;

const hashes = new Map<string, Promise<string>>();
const hashOf = (source: string) => {
	let hash = hashes.get(source);
	if (!hash) hashes.set(source, (hash = sha256Base64(source)));
	return hash;
};

export const escape = (text: string) =>
	text.replace(/[&<>"']/g, (char) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[char]!);

export type Page = {
	status: number;
	title: string;
	body: string;
	/** Rendered before `<main>`, for the dashboard's sign-out bar. */
	header?: string;
	wide?: boolean;
	/** One inline script, allowed by its hash. */
	script?: string;
	/** Sources besides this origin that forms may post to. */
	formTargets?: string;
	/** Lets the script fetch from this origin. */
	connect?: boolean;
	/** Sends the browser on to this path once the page loads, without a script. */
	next?: string;
	headers?: Record<string, string>;
};

/** An HTML page whose CSP allows exactly its own inline style and script, and its favicon as a data URI. */
export async function page({ status, title, body, header, wide, script, formTargets, connect, next, headers = {} }: Page) {
	const csp = [
		"default-src 'none'",
		`style-src 'sha256-${await hashOf(style)}'`,
		'img-src data:',
		script ? `script-src 'sha256-${await hashOf(script)}'` : '',
		connect ? "connect-src 'self'" : '',
		`form-action 'self'${formTargets ? ` ${formTargets}` : ''}`,
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
<meta name="color-scheme" content="dark">
<meta name="robots" content="noindex">
<link rel="icon" href="${favicon}">
${next ? `<meta http-equiv="refresh" content="0; url=${escape(next)}">\n` : ''}<title>${escape(title)} · boxrunner</title>
<style>${style}</style>
</head>
<body>
${header ? `<header>\n${header}\n</header>\n` : ''}<main${wide ? ' class="wide"' : ''}>
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

export const alert = (message?: string) => (message ? `<p role="alert">${escape(message)}</p>` : '');

export function messagePage(status: number, title: string, text: string, link?: { href: string; text: string }, headers?: Record<string, string>) {
	return page({
		status,
		title,
		...(headers ? { headers } : {}),
		body: `<h1>${escape(title)}</h1>
<p>${escape(text)}</p>
${link ? `<p><a href="${escape(link.href)}">${escape(link.text)}</a></p>` : ''}`,
	});
}

/** Answers a form post with a page to load, so a reload loads the page instead of posting again. */
export const seeOther = (location: string, headers: Record<string, string> = {}) =>
	new Response(null, { status: 303, headers: { location, 'cache-control': 'no-store', ...headers } });

/** A new box token's page once it was shown, or after its minute. */
export const tokenShownPage = (replace: { href: string; text: string }) =>
	messagePage(
		410,
		'This box token was already shown',
		'The Worker shows a new box token once, within a minute of making it, and then keeps only its hash. If you did not copy it, replace the token again.',
		replace,
	);

export function readCookie(request: Request, name: string): string | undefined {
	for (const part of (request.headers.get('cookie') ?? '').split(';')) {
		const [key, ...value] = part.trim().split('=');
		if (key === name) return value.join('=');
	}
	return undefined;
}
