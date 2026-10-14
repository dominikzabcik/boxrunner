import { sha256Base64 } from './crypto';
import { favicon } from './favicon';

const style = `
:root { color-scheme: light dark; --bg: #fff; --fg: #1a1a1a; --muted: #555; --line: #c8c8c8; --accent: #0b57d0; --alert: #b3261e; }
@media (prefers-color-scheme: dark) { :root { --bg: #141414; --fg: #ececec; --muted: #a8a8a8; --line: #444; --accent: #8ab4f8; --alert: #f2b8b5; } }
body { margin: 0; background: var(--bg); color: var(--fg); font: 16px/1.5 system-ui, sans-serif; }
main { max-width: 40rem; margin: 0 auto; padding: 2rem 1rem; }
main.wide, header { max-width: 72rem; }
header { margin: 0 auto; padding: 1rem 1rem 0; display: flex; flex-wrap: wrap; gap: 0.5rem 1rem; align-items: center; justify-content: space-between; }
header form { display: flex; gap: 0.75rem; align-items: center; }
header button { margin-top: 0; }
h1 { font-size: 1.6rem; line-height: 1.2; }
h2 { font-size: 1.1rem; margin-top: 2rem; }
h3 { font-size: 1rem; margin: 1.25rem 0 0; }
p, label { max-width: 36rem; }
.hint { color: var(--muted); font-size: 0.9rem; margin-top: 0.25rem; }
label { display: block; font-weight: 600; margin-top: 1.25rem; }
input { display: block; width: 100%; box-sizing: border-box; margin-top: 0.25rem; padding: 0.5rem; font: inherit; color: inherit; background: transparent; border: 1px solid var(--line); border-radius: 4px; }
input[type=checkbox] { display: inline; width: auto; margin: 0 0.5rem 0 0; }
button, .button { display: inline-block; margin-top: 1.5rem; padding: 0.6rem 1rem; font: inherit; font-weight: 600; color: var(--bg); background: var(--fg); border: 0; border-radius: 4px; cursor: pointer; text-decoration: none; }
:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
a { color: var(--accent); }
pre, code { font-family: ui-monospace, monospace; font-size: 0.9rem; }
pre { padding: 0.75rem; border: 1px solid var(--line); border-radius: 4px; white-space: pre-wrap; word-break: break-all; }
[role=alert], .alert { color: var(--alert); font-weight: 600; }
table { width: 100%; min-width: max-content; border-collapse: collapse; margin-top: 0.5rem; font-size: 0.9rem; }
.scroll { overflow-x: auto; }
th, td { text-align: left; padding: 0.4rem 1rem 0.4rem 0; border-bottom: 1px solid var(--line); vertical-align: top; }
th:last-child, td:last-child { padding-right: 0; }
td form { margin: 0; }
td button { margin-top: 0; padding: 0.25rem 0.75rem; }
th { color: var(--muted); font-weight: 600; }
.number { text-align: right; font-variant-numeric: tabular-nums; }
dl { display: grid; grid-template-columns: max-content 1fr; gap: 0.25rem 1rem; margin: 0.5rem 0 0; }
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
<meta name="color-scheme" content="light dark">
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
