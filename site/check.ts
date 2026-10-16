// Builds the site, serves dist/ with its _headers and checks every page in Chromium.
import { chromium, type Browser, type Page } from 'playwright-core';
import { join } from 'node:path';
import { serve } from './serve.ts';

// Landing page bytes on the wire: the HTML gzipped, the four fonts and the response headers.
// Measured 140,871 in October 2026: 21,146 of HTML, 113,020 of fonts and about 6,700 of headers,
// most of them the CSP sent with each response. Cloudflare's brotli makes the HTML a little smaller.
const LANDING_BUDGET = 160_000;

const build = Bun.spawnSync(['bun', 'build.ts'], { cwd: import.meta.dir, stdout: 'inherit', stderr: 'inherit' });
if (build.exitCode !== 0) process.exit(build.exitCode ?? 1);

const server = serve(join(import.meta.dir, 'dist'));
const origin = server.url.origin;
const failures: string[] = [];
const fail = (message: string) => failures.push(message);
const ok = (message: string) => console.log(`ok    ${message}`);

const sitemap = await (await fetch(`${origin}/sitemap.xml`)).text();
const paths = [...sitemap.matchAll(/<loc>https:\/\/boxrunner\.zabcik\.me(\/[^<]*)<\/loc>/g)].map((m) => m[1]!);
const missing = '/no/such/page';

async function headers() {
	const home = await fetch(`${origin}/`);
	const csp = home.headers.get('content-security-policy') ?? '';
	if (!csp.includes("default-src 'none'") || /unsafe-(inline|eval|hashes)/.test(csp)) fail(`/ has a weak or missing CSP: ${csp}`);
	if (!home.headers.get('strict-transport-security')?.includes('preload')) fail('/ has no HSTS preload');
	const font = (await (await fetch(`${origin}/`)).text()).match(/\/fonts\/[\w.-]+\.woff2/)?.[0];
	const fontRes = await fetch(origin + font);
	if (fontRes.headers.get('cache-control') !== 'public, max-age=31536000, immutable') fail(`${font} is not cached as immutable: ${fontRes.headers.get('cache-control')}`);
	const bare = await fetch(`${origin}/docs/config`, { redirect: 'manual' });
	if (bare.status !== 307 || bare.headers.get('location') !== '/docs/config/') fail(`/docs/config answers ${bare.status} to ${bare.headers.get('location')}`);
	const lost = await fetch(origin + missing);
	if (lost.status !== 404 || !(await lost.text()).includes('No page at this address')) fail(`${missing} does not answer with the 404 page`);
	ok(`headers: CSP with hashes only, HSTS preload, immutable ${font}, /docs/config redirects, unknown paths get 404.html`);
}

async function visit(page: Page, path: string, width: number) {
	const problems: string[] = [];
	const url = origin + path;
	page.removeAllListeners();
	page.on('console', (m) => {
		const own404 = path === missing && m.location().url === url && m.text().includes('404');
		if ((m.type() === 'error' || m.type() === 'warning') && !own404) problems.push(`console ${m.type()}: ${m.text()}`);
	});
	page.on('pageerror', (e) => problems.push(`uncaught: ${e.message}`));
	page.on('response', (r) => {
		if (r.status() >= 400 && !(path === missing && r.url() === url)) problems.push(`${r.status()} ${r.url()}`);
	});
	page.on('requestfailed', (r) => problems.push(`failed: ${r.url()} ${r.failure()?.errorText}`));
	await page.goto(url, { waitUntil: 'load' });
	await page.evaluate(() => document.fonts.ready);
	await page.waitForTimeout(400);
	const violations = await page.evaluate(() => (window as unknown as { violations: string[] }).violations);
	problems.push(...violations.map((v) => `CSP violation: ${v}`));
	if (width < 600) {
		const wide = await page.evaluate(() => {
			const vw = document.documentElement.clientWidth;
			const out: string[] = [];
			if (document.documentElement.scrollWidth > vw) out.push(`page scrolls sideways (${document.documentElement.scrollWidth}px)`);
			for (const el of document.querySelectorAll('body *')) {
				const r = el.getBoundingClientRect();
				if (!r.width || (r.right <= vw + 0.5 && r.left >= -0.5)) continue;
				let clipped = false;
				for (let a = el.parentElement; a && a !== document.body; a = a.parentElement) if (getComputedStyle(a).overflowX !== 'visible') clipped = true;
				if (!clipped) out.push(`<${el.tagName.toLowerCase()} class="${el.getAttribute('class') ?? ''}"> spans ${Math.round(r.left)} to ${Math.round(r.right)}px`);
			}
			return out;
		});
		problems.push(...wide);
	}
	for (const p of problems) fail(`${path} at ${width}px: ${p}`);
	return page.evaluate(() => [...document.querySelectorAll('a[href]')].map((a) => (a as HTMLAnchorElement).href));
}

async function links(hrefs: Set<string>) {
	const pages = new Map<string, string>();
	let checked = 0;
	for (const href of hrefs) {
		const url = new URL(href);
		if (url.origin !== origin) continue;
		const key = url.pathname;
		if (!pages.has(key)) {
			const res = await fetch(origin + key);
			if (res.status !== 200) fail(`link ${key} answers ${res.status}`);
			pages.set(key, await res.text());
		}
		if (url.hash && !pages.get(key)!.includes(`id="${decodeURIComponent(url.hash.slice(1))}"`)) fail(`link ${key}${url.hash} points at no element`);
		checked++;
	}
	ok(`${checked} internal links resolve, fragments included`);
}

async function landing(page: Page) {
	const sizes: Record<string, number> = {};
	page.removeAllListeners();
	page.on('requestfinished', async (r) => {
		const kind = r.resourceType();
		if (kind === 'document' || kind === 'font') sizes[r.url()] = (await r.sizes()).responseBodySize + (await r.sizes()).responseHeadersSize;
	});
	await page.goto(`${origin}/`, { waitUntil: 'networkidle' });
	await page.waitForTimeout(2000);
	const lit = await page.locator('.hero canvas.art').evaluate((c: HTMLCanvasElement) => {
		const data = c.getContext('2d')!.getImageData(0, 0, c.width, c.height).data;
		let n = 0;
		for (let i = 3; i < data.length; i += 4) if (data[i]! > 0) n++;
		return n;
	});
	if (lit < 500) fail(`the hero canvas drew only ${lit} pixels after 2 s`);
	else ok(`hero canvas drew ${lit} non-blank pixels after 2 s`);
	const total = Object.values(sizes).reduce((a, b) => a + b, 0);
	const fonts = Object.keys(sizes).filter((u) => u.endsWith('.woff2')).length;
	if (fonts !== 4) fail(`the landing page loaded ${fonts} fonts, expected 4`);
	if (total > LANDING_BUDGET) fail(`the landing page transferred ${total} bytes, over the ${LANDING_BUDGET} budget`);
	else ok(`landing page transferred ${total.toLocaleString('en-US')} bytes (HTML and ${fonts} fonts), budget ${LANDING_BUDGET.toLocaleString('en-US')}`);
	const canary = await page.evaluate(() => {
		const s = document.createElement('script');
		s.textContent = 'window.canary = true';
		document.head.append(s);
		return (window as unknown as { canary?: boolean }).canary === true;
	});
	if (canary) fail('an injected inline script ran: the CSP is not enforced');
	else ok('an injected inline script was blocked by the CSP');
}

// Headlines are written to fit two lines on a phone; a third line means the type or the words grew.
async function headlines(browser: Browser) {
	const wide: string[] = [];
	for (const width of [390, 360]) {
		const context = await browser.newContext({ viewport: { width, height: 800 } });
		const page = await context.newPage();
		await page.goto(`${origin}/`, { waitUntil: 'load' });
		await page.evaluate(() => document.fonts.ready);
		const found = await page.evaluate(() =>
			[...document.querySelectorAll<HTMLElement>('h1, h2, .say')].map((el) => {
				const style = getComputedStyle(el);
				const lineHeight = parseFloat(style.lineHeight) || parseFloat(style.fontSize) * 1.2;
				return { text: el.textContent!.trim().replace(/\s+/g, ' '), lines: Math.round(el.getBoundingClientRect().height / lineHeight) };
			}),
		);
		for (const h of found) if (h.lines > 2) wide.push(`"${h.text}" runs to ${h.lines} lines at ${width}px`);
		await context.close();
	}
	for (const w of wide) fail(`headline: ${w}`);
	if (!wide.length) ok('every headline fits two lines at 390 and 360px');
}

const browser = await chromium.launch();
try {
	await headers();
	const hrefs = new Set<string>();
	for (const [width, colorScheme] of [[1280, 'light'], [1280, 'dark'], [390, 'light'], [390, 'dark']] as const) {
		const context = await browser.newContext({ viewport: { width, height: width < 600 ? 844 : 800 }, colorScheme });
		await context.addInitScript(() => {
			const w = window as unknown as { violations: string[] };
			w.violations = [];
			document.addEventListener('securitypolicyviolation', (e) => w.violations.push(`${e.violatedDirective} ${e.blockedURI || 'inline'}`));
		});
		const page = await context.newPage();
		const before = failures.length;
		for (const path of [...paths, missing]) for (const href of await visit(page, path, width)) hrefs.add(href);
		if (failures.length === before) ok(`${paths.length + 1} pages at ${width}px, ${colorScheme}: no console errors or warnings, no CSP violations${width < 600 ? ', nothing wider than the viewport' : ''}`);
		await context.close();
	}
	await links(hrefs);
	await headlines(browser);
	const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
	await landing(await context.newPage());
	await context.close();
} finally {
	await browser.close();
	server.stop(true);
}

if (failures.length) {
	for (const f of failures) console.error(`FAIL  ${f}`);
	process.exit(1);
}
console.log('All checks passed.');
