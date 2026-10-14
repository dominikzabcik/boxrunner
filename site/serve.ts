// Serves dist/ the way Workers static assets will: `_headers` applied, `/docs/x` redirected to
// `/docs/x/`, unknown paths answered with 404.html, and text compressed as Cloudflare would.
import { existsSync, readFileSync, statSync } from 'node:fs';
import { join, normalize } from 'node:path';

type Rule = { pattern: RegExp; set: [string, string][]; detach: string[] };

function parseHeaders(text: string): Rule[] {
	const rules: Rule[] = [];
	for (const line of text.split('\n')) {
		if (!line.trim() || line.trimStart().startsWith('#')) continue;
		if (!/^\s/.test(line)) {
			const source = line.trim().replace(/[.+?^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '.*');
			rules.push({ pattern: new RegExp(`^${source}$`), set: [], detach: [] });
			continue;
		}
		const rule = rules.at(-1);
		if (!rule) throw new Error(`_headers: header before any path: ${line}`);
		const detach = line.trim().match(/^!\s*(.+)$/);
		if (detach) rule.detach.push(detach[1]!.toLowerCase());
		else {
			const at = line.indexOf(':');
			rule.set.push([line.slice(0, at).trim(), line.slice(at + 1).trim()]);
		}
	}
	return rules;
}

const types: Record<string, string> = {
	html: 'text/html; charset=utf-8',
	xml: 'application/xml; charset=utf-8',
	txt: 'text/plain; charset=utf-8',
	svg: 'image/svg+xml',
	png: 'image/png',
	woff2: 'font/woff2',
};

export function serve(dir: string, port = 0) {
	const rules = parseHeaders(existsSync(join(dir, '_headers')) ? readFileSync(join(dir, '_headers'), 'utf8') : '');
	const file = (path: string) => {
		const full = normalize(join(dir, path));
		return full.startsWith(dir) && existsSync(full) && statSync(full).isFile() ? full : null;
	};
	return Bun.serve({
		port,
		hostname: '127.0.0.1',
		async fetch(req) {
			const { pathname } = new URL(req.url);
			const headers = new Headers();
			for (const rule of rules) {
				if (!rule.pattern.test(pathname)) continue;
				for (const name of rule.detach) headers.delete(name);
				for (const [name, value] of rule.set) headers.append(name, value);
			}
			let found = pathname.endsWith('/') ? file(join(pathname, 'index.html')) : file(pathname);
			let status = 200;
			if (!found && !pathname.endsWith('/') && file(join(pathname, 'index.html'))) {
				headers.set('location', `${pathname}/`);
				return new Response(null, { status: 307, headers });
			}
			if (!found || pathname === '/_headers') {
				found = file('404.html')!;
				status = 404;
			}
			const type = types[found.split('.').pop()!] ?? 'application/octet-stream';
			headers.set('content-type', type);
			let body: Uint8Array = await Bun.file(found).bytes();
			if (/^(text|application\/xml|image\/svg)/.test(type) && /\bgzip\b/.test(req.headers.get('accept-encoding') ?? '')) {
				body = Bun.gzipSync(body);
				headers.set('content-encoding', 'gzip');
			}
			return new Response(body, { status, headers });
		},
	});
}

if (import.meta.main) {
	const server = serve(join(import.meta.dir, 'dist'), Number(process.env.PORT ?? 8788));
	console.log(`Serving dist/ at ${server.url}`);
}
