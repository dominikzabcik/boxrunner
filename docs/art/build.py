#!/usr/bin/env python3
"""Draws the README's images from the site's fonts and colors.

    python3 docs/art/build.py

Text is shaped with HarfBuzz (hb-shape) and written as outlines, so GitHub
shows it in the site's faces without loading a font. Needs Python with
fonttools and brotli (pip3 install fonttools brotli) and hb-shape (HarfBuzz).

Also draws the logo, the site's favicon and app icons and the repository's
social preview. The PNGs are screenshots from headless Chrome, found on the
PATH or at the path in $CHROME.
"""

import json
import math
import os
import random
import shutil
import subprocess
import tempfile
import urllib.parse
from pathlib import Path

from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.ttLib import TTFont
from fontTools.varLib import instancer

ART = Path(__file__).resolve().parent
SITE = ART.parent.parent / "site"
WORKER = ART.parent.parent / "worker" / "src"
FONTS = SITE / "fonts"
TMP = Path(tempfile.mkdtemp())

C = {
    "night": "#070a10",
    "deep": "#0c1119",
    "panel": "#111824",
    "rule": "#1f2a3a",
    "ink": "#edf1f7",
    "dim": "#8c99ab",
    "faint": "#56637a",
    "normal": "#5aa7ff",
    "idle": "#f5b54a",
    "key": "#ff6a3d",
    "ok": "#45d48a",
}


def num(v):
    s = f"{v:.1f}"
    return s[:-2] if s.endswith(".0") else s


class Face:
    def __init__(self, file, **axes):
        font = TTFont(FONTS / file)
        font.flavor = None
        font = instancer.instantiateVariableFont(font, axes)
        self.file = TMP / f"{file}-{'-'.join(f'{k}{v}' for k, v in axes.items())}.ttf"
        font.save(self.file)
        self.font = TTFont(self.file)
        self.glyphs = self.font.getGlyphSet()
        self.order = self.font.getGlyphOrder()
        self.upem = self.font["head"].unitsPerEm
        global FACES
        self.key = "abcdefgh"[FACES]
        FACES += 1

    def shape(self, text):
        out = subprocess.run(
            ["hb-shape", "--output-format=json", "--no-glyph-names", f"--font-file={self.file}", "--", text],
            capture_output=True, text=True, check=True,
        ).stdout
        return json.loads(out)

    def width(self, text, size, tracking=0.0):
        run = self.shape(text)
        return sum(g["ax"] for g in run) * size / self.upem + tracking * size * len(run)

    def path(self, text, size, x, y, tracking=0.0):
        """Draws text whose baseline starts at (x, y) as uses of shared glyphs; returns (markup, advance)."""
        scale = size / self.upem
        uses = []
        pos = 0.0
        for g in self.shape(text):
            gx = x + (pos + g.get("dx", 0)) * scale
            gy = y - g.get("dy", 0) * scale
            if self.outline(g["g"]):
                uses.append(f'<use href="#{self.glyph_id(g["g"])}" transform="translate({num(gx)} {num(gy)}) scale({scale:.4g} {-scale:.4g})"/>')
            pos += g["ax"] + tracking * self.upem
        return "".join(uses), pos * scale

    def glyph_id(self, gid):
        return f"{self.key}{gid}"

    def outline(self, gid):
        """The glyph's outline in font units, registered once per image; empty for a space."""
        name = self.glyph_id(gid)
        if name not in USED:
            pen = SVGPathPen(self.glyphs, ntos=lambda v: str(round(v)))
            self.glyphs[self.order[gid]].draw(pen)
            USED[name] = pen.getCommands()
        return USED[name]


USED = {}
FACES = 0


display = Face("bricolage-grotesque.woff2", wght=800, opsz=96)
ui = Face("geist.woff2", wght=400)
ui_bold = Face("geist.woff2", wght=600)
mono = Face("geist-mono.woff2", wght=400)
mono_bold = Face("geist-mono.woff2", wght=500)


def text(face, s, size, x, y, fill, tracking=0.0, anchor="start", stroke=None, extra=""):
    if anchor != "start":
        w = face.width(s, size, tracking)
        x -= w if anchor == "end" else w / 2
    uses, w = face.path(s, size, x, y, tracking)
    paint = (
        f'fill="none" stroke="{stroke[0]}" stroke-width="{stroke[1] * face.upem / size:.4g}"' if stroke else f'fill="{fill}"'
    )
    return f"<g {paint} {extra}>{uses}</g>", w


def runs(parts, size, x, y):
    """Draws (face, text, color) parts one after another on a line."""
    out = []
    for face, s, fill in parts:
        p, w = text(face, s, size, x, y, fill)
        out.append(p)
        x += w
    return "".join(out), x


def document(w, h, title, body, style="", background=None):
    """A standalone SVG with its glyphs, after which the next image starts with none."""
    if background is None:
        background = f'<rect width="{w}" height="{h}" rx="18" fill="{C["night"]}"/>'
    defs = "".join(f'<path id="{k}" d="{d}"/>' for k, d in USED.items() if d)
    USED.clear()
    return (
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {num(w)} {h}" width="{num(w)}" height="{h}" role="img">'
        f"<title>{title}</title>"
        + (f"<style>{style}</style>" if style else "")
        + (f"<defs>{defs}</defs>" if defs else "")
        + f"{background}{body}</svg>\n"
    )


def write(path, doc):
    path.write_text(doc)
    print(f"{path.relative_to(ART.parent.parent)}: {len(doc.encode()) / 1024:.1f} kB")


def svg(name, w, h, title, body, style=""):
    write(ART / name, document(w, h, title, body, style))


# The mark, on a 16-unit grid so that a 16 px favicon lands on whole pixels: the box
# you already own, the main job's container in it, and idle jobs in the room it leaves.
BOX = "M2 2h12v12H2zM3 3v10h10V3z"
MAIN = "M4 4h5v5H4z"
IDLE_CELLS = [(10, 4), (10, 7), (4, 10), (7, 10), (10, 10)]
IDLE = "".join(f"M{x} {y}h2v2h-2z" for x, y in IDLE_CELLS)


def filled(color):
    return f'fill="{color}"'


def mark_paths(box):
    """The mark on its grid; box paints the box, with a fill or a class."""
    return f'<path d="{BOX}" fill-rule="evenodd" {box}/><path d="{MAIN}" {filled(C["normal"])}/><path d="{IDLE}" {filled(C["idle"])}/>'


def mark(x, y, size, box):
    """The mark's 12 units, from (2, 2) to (14, 14), drawn size wide with its corner at (x, y)."""
    return f'<g transform="translate({num(x)} {num(y)}) scale({size / 12:.4g}) translate(-2 -2)">{mark_paths(box)}</g>'


EDGES = [(0, 1), (1, 3), (3, 2), (2, 0), (4, 5), (5, 7), (7, 6), (6, 4), (0, 4), (1, 5), (2, 6), (3, 7)]


def edge_point(e, t, h, c):
    """A point t of the way along edge e of a box with half-sizes h around c, as the site draws it."""
    a, b = EDGES[e]
    out = []
    for k in range(3):
        A = (1 if a >> k & 1 else -1) * h[k]
        B = (1 if b >> k & 1 else -1) * h[k]
        out.append(c[k] + A + (B - A) * t)
    return out


def projector(cx, cy, scale, yaw=0.62, pitch=0.5, distance=4.2):
    cy_, sy, cp, sp = math.cos(yaw), math.sin(yaw), math.cos(pitch), math.sin(pitch)

    def project(x, y, z):
        x1, z1 = x * cy_ + z * sy, -x * sy + z * cy_
        y2, z2 = y * cp - z1 * sp, y * sp + z1 * cp
        k = distance / (distance + z2)
        return cx + x1 * k * scale, cy + y2 * k * scale

    return project


def dots(points):
    return "".join(f"M{num(x)} {num(y)}h0" for x, y in points)


def hero():
    W, H = 1200, 540
    body = [mark(56, 48, 18, filled(C["ink"]))]
    p, _ = text(mono_bold, "boxrunner", 19, 86, 64, C["ink"])
    body.append(p)
    p, _ = text(mono, "boxrunner.zabcik.me", 15, W - 56, 63, C["dim"], anchor="end")
    body.append(p)

    body.append(f'<circle cx="62" cy="130" r="9" fill="{C["ok"]}" opacity=".18"/><circle cx="62" cy="130" r="4" fill="{C["ok"]}"/>')
    p, _ = text(mono, "SELF-HOSTED GITHUB ACTIONS RUNNERS", 13, 80, 135, C["dim"], tracking=0.16)
    body.append(p)

    size, lead, top = 86, 79, 222
    p, w = text(display, "CI on the box", size, 52, top, C["ink"], tracking=-0.035)
    body.append(p)
    p, w = text(display, "you ", size, 52, top + lead, C["ink"], tracking=-0.035)
    body.append(p)
    p, _ = text(display, "already", size, 52 + w, top + lead, None, tracking=-0.035, stroke=(C["ink"], 1.6))
    body.append(p)
    p, _ = text(display, "pay for.", size, 52, top + 2 * lead, C["ink"], tracking=-0.035)
    body.append(p)

    line, _ = runs([(ui, "Every job gets a ", C["dim"]), (ui_bold, "fresh container", C["ink"]), (ui, ". Your GitHub", C["dim"])], 21, 56, 432)
    body.append(line)
    line, _ = runs([(ui, "App key ", C["dim"]), (ui_bold, "never touches the box", C["ink"]), (ui, ".", C["dim"])], 21, 56, 462)
    body.append(line)

    status, _ = runs(
        [
            (mono, "4 cores  ", C["dim"]),
            (mono_bold, "1", C["normal"]),
            (mono, " normal  ", C["dim"]),
            (mono_bold, "3", C["idle"]),
            (mono, " idle  ", C["dim"]),
            (mono, "queue to start ", C["dim"]),
            (mono_bold, "6 to 12 s", C["ink"]),
        ],
        13, 56, 506,
    )
    body.append(status)

    # The site's hero, held still: a box drawn in particles with four job
    # containers inside, one normal-priority (blue) and three idle (amber). Each
    # forms out of scattered particles, runs and dissolves, on the site's timings.
    project = projector(W - 310, 282, 140)
    rnd = random.Random(20261013)
    dust = []
    for _ in range(170):
        u, th = rnd.uniform(-1, 1), rnd.uniform(0, math.tau)
        r, s = rnd.uniform(1.25, 2.3), math.sqrt(1 - u * u)
        x, y = project(s * math.cos(th) * r, u * r * 0.7, s * math.sin(th) * r)
        if 660 < x < W - 20 and 20 < y < H - 20:
            dust.append((x, y, rnd.random()))
    for group, (lo, hi) in enumerate([(0, 0.5), (0.5, 1)]):
        pts = [(x, y) for x, y, k in dust if lo <= k < hi]
        body.append(f'<path class="t t{group}" d="{dots(pts)}" stroke="{C["idle"]}" stroke-width="2.2" stroke-linecap="round" opacity=".28"/>')
    shell = [project(*edge_point(e, i / 40, (1, 1, 1), (0, 0, 0))) for e in range(12) for i in range(40)]
    body.append(f'<path d="{dots(shell)}" stroke="{C["ink"]}" stroke-width="2.6" stroke-linecap="round" opacity=".5"/>')
    slots = [(-0.48, 0.48, -0.48), (0.48, 0.48, -0.48), (-0.48, 0.48, 0.48), (0.48, 0.48, 0.48)]
    for j, centre in enumerate(slots):
        pts = [project(*edge_point(e, i / 14, (0.3, 0.3, 0.3), centre)) for e in range(12) for i in range(14)]
        ox, oy = project(*centre)
        period = 8.5 if j == 0 else 4.6 + j * 0.9
        color = C["normal"] if j == 0 else C["idle"]
        body.append(
            f'<path class="c" style="animation-duration:{period}s;animation-delay:-{1.5 + j * 1.7:.1f}s;transform-origin:{ox:.1f}px {oy:.1f}px" '
            f'd="{dots(pts)}" stroke="{color}" stroke-width="3.6" stroke-linecap="round"/>'
        )

    style = (
        ".c{animation:life 6s linear infinite}"
        "@keyframes life{0%{opacity:0;transform:scale(1.9)}12%{opacity:1;transform:scale(1)}"
        "78%{opacity:1;transform:scale(1)}100%{opacity:0;transform:scale(2.2)}}"
        ".t{animation:twinkle 5s ease-in-out infinite alternate}.t1{animation-delay:-2.5s}"
        "@keyframes twinkle{from{opacity:.1}to{opacity:.38}}"
        "@media (prefers-reduced-motion:reduce){.c,.t{animation:none}}"
    )
    svg("hero.svg", W, H, "boxrunner: CI on the box you already pay for", "".join(body), style)


def bench():
    W = 1200
    groups = [
        ("4-core ARM VPS", [428, 429, 2096]),
        ("4-core x86 VPS", [1393, 1438, 6905]),
    ]
    labels = ["alone", "beside 4 busy idle-priority jobs", "beside 4 busy normal-priority jobs"]
    colors = [C["dim"], C["idle"], C["normal"]]
    body = []
    p, _ = text(mono, "BOXRUNNER BENCH", 13, 56, 64, C["dim"], tracking=0.16)
    body.append(p)
    p, _ = text(ui_bold, "4 threads of CPU work in a normal-priority job", 26, 56, 104, C["ink"])
    body.append(p)
    p, _ = text(ui, "Idle jobs use only the cores your main job leaves free.", 18, 56, 136, C["dim"])
    body.append(p)

    label_w, bar_x, bar_max = 380, 56 + 380, W - 56 - 380 - 220
    y = 200
    n = 0
    for name, values in groups:
        p, _ = text(mono_bold, name, 14, 56, y, C["ink"])
        body.append(p)
        body.append(f'<line x1="56" y1="{y + 14}" x2="{W - 56}" y2="{y + 14}" stroke="{C["rule"]}"/>')
        y += 48
        top = max(values)
        for label, value, color in zip(labels, values, colors):
            p, _ = text(ui, label, 17, 56, y + 5, C["dim"])
            body.append(p)
            length = max(4, value / top * bar_max)
            body.append(
                f'<rect class="b" style="animation-delay:{0.15 * n:.2f}s" x="{bar_x}" y="{y - 13}" width="{length:.1f}" height="24" rx="4" fill="{color}"/>'
            )
            shown = f"{value} ms" if value < 1000 else f"{value / 1000:.2f} s"
            change = "" if value == values[0] else f"  +{round((value / values[0] - 1) * 100)}%"
            p, w = text(mono_bold, shown, 15, bar_x + length + 14, y + 5, C["ink"])
            body.append(p)
            if change:
                p, _ = text(mono, change, 15, bar_x + length + 14 + w, y + 5, color if color != C["dim"] else C["dim"])
                body.append(p)
            y += 40
            n += 1
        y += 36
    H = y + 4
    p, _ = text(mono, "median of 3 runs; run sudo boxrunner bench on your own box", 13, 56, H - 34, C["faint"])
    body.append(p)
    H += 6
    style = (
        ".b{transform-box:fill-box;transform-origin:left;animation:grow 1.1s cubic-bezier(.2,.8,.2,1) both}"
        "@keyframes grow{from{transform:scaleX(0)}}"
        "@media (prefers-reduced-motion:reduce){.b{animation:none}}"
    )
    svg("bench.svg", W, H, "boxrunner bench: idle jobs leave the main job's speed alone", "".join(body), style)


def node(x, y, w, h, title, sub, holds, color):
    out = [f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="14" fill="{C["deep"]}" stroke="{C["rule"]}" stroke-width="1.5"/>']
    p, _ = text(ui_bold, title, 22, x + 24, y + 42, C["ink"])
    out.append(p)
    p, _ = text(mono, sub, 13, x + 24, y + 66, C["dim"])
    out.append(p)
    out.append(f'<circle cx="{x + 29}" cy="{y + h - 30}" r="5" fill="{color}"/>')
    p, _ = text(mono_bold, holds, 13, x + 42, y + h - 25, color)
    out.append(p)
    return "".join(out)


def flow():
    W, H = 1200, 660
    nw, nh = 300, 132
    box, worker, github, job = (56, 290), (450, 110), (W - 56 - nw, 290), (450, 480)
    body = []
    p, _ = text(mono, "HOW IT WORKS", 13, 56, 58, C["dim"], tracking=0.16)
    body.append(p)

    def mid(n, side):
        x, y = n
        return {"l": (x, y + nh / 2), "r": (x + nw, y + nh / 2), "t": (x + nw / 2, y), "b": (x + nw / 2, y + nh)}[side]

    def elbow(a, b, vertical_first):
        (x1, y1), (x2, y2) = a, b
        corner = (x1, y2) if vertical_first else (x2, y1)
        return f"M{x1} {y1} Q{corner[0]} {corner[1]} {x2} {y2}"

    edges = [
        ("e1", elbow(mid(box, "t"), mid(worker, "l"), True), C["idle"]),
        ("e2", elbow(mid(worker, "r"), mid(github, "t"), False), C["key"]),
        ("e3", "M{} {} L{} {}".format(*mid(box, "r"), *mid(github, "l")), C["normal"]),
        ("e4", elbow(mid(box, "b"), mid(job, "l"), True), C["ok"]),
        ("e5", elbow(mid(job, "r"), mid(github, "b"), False), C["ok"]),
    ]
    for eid, d, _ in edges:
        dash = ' stroke-dasharray="2 7" stroke-linecap="round"' if eid == "e3" else ""
        body.append(f'<path id="{eid}" d="{d}" fill="none" stroke="{C["rule"]}" stroke-width="2"{dash}/>')

    body.append(node(*box, nw, nh, "Your box", "boxrunner agent, no open port", "box token", C["idle"]))
    body.append(node(*worker, nw, nh, "Your Worker", "Cloudflare, free plan", "GitHub App key", C["key"]))
    body.append(node(*github, nw, nh, "GitHub", "Actions queue", "scale set per class", C["normal"]))
    body.append(node(*job, nw, nh, "Job container", "fresh for every job", "one-job runner config", C["ok"]))

    def step(n, lines, x, y, anchor="start"):
        cx = x if anchor == "start" else x
        out = [f'<circle cx="{cx}" cy="{y - 5}" r="12" fill="{C["panel"]}" stroke="{C["rule"]}"/>']
        p, _ = text(mono_bold, str(n), 13, cx, y, C["ink"], anchor="middle")
        out.append(p)
        lx = x + 22 if anchor == "start" else x - 22
        for i, line in enumerate(lines):
            p, _ = text(mono, line, 13, lx, y + i * 20, C["dim"], anchor=anchor)
            out.append(p)
        return "".join(out)

    left, right = box[0] + nw / 2 + 28, W - 56 - nw / 2 - 28
    body.append(step(1, ["open a session", "with the box token"], left, 222))
    body.append(step(2, ["create the scale set", "with the App key"], right, 222, anchor="end"))
    body.append(step(3, ["long-poll the queue with a session token"], W / 2 - 170, box[1] + nh / 2 - 18))
    body.append(step(4, ["fetch one runner config", "and start a container"], left, 470))
    body.append(step(5, ["run exactly one job,", "then it is removed"], right, 470, anchor="end"))

    pulses = []
    for i, (eid, d, color) in enumerate(edges):
        pulses.append(f'<circle class="p" r="6" fill="{color}" style="offset-path:path(\'{d}\');animation-delay:{i * 2}s"/>')
    body.append("".join(pulses))
    style = (
        ".p{opacity:0;animation:pulse 10s linear infinite}"
        "@keyframes pulse{0%{offset-distance:0%;opacity:0}1%{opacity:1}14%{offset-distance:100%;opacity:1}15%,100%{offset-distance:100%;opacity:0}}"
        "@media (prefers-reduced-motion:reduce){.p{animation:none}}"
    )
    svg("flow.svg", W, H, "How boxrunner gets a job onto your box", "".join(body), style)


ICONS = {
    "container": f'<rect x="5" y="5" width="22" height="22" fill="none" stroke="{C["normal"]}" stroke-width="2.4"/><rect x="11" y="11" width="10" height="10" fill="{C["normal"]}"/>',
    "key": f'<circle cx="11" cy="16" r="6" fill="none" stroke="{C["key"]}" stroke-width="2.4"/><path d="M17 16h11M24 16v5M28 16v4" fill="none" stroke="{C["key"]}" stroke-width="2.4" stroke-linecap="round"/>',
    "priority": f'<rect x="4" y="6" width="24" height="8" rx="2" fill="{C["normal"]}"/><rect x="4" y="18" width="14" height="8" rx="2" fill="{C["idle"]}"/>',
    "speed": f'<circle cx="16" cy="17" r="11" fill="none" stroke="{C["ok"]}" stroke-width="2.4"/><path d="M16 17l6-6M13 3h6" fill="none" stroke="{C["ok"]}" stroke-width="2.4" stroke-linecap="round"/>',
    "light": f'<rect x="4" y="9" width="24" height="16" rx="3" fill="none" stroke="{C["dim"]}" stroke-width="2.4"/><circle cx="10" cy="17" r="2.5" fill="{C["ok"]}"/><path d="M16 17h8" stroke="{C["dim"]}" stroke-width="2.4" stroke-linecap="round"/>',
    "shield": f'<path d="M16 4l11 4v8c0 7-5 11-11 13C10 27 5 23 5 16V8z" fill="none" stroke="{C["ok"]}" stroke-width="2.4" stroke-linejoin="round"/><path d="M11 16l4 4 6-7" fill="none" stroke="{C["ok"]}" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"/>',
}


def icons():
    for name, shape in ICONS.items():
        doc = f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32" aria-hidden="true">{shape}</svg>\n'
        (ART / f"icon-{name}.svg").write_text(doc)


def terminal():
    """Recolors svg-term's render of install.cast in the site's palette.

    npx svg-term-cli@2.1.1 --in docs/art/install.cast --out docs/art/install.svg --window --width 92 --height 29 --padding 12
    """
    file = ART / "install.svg"
    doc = file.read_text()
    for theirs, ours in {"#282d35": C["night"], "#b9c0cb": C["ink"], "#a8cc8c": C["ok"], "#6f7683": C["faint"]}.items():
        doc = doc.replace(theirs, ours)
    doc = doc.replace('rx="5" ry="5"', 'rx="18" ry="18"')
    file.write_text(doc)


def chrome():
    for found in [os.environ.get("CHROME"), shutil.which("google-chrome"), shutil.which("chromium"), "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"]:
        if found and Path(found).exists():
            return found
    raise SystemExit("build.py: no Chrome found; set CHROME to its binary")


def png(doc, w, h, path):
    """Screenshots an SVG at w by h CSS pixels, transparent where it draws nothing."""
    src = TMP / f"{path.stem}.svg"
    src.write_text(doc)
    page = TMP / f"{path.stem}.html"
    page.write_text(f'<!doctype html><style>html,body{{margin:0;background:transparent}}img{{display:block}}</style><img src="{src.name}" width="{w}" height="{h}">')
    subprocess.run(
        [
            chrome(), "--headless", "--disable-gpu", "--hide-scrollbars", "--force-device-scale-factor=1",
            "--default-background-color=00000000", f"--user-data-dir={TMP / 'chrome'}",
            f"--window-size={w},{h}", f"--screenshot={path}", page.as_uri(),
        ],
        check=True, capture_output=True,
    )
    print(f"{path.relative_to(ART.parent.parent)}: {path.stat().st_size / 1024:.1f} kB")


def tile(rounded):
    corner = ' rx="3.5"' if rounded else ""
    return (
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><rect width="16" height="16"{corner} {filled(C["night"])}/>'
        f'{mark_paths(filled(C["ink"]))}</svg>\n'
    )


def lockup(fg, x=0.0, y=0.0):
    """The mark beside the name in Geist Mono, as the site's header sets it; returns (markup, width)."""
    size, h, gap = 30, 36, 20
    cap = mono_bold.font["OS/2"].sCapHeight / mono_bold.upem * size
    words, w = text(mono_bold, "boxrunner", size, x + h + gap, y + h / 2 + cap / 2, fg)
    return mark(x, y, h, filled(fg)) + words, h + gap + w


def particle_mark(x, y, size):
    """The mark drawn in particles, the way the site draws its box."""
    unit = size / 12
    pitch = unit / 2.5

    def at(u, v):
        return x + (u - 2) * unit, y + (v - 2) * unit

    def fill(u, v, w, h):
        n, m = round(w * 2.5), round(h * 2.5)
        return [at(u + (i + 0.5) * w / n, v + (j + 0.5) * h / m) for i in range(n) for j in range(m)]

    edge = round(11 * unit / pitch)
    corners = [(2.5, 2.5), (13.5, 2.5), (13.5, 13.5), (2.5, 13.5)]
    shell = [at(a + (c - a) * i / edge, b + (d - b) * i / edge) for (a, b), (c, d) in zip(corners, corners[1:] + corners[:1]) for i in range(edge)]
    idle = [pt for u, v in IDLE_CELLS for pt in fill(u, v, 2, 2)]
    width = pitch * 0.6
    return (
        f'<path d="{dots(shell)}" stroke="{C["ink"]}" stroke-width="{width:.1f}" stroke-linecap="round" opacity=".7"/>'
        f'<path d="{dots(fill(4, 4, 5, 5))}" stroke="{C["normal"]}" stroke-width="{width:.1f}" stroke-linecap="round"/>'
        f'<path d="{dots(idle)}" stroke="{C["idle"]}" stroke-width="{width:.1f}" stroke-linecap="round"/>'
    )


def social():
    W, H = 1280, 640
    body = [lockup(C["ink"], 80, 76)[0]]
    size, lead, top = 104, 96, 330
    p, _ = text(display, "CI on the box", size, 74, top, C["ink"], tracking=-0.035)
    body.append(p)
    p, w = text(display, "you ", size, 74, top + lead, C["ink"], tracking=-0.035)
    body.append(p)
    p, _ = text(display, "already", size, 74 + w, top + lead, None, tracking=-0.035, stroke=(C["ink"], 1.8))
    body.append(p)
    p, _ = text(display, "pay for.", size, 74, top + 2 * lead, C["ink"], tracking=-0.035)
    body.append(p)
    body.append(particle_mark(W - 80 - 340, (H - 340) / 2, 340))
    return document(W, H, "boxrunner: CI on the box you already pay for.", "".join(body), background=f'<rect width="{W}" height="{H}" fill="{C["night"]}"/>')


def brand():
    favicon = tile(rounded=True)
    write(SITE / "favicon.svg", favicon)
    uri = "data:image/svg+xml," + urllib.parse.quote(favicon.strip(), safe=" =/:.-")
    write(WORKER / "favicon.ts", f"// Drawn by docs/art/build.py, the same as site/favicon.svg.\nexport const favicon = '{uri}';\n")
    write(SITE / "mark.svg", f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="2 2 12 12" aria-hidden="true">{mark_paths(filled(C["ink"]))}</svg>\n')
    style = f'.box{{fill:{C["ink"]}}}@media (prefers-color-scheme:light){{.box{{fill:{C["night"]}}}}}'
    write(ART / "logo.svg", document(96, 96, "boxrunner", mark(0, 0, 96, 'class="box"'), style, background=""))
    for name, fg in [("logo-lockup-dark.svg", C["ink"]), ("logo-lockup-light.svg", C["night"])]:
        body, w = lockup(fg)
        write(ART / name, document(w, 36, "boxrunner", body, background=""))
    for size, rounded, name in [(180, False, "apple-touch-icon.png"), (192, True, "icon-192.png"), (512, True, "icon-512.png")]:
        png(tile(rounded), size, size, SITE / name)
    png(social(), 1280, 640, ART / "social-preview.png")


hero()
bench()
flow()
icons()
terminal()
brand()
