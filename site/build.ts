// Builds dist/: the landing page, the docs rendered from ../docs and ../README.md, a 404 page,
// fonts under content-hashed names, the icons, the OG image, the web manifest, robots.txt,
// sitemap.xml and _headers with a CSP that allows each inline <script> and <style> by its hash.
import { Marked } from 'marked';
import { chromium, type Browser } from 'playwright-core';
import { createHash } from 'node:crypto';
import { cpSync, existsSync, mkdirSync, readdirSync, rmSync, statSync } from 'node:fs';
import { join, posix } from 'node:path';
import { serve } from './serve.ts';

const ORIGIN = 'https://boxrunner.zabcik.me';
const REPO = 'https://github.com/dominikzabcik/boxrunner';
// The live stats endpoint does not exist yet; replace this with its origin when it does.
// `.invalid` can never resolve, so until then the CSP allows nothing real.
const STATS_ORIGIN = 'https://stats.boxrunner.invalid';

const site = import.meta.dir;
const root = join(site, '..');
const dist = join(site, 'dist');
const read = (path: string) => Bun.file(join(root, path)).text();

type Doc = { slug: string; src: string };
// The docs in sidebar order. README.md gives the Quickstart page at /docs/.
const DOCS: Doc[] = [
	{ slug: '', src: 'README.md' },
	{ slug: 'architecture', src: 'docs/architecture.md' },
	{ slug: 'config', src: 'docs/config.md' },
	{ slug: 'upgrading', src: 'docs/upgrading.md' },
	{ slug: 'fallback', src: 'docs/fallback.md' },
	{ slug: 'alerts', src: 'docs/alerts.md' },
	{ slug: 'unserved-jobs', src: 'docs/unserved-jobs.md' },
	{ slug: 'api', src: 'docs/api.md' },
	{ slug: 'dashboard', src: 'docs/dashboard.md' },
	{ slug: 'decisions', src: 'docs/decisions.md' },
];
const docPath = (doc: Doc) => `/docs/${doc.slug ? `${doc.slug}/` : ''}`;

type Page = { path: string; html: string };
type Rendered = { doc: Doc; title: string; description: string; toc: { id: string; html: string }[]; body: string };

// The header's mark, drawn by docs/art/build.py, is inlined where index.html says <!--mark-->.
const mark = (await Bun.file(join(site, 'mark.svg')).text()).trim();
const landing = (await read('site/index.html')).replace('<!--mark-->', mark);
if (!landing.includes(mark)) throw new Error('index.html: the <!--mark--> placeholder is missing');
const landingStyle = landing.match(/<style>([\s\S]*?)<\/style>/)?.[1] ?? '';
const cut = landingStyle.indexOf('/* Everything above this line is shared');
if (cut < 0) throw new Error('index.html: the marker that ends the shared stylesheet is missing');
const base = landingStyle.slice(0, cut);
const tokens = Object.fromEntries([...base.matchAll(/--([\w-]+):\s*([^;]+);/g)].map((m) => [m[1]!, m[2]!.trim()]));
const nav = (landing.match(/<nav class="top[\s\S]*?<\/nav>/)?.[0] ?? '').replace('href="#top"', 'href="/"').replaceAll('href="#', 'href="/#');
const meta = landing.match(/<div class="meta">[\s\S]*?<\/div>/)?.[0];
const headline = landing.match(/<h1>[\s\S]*?<\/h1>/)?.[0];
if (!nav || !meta || !headline) throw new Error('index.html: the nav, the footer meta or the h1 is missing');
const docsCss = await Bun.file(join(site, 'docs.css')).text();

const escape = (s: string) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const plain = (html: string) =>
	html
		.replace(/<[^>]+>/g, '')
		.replace(/&quot;/g, '"')
		.replace(/&#39;/g, "'")
		.replace(/&lt;/g, '<')
		.replace(/&gt;/g, '>')
		.replace(/&amp;/g, '&');
// GitHub's heading ids, so links like config.md#docker-inside-jobs keep working.
const slugify = (text: string) => text.toLowerCase().replace(/[^\p{L}\p{N}\p{M}\s_-]/gu, '').replace(/\s/g, '-');
const summary = (text: string) => {
	if (text.length <= 160) return text;
	return `${text.slice(0, 157).replace(/[\s,;:]+\S*$/, '')}…`;
};

function link(href: string, from: string) {
	if (/^([a-z]+:|#)/i.test(href)) return href;
	const [path = '', fragment] = href.split('#');
	const hash = fragment ? `#${fragment}` : '';
	const target = posix.normalize(posix.join(posix.dirname(from), path));
	const doc = DOCS.find((d) => d.src === target && d.src !== 'README.md');
	if (doc) return docPath(doc) + hash;
	if (target === 'README.md') return REPO + hash;
	const full = join(root, target);
	if (!existsSync(full)) throw new Error(`${from} links to ${href}, which does not exist`);
	return `${REPO}/${statSync(full).isDirectory() ? 'tree' : 'blob'}/main/${target}${hash}`;
}

async function markdown(doc: Doc) {
	const text = await read(doc.src);
	if (doc.src !== 'README.md') return text;
	const section = (heading: string) => {
		const part = text.split(/^## /m).find((p) => p.startsWith(`${heading}\n`));
		if (!part) throw new Error(`README.md has no "## ${heading}" section`);
		return part.slice(heading.length + 1);
	};
	return `# Quickstart\n\n${section('Quickstart')}\n## Uninstall\n${section('Uninstall')}`;
}

const diagrams: string[] = [];
// Images the docs embed as HTML (<img src="docs/…">), served at the same path from the repository root.
const images = new Set<string>();

function image(src: string, from: string) {
	if (/^([a-z]+:|\/)/i.test(src)) return src;
	const target = posix.normalize(posix.join(posix.dirname(from), src));
	if (!existsSync(join(root, target))) throw new Error(`${from} shows ${src}, which does not exist`);
	images.add(target);
	return `/${target}`;
}

async function render(doc: Doc): Promise<Rendered> {
	const toc: Rendered['toc'] = [];
	const seen = new Map<string, number>();
	const marked = new Marked({
		gfm: true,
		walkTokens(token) {
			if (token.type === 'link') token.href = link(token.href, doc.src);
			// The README shows Cloudflare's button image; the site serves its own copy, so the CSP stays 'self'.
			if (token.type === 'image' && token.href === 'https://deploy.workers.cloudflare.com/button') token.href = '/deploy-to-cloudflare.svg';
			if (token.type === 'html') token.text = token.text.replace(/<img([^>]*?) src="([^"]+)"/g, (_, attrs: string, src: string) => `<img${attrs} src="${image(src, doc.src)}"`);
		},
		renderer: {
			heading({ tokens, depth }) {
				const inner = this.parser.parseInline(tokens);
				let id = slugify(plain(inner));
				const n = seen.get(id) ?? 0;
				seen.set(id, n + 1);
				if (n) id += `-${n}`;
				if (depth === 1) return `<h1 id="${id}">${inner}</h1>\n`;
				if (depth === 2) toc.push({ id, html: inner });
				const text = inner.includes('<a ') ? inner : `<a class="self" href="#${id}">${inner}</a>`;
				return `<h${depth} id="${id}">${text}</h${depth}>\n`;
			},
			code({ text, lang }) {
				if (lang === 'mermaid') {
					diagrams.push(text);
					return `<!--diagram:${diagrams.length - 1}-->\n`;
				}
				return `<pre tabindex="0"><code${lang ? ` class="language-${lang}"` : ''}>${escape(text)}</code></pre>\n`;
			},
		},
	});
	const body = (await marked.parse(await markdown(doc)))
		.replaceAll('<table>', '<div class="table" tabindex="0" role="region" aria-label="Table"><table>')
		.replaceAll('</table>', '</table></div>');
	const title = plain(body.match(/<h1[^>]*>([\s\S]*?)<\/h1>/)?.[1] ?? '');
	const description = summary(plain(body.match(/<p>([\s\S]*?)<\/p>/)?.[1] ?? '').replace(/\s+/g, ' '));
	if (!title || !description) throw new Error(`${doc.src} needs a "# Title" and a first paragraph`);
	return { doc, title, description, toc, body };
}

// Mermaid renders in Chromium with the docs' tokens and fonts. Its inline style attributes become
// classes in the diagram's own <style>, so the CSP never has to allow style attributes.
async function renderDiagrams(browser: Browser) {
	if (!diagrams.length) return [];
	const page = await browser.newPage();
	const geist = Buffer.from(await Bun.file(join(site, 'fonts/geist.woff2')).arrayBuffer()).toString('base64');
	await page.setContent(`<style>@font-face { font-family: "Geist"; src: url(data:font/woff2;base64,${geist}); font-weight: 400 600; }</style>`);
	await page.addScriptTag({ path: join(site, 'node_modules/mermaid/dist/mermaid.min.js') });
	await page.evaluate(() => document.fonts.load('14px Geist'));
	const t = tokens;
	const theme = {
		darkMode: true,
		fontFamily: '"Geist", "Geist Fallback", system-ui, sans-serif',
		fontSize: '14px',
		background: t.deep,
		mainBkg: t.panel,
		primaryColor: t.panel,
		primaryTextColor: t.ink,
		primaryBorderColor: t.faint,
		nodeBorder: t.faint,
		secondaryColor: t.panel,
		tertiaryColor: t.deep,
		lineColor: t.dim,
		textColor: t.ink,
		clusterBkg: t.night,
		clusterBorder: t.rule,
		titleColor: t.dim,
		edgeLabelBackground: t.deep,
	};
	const svgs = await page.evaluate(
		async ({ sources, theme }) => {
			const mermaid = (window as unknown as { mermaid: any }).mermaid;
			mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme: 'base', themeVariables: theme, htmlLabels: false, flowchart: { htmlLabels: false } });
			const out: string[] = [];
			for (const [i, source] of sources.entries()) {
				const { svg } = await mermaid.render(`diagram-${i}`, source);
				const doc = new DOMParser().parseFromString(svg, 'image/svg+xml');
				const root = doc.documentElement as unknown as SVGSVGElement;
				const classes = new Map<string, string>();
				for (const el of [root, ...root.querySelectorAll<SVGElement>('[style]')]) {
					const css = el.style.cssText;
					el.removeAttribute('style');
					if (!css) continue;
					if (!classes.has(css)) classes.set(css, `d${i}s${classes.size}`);
					el.classList.add(classes.get(css)!);
				}
				const style = root.querySelector('style')!;
				style.textContent += [...classes].map(([css, cls]) => `.${cls}{${css.replace(/;/g, ' !important;')}}`).join('');
				out.push(new XMLSerializer().serializeToString(root));
			}
			return out;
		},
		{ sources: diagrams, theme },
	);
	await page.close();
	// Inside inline SVG the parser decodes entities in <style>, and the CSP hashes what it decoded.
	return svgs.map((svg) =>
		svg.replace(/<style>([\s\S]*?)<\/style>/, (_, css: string) => {
			const raw = css.replace(/&gt;/g, '>');
			if (/[&<]/.test(raw)) throw new Error('a Mermaid stylesheet contains & or <, which would change its CSP hash');
			return `<style>${raw}</style>`;
		}),
	);
}

function head(o: { title: string; description: string; path: string | null }) {
	const url = o.path && ORIGIN + o.path;
	return `<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${escape(o.title)}</title>
<meta name="description" content="${escape(o.description)}">
${url ? `<link rel="canonical" href="${url}">` : '<meta name="robots" content="noindex">'}
<meta property="og:type" content="website">
<meta property="og:site_name" content="boxrunner">
<meta property="og:title" content="${escape(o.title)}">
<meta property="og:description" content="${escape(o.description)}">
${url ? `<meta property="og:url" content="${url}">\n` : ''}<meta property="og:image" content="${ORIGIN}/og.png">
<meta property="og:image:width" content="1200">
<meta property="og:image:height" content="630">
<meta name="twitter:card" content="summary_large_image">
<meta name="theme-color" content="${tokens.night}">
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
<link rel="apple-touch-icon" href="/apple-touch-icon.png">
<link rel="manifest" href="/manifest.webmanifest">
<link rel="preload" href="/fonts/bricolage-grotesque.woff2" as="font" type="font/woff2" crossorigin>
<link rel="preload" href="/fonts/geist.woff2" as="font" type="font/woff2" crossorigin>
<style>${base}${docsCss}</style>
`;
}

const footer = `<footer class="gutter"><div class="measure">${meta}</div></footer>\n`;
const docsNav = nav.replace('<a href="/docs/">', '<a href="/docs/" aria-current="page">');

function docPage(r: Rendered, all: Rendered[], svgs: string[]): Page {
	const side = all
		.map((o) => {
			const current = o === r;
			const toc = current && o.toc.length ? `<ul class="toc">${o.toc.map((h) => `<li><a href="#${h.id}">${h.html}</a></li>`).join('')}</ul>` : '';
			return `<li><a href="${docPath(o.doc)}"${current ? ' aria-current="page"' : ''}>${escape(o.title)}</a>${toc}</li>`;
		})
		.join('\n');
	const body = r.body.replace(/<!--diagram:(\d+)-->/g, (_, i: string) => `<div class="diagram" tabindex="0" role="region" aria-label="Diagram">${svgs[Number(i)]}</div>`);
	const path = docPath(r.doc);
	return {
		path,
		html: `${head({ title: `${r.title} · boxrunner docs`, description: r.description, path })}<a class="skip" href="#content">Skip to content</a>
${docsNav}
<div class="docs gutter"><div class="measure">
<nav class="side" aria-label="Docs"><p class="kicker">Docs</p><ul>
${side}
</ul></nav>
<main id="content"><article class="prose">
${body}</article>
<p class="source"><a href="${REPO}/blob/main/${r.doc.src}">Edit this page on GitHub</a></p></main>
</div></div>
${footer}`,
	};
}

const notFound: Page = {
	path: '/404.html',
	html: `${head({ title: 'Page not found · boxrunner', description: 'There is no page at this address.', path: null })}${nav}
<main class="lost gutter"><div class="measure">
<p class="kicker">404</p>
<h1>No page at this address.</h1>
<p>It may have moved, or the link has a typo.</p>
<p class="links"><a class="ghost" href="/">Back to the start</a><a class="ghost" href="/docs/">Read the docs</a></p>
</div></main>
${footer}`,
};

async function ogImage(browser: Browser, fonts: (html: string) => string) {
	const server = serve(dist);
	const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2, reducedMotion: 'reduce' });
	const page = await context.newPage();
	await page.goto(server.url.href);
	const box = await page.locator('.hero .stage canvas').evaluate((c: HTMLCanvasElement) => c.toDataURL('image/png'));
	const eyebrow = landing.match(/<div class="eyebrow">[\s\S]*?<\/div>/)?.[0] ?? '';
	const card = `<!doctype html><meta charset="utf-8"><style>${fonts(base)}
body { margin: 0; background: var(--night); color: var(--ink); }
.card { position: relative; width: 1200px; height: 630px; overflow: hidden; padding: 64px 72px; box-sizing: border-box; display: grid; grid-template-rows: auto 1fr auto; }
.box { position: absolute; right: -10px; top: 30px; width: 540px; height: 600px; object-fit: contain; }
.mark { font-size: 22px; gap: 14px; } .mark svg { width: 24px; height: 24px; }
.eyebrow { font: 500 17px/1 var(--f-mono); letter-spacing: .14em; text-transform: uppercase; color: var(--dim); display: flex; align-items: center; gap: 12px; }
.eyebrow .live { width: 10px; height: 10px; border-radius: 50%; background: var(--ok); }
h1 { position: relative; font: 800 100px/.92 var(--f-display); letter-spacing: -0.035em; margin: 0; max-width: 680px; align-self: center; }
h1 .w { display: inline-block; }
h1 em { font-style: normal; color: transparent; -webkit-text-stroke: 2px var(--ink); }
</style>
<div class="card"><span class="mark">${mark}boxrunner</span>${headline}${eyebrow}<img class="box" src="${box}" alt=""></div>`;
	await page.route('**/og-card', (route) => route.fulfill({ contentType: 'text/html', body: card }));
	await page.setViewportSize({ width: 1200, height: 630 });
	await page.goto(new URL('/og-card', server.url).href);
	await page.evaluate(() => document.fonts.ready);
	await page.locator('.card').screenshot({ path: join(dist, 'og.png'), scale: 'css' });
	await context.close();
	server.stop(true);
}

rmSync(dist, { recursive: true, force: true });
mkdirSync(join(dist, 'fonts'), { recursive: true });

// Fonts get a content hash in their name, so they can be cached for a year.
const fontNames = new Map<string, string>();
for (const name of readdirSync(join(site, 'fonts'))) {
	const bytes = await Bun.file(join(site, 'fonts', name)).bytes();
	const hashed = name.endsWith('.woff2') ? name.replace(/\.woff2$/, `.${createHash('sha256').update(bytes).digest('hex').slice(0, 10)}.woff2`) : name;
	await Bun.write(join(dist, 'fonts', hashed), bytes);
	fontNames.set(`/fonts/${name}`, `/fonts/${hashed}`);
}
const withFonts = (html: string) => [...fontNames].reduce((out, [from, to]) => out.replaceAll(from, to), html);
for (const icon of ['favicon.svg', 'apple-touch-icon.png', 'icon-192.png', 'icon-512.png']) cpSync(join(site, icon), join(dist, icon));
cpSync(join(site, 'deploy-to-cloudflare.svg'), join(dist, 'deploy-to-cloudflare.svg'));

const browser = await chromium.launch();
try {
	const rendered: Rendered[] = [];
	// One at a time, so diagrams are numbered the same way on every build.
	for (const doc of DOCS) rendered.push(await render(doc));
	for (const target of images) cpSync(join(root, target), join(dist, target));
	const svgs = await renderDiagrams(browser);
	const pages: Page[] = [{ path: '/', html: landing }, ...rendered.map((r) => docPage(r, rendered, svgs)), notFound].map((p) => ({ ...p, html: withFonts(p.html) }));
	for (const page of pages) {
		const file = page.path.endsWith('/') ? `${page.path}index.html` : page.path;
		await Bun.write(join(dist, file), page.html);
	}

	const hashes = (tag: string) => {
		const set = new Set<string>();
		for (const { html } of pages)
			for (const m of html.matchAll(new RegExp(`<${tag}>([\\s\\S]*?)</${tag}>`, 'g'))) set.add(`'sha256-${createHash('sha256').update(m[1]!).digest('base64')}'`);
		return [...set].join(' ');
	};
	for (const { path, html } of pages)
		if (/<(script|style)\s[^>]*>/.test(html)) throw new Error(`${path}: an inline <script> or <style> with attributes would escape the CSP hashes`);
	const csp = [
		"default-src 'none'",
		`script-src ${hashes('script') || "'none'"}`,
		`style-src ${hashes('style')}`,
		"font-src 'self'",
		"img-src 'self'",
		`connect-src 'self' ${STATS_ORIGIN}`,
		"manifest-src 'self'",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
		'upgrade-insecure-requests',
	].join('; ');
	const permissions = ['accelerometer', 'autoplay', 'browsing-topics', 'camera', 'clipboard-read', 'display-capture', 'encrypted-media', 'fullscreen', 'geolocation', 'gyroscope', 'hid', 'idle-detection', 'magnetometer', 'microphone', 'midi', 'payment', 'picture-in-picture', 'publickey-credentials-get', 'screen-wake-lock', 'serial', 'usb', 'xr-spatial-tracking']
		.map((f) => `${f}=()`)
		.concat('clipboard-write=(self)')
		.join(', ');
	const html = 'public, max-age=0, must-revalidate';
	await Bun.write(
		join(dist, '_headers'),
		`/*
  Content-Security-Policy: ${csp}
  Strict-Transport-Security: max-age=63072000; includeSubDomains; preload
  X-Content-Type-Options: nosniff
  X-Frame-Options: DENY
  Referrer-Policy: strict-origin-when-cross-origin
  Permissions-Policy: ${permissions}
/
  Cache-Control: ${html}
/docs/*
  Cache-Control: ${html}
/fonts/*
  Cache-Control: public, max-age=31536000, immutable
/install.sh
  Content-Type: text/plain; charset=utf-8
  Cache-Control: public, max-age=300
/v*/install.sh
  Content-Type: text/plain; charset=utf-8
  Cache-Control: public, max-age=31536000, immutable
`,
	);

	const listed = pages.filter((p) => p.path.endsWith('/'));
	await Bun.write(
		join(dist, 'sitemap.xml'),
		`<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
${listed.map((p) => `  <url><loc>${ORIGIN}${p.path}</loc></url>`).join('\n')}
</urlset>
`,
	);
	await Bun.write(
		join(dist, 'manifest.webmanifest'),
		JSON.stringify({
			name: 'boxrunner',
			icons: [192, 512].map((size) => ({ src: `/icon-${size}.png`, sizes: `${size}x${size}`, type: 'image/png' })),
			theme_color: tokens.night,
			background_color: tokens.night,
			display: 'browser',
		}),
	);
	await Bun.write(join(dist, 'robots.txt'), `User-agent: *\nAllow: /\n\nSitemap: ${ORIGIN}/sitemap.xml\n`);
	await ogImage(browser, withFonts);
	console.log(`Built ${pages.length} pages, ${diagrams.length} diagram(s) and og.png into ${dist}`);
} finally {
	await browser.close();
}
