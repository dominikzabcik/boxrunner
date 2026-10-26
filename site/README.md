# boxrunner.zabcik.me

The landing page and the docs. `index.html` is the landing page as it ships, with its CSS and JavaScript inline. The docs pages are rendered from `../docs/*.md` and the Quickstart and Uninstall sections of `../README.md`, so edit those files, not anything here.

## Build

```sh
bun install
bun run build
```

`build.ts` writes `dist/`:

- `index.html`, `docs/<name>/index.html` for each doc (Quickstart at `docs/index.html`) and `404.html`. The docs and 404 pages reuse the landing page's tokens, fonts, nav and footer line: the build takes the stylesheet up to the "shared" marker in `index.html` and adds `docs.css`. Everything uses the Paper design: laid paper, Castoro for display type, Instrument Sans for text, and a light and a dark theme that follow `prefers-color-scheme`.
- `fonts/`, each `.woff2` renamed with a content hash so it can be cached for a year, and `paper.svg`, the grain behind the page.
- `og.png`, rendered in Chromium from the landing page's headline and its engraved hero plate (drawn with reduced motion, so the image is the same on every build). Mermaid diagrams become inline SVG the same way, once for each theme.
- `_headers` with a Content-Security-Policy that allows each inline `<script>` and `<style>` by its SHA-256 hash, plus `robots.txt` and `sitemap.xml`.

The build needs Chromium for Playwright 1.63: `bunx playwright-core@1.63.0 install chromium` once, if it is not installed yet.

Inline `style="…"` attributes and inline event handlers are blocked by the CSP. Put styles in the stylesheet; JavaScript may still set `element.style`.

## Check

```sh
bun run check
```

Builds, serves `dist/` with `serve.ts` (which applies `_headers` and answers like Workers static assets) and checks every page at 1280 and 390px wide, in the light and the dark theme, in Chromium: no console errors or warnings, no CSP violations, nothing wider than a phone screen, every internal link and fragment resolves, no headline runs past two lines at 390 and 360px, the hero canvas draws, and the landing page stays under its byte budget (see `check.ts`). It also confirms that the CSP actually blocks an injected script, and that the minutes calculator reads right with scripts off, adds up with them on, answers the keyboard and links every rate to GitHub's billing docs. `bun run serve` serves the last build at http://127.0.0.1:8788.

## Fonts

Castoro (regular and italic), Instrument Sans and Geist Mono, all under the SIL Open Font License (`fonts/OFL-*.txt`). `fonts.sh` rebuilds them from Google Fonts' sources at a pinned commit: the latin subset plus the Czech letters and the symbols the page uses, limited to the weights the site sets (Castoro at 400 only). It needs Python with `fonttools` and `brotli`.

## Deploy (later)

`wrangler.jsonc` describes an assets-only Worker named `boxrunner-site` that serves `dist/`: `/docs/config` redirects to `/docs/config/`, and unknown paths get `404.html` with status 404. It has no route or custom domain yet. When it is time:

```sh
bun run build
bunx wrangler@4.148.0 deploy
```

then add `boxrunner.zabcik.me` as a custom domain on the Worker. `install.sh` is published with each release and is not part of this build.
