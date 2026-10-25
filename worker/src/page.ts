import { sha256Base64 } from './crypto';
import { favicon } from './favicon';

// One dark, monospaced look in the manner of a terminal UI, shared with the menu bar app.
const style = `
:root { color-scheme: dark; --bg: #0a0a0a; --panel: #141414; --line: #262626; --fg: #eeeeee; --muted: #808080; --faint: #4a4a4a; --accent: #fab283; --link: #5c9cf5; --ok: #7fd88f; --warn: #e5c07b; --alert: #e06c75; }
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
form.inline input[type=number] { width: 3.5rem; margin-top: 0; padding: 0.15rem 0.4rem; }
th { color: var(--muted); font-weight: 500; text-transform: lowercase; }
.number { text-align: right; font-variant-numeric: tabular-nums; }
dl { display: grid; grid-template-columns: max-content 1fr; gap: 0.15rem 1.5rem; margin: 0.5rem 0 0; }
dt { color: var(--muted); }
dd { margin: 0; overflow-wrap: anywhere; }
.muted, td small, .tree small { color: var(--muted); }
.faint { color: var(--faint); }
.warn { color: var(--warn); }
td small, .tree small { display: block; font-size: 0.75rem; }
.sr { position: absolute; width: 1px; height: 1px; overflow: hidden; clip-path: inset(50%); white-space: nowrap; }
.live { color: var(--muted); }
.live::before { content: "● "; color: var(--ok); }
.live[data-state=stale]::before { color: var(--alert); }
.summary { display: flex; flex-wrap: wrap; gap: 0.75rem 2.5rem; margin: 1.5rem 0 2.5rem; padding: 0.9rem 1.1rem; background: var(--panel); border: 1px solid var(--line); border-left: 2px solid var(--accent); }
.summary div { display: flex; flex-direction: column-reverse; }
.summary dt { color: var(--muted); font-size: 0.75rem; }
.summary dd { font-size: 1.5rem; font-weight: 700; line-height: 1.2; font-variant-numeric: tabular-nums; }
.summary .run { color: var(--link); } .summary .ok { color: var(--ok); } .summary .accent { color: var(--accent); } .summary .zero { color: var(--faint); }
.summary .warn { color: var(--warn); } .summary .alert { color: var(--alert); }
.board { display: grid; gap: 2.5rem 2rem; grid-template-columns: minmax(0, 1fr); align-items: start; }
@media (min-width: 72rem) { .board { grid-template-columns: minmax(0, 1fr) 26rem; } }
.stack, .boxes { display: grid; gap: 2.5rem 1.5rem; min-width: 0; }
.boxes { grid-template-columns: repeat(auto-fill, minmax(min(100%, 25rem), 1fr)); }
.stack > .hint { margin-top: -1.5rem; max-width: none; }
.block { position: relative; min-width: 0; padding: 1.4rem 1rem 1rem; border: 1px solid var(--line); box-shadow: inset 2px 0 0 var(--edge, var(--line)); }
main > .block { margin-top: 2.5rem; }
.block > h2, .block > .trail { position: absolute; top: 0; transform: translateY(-50%); margin: 0; padding: 0 0.5rem; background: var(--bg); font-size: 0.75rem; line-height: 1.4; }
.block > h2 { left: 0.6rem; display: flex; gap: 0.5rem; align-items: center; border: 0; font-weight: 500; color: var(--muted); text-transform: lowercase; }
.block > h2 .name { color: var(--fg); font-size: 0.9rem; font-weight: 700; text-transform: none; }
.block > .trail { right: 0.6rem; color: var(--faint); }
.block > h2 + *, .block > .trail + * { margin-top: 0; }
.block > :last-child { margin-bottom: 0; }
.block h3 { margin-top: 1.25rem; }
.block table { min-width: 0; }
.block .number, .block time, .block form { white-space: nowrap; }
.block .number small { white-space: normal; }
.block ul:not(.tree) { margin: 0.4rem 0 0; padding: 0; list-style: none; }
.block ul:not(.tree) li::before { content: "· "; color: var(--faint); }
[data-edge=run] { --edge: var(--link); } [data-edge=ok] { --edge: var(--ok); } [data-edge=warn] { --edge: var(--warn); }
[data-edge=off] { --edge: var(--faint); } [data-edge=alert] { --edge: var(--alert); } [data-edge=accent] { --edge: var(--accent); }
.dot { font-size: 0.7rem; }
.dot.run { color: var(--link); animation: pulse 1.8s ease-in-out infinite; }
.dot.ok { color: var(--ok); } .dot.warn { color: var(--warn); } .dot.off { color: var(--faint); }
@keyframes pulse { 50% { opacity: 0.3; } }
@media (prefers-reduced-motion: reduce) { .dot.run { animation: none; } }
.row { display: flex; flex-wrap: wrap; gap: 0.5rem 1rem; align-items: center; justify-content: space-between; }
.row p, .row form { margin: 0; }
.slots { font-size: 1.15rem; letter-spacing: 0.12em; white-space: nowrap; }
.slots .run { color: var(--link); } .slots .start { color: var(--link); opacity: 0.4; } .slots .free { color: var(--muted); } .slots .cap { color: var(--faint); }
.tree { list-style: none; margin: 0.4rem 0 0; padding: 0; display: grid; gap: 0.5rem; }
.tree li { display: flex; gap: 0.6rem; align-items: baseline; }
.tree .grow { flex: 1; min-width: 0; overflow-wrap: anywhere; }
.state { color: var(--muted); } .state.running { color: var(--link); }
.result { font-weight: 700; color: var(--muted); } .result.succeeded { color: var(--ok); } .result.failed { color: var(--alert); }
button.ghost, td button.ghost { margin: 0; padding: 0.1rem 0.6rem; font-weight: 400; color: var(--fg); background: transparent; border-color: var(--line); }
button.ghost:hover { color: var(--accent); border-color: var(--accent); background: transparent; }
button.switch { display: inline-flex; margin: 0; padding: 0; font-size: 0.75rem; font-weight: 700; color: var(--faint); background: transparent; border-color: var(--line); }
button.switch span { padding: 0.15rem 0.5rem; }
button.switch.on span:first-child { color: var(--bg); background: var(--ok); }
button.switch.off span:last-child { color: var(--bg); background: var(--warn); }
button.switch:hover { background: transparent; border-color: var(--muted); }
button.danger { color: var(--alert); background: transparent; border-color: var(--alert); }
button.danger:hover { color: var(--bg); background: var(--alert); }
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
