#!/usr/bin/env python3
"""Draws the README's images in the site's Paper design, from its fonts and colors.

    python3 docs/art/build.py

Text is shaped with HarfBuzz (hb-shape) and written as outlines, so GitHub
shows it in the site's faces without loading a font. Needs Python with
fonttools and brotli (pip3 install fonttools brotli) and hb-shape (HarfBuzz).

The hero, the benchmark and the flow come in a light and a dark variant,
which the README picks with <picture>. Also draws the logo, the feature icons,
the site's favicon, app icons and header mark, the Worker's favicon and the
repository's social preview, and recolors the install terminal. The PNGs are
screenshots from headless Chrome, found on the PATH or at the path in $CHROME.
"""

import json
import math
import os
import random
import re
import shutil
import subprocess
import tempfile
import urllib.parse
from pathlib import Path

from fontTools.pens.basePen import BasePen
from fontTools.ttLib import TTFont
from fontTools.varLib import instancer

ART = Path(__file__).resolve().parent
SITE = ART.parent.parent / "site"
WORKER = ART.parent.parent / "worker" / "src"
FONTS = SITE / "fonts"
TMP = Path(tempfile.mkdtemp())

# The site's tokens (site/index.html): the paper theme, named light here for the README, and the dark one.
PALETTES = {
    "light": {
        "paper": "#e9dfc9", "plate": "#ddd0b2", "slip": "#f5efe1", "ink": "#241c14", "ink2": "#463a2d",
        "dim": "#66584a", "faint": "#93846d", "rule": "rgba(36,28,20,.16)", "rule2": "rgba(36,28,20,.28)",
        "blue": "#2c69ad", "amber": "#93620f", "key": "#b8461f", "ok": "#26794a",
        "wash_blue": ("90 167 255", .55), "wash_amber": ("245 181 74", .62), "wash_key": ("255 106 61", .5),
        "wash_ok": ("69 212 138", .28), "wash_del": ("255 106 61", .16),
        "blend": "multiply", "emboss": "rgba(255,252,240,.55)", "tex": "36 28 20",
    },
    "dark": {
        "paper": "#17120d", "plate": "#221b14", "slip": "#2b231a", "ink": "#ede2cb", "ink2": "#d6c9b0",
        "dim": "#ab9c84", "faint": "#7f725e", "rule": "rgba(237,226,203,.13)", "rule2": "rgba(237,226,203,.24)",
        "blue": "#8dc0f8", "amber": "#f1c477", "key": "#ff9068", "ok": "#6cd8a2",
        "wash_blue": ("90 167 255", .40), "wash_amber": ("245 181 74", .40), "wash_key": ("255 106 61", .45),
        "wash_ok": ("69 212 138", .16), "wash_del": ("255 106 61", .14),
        "blend": "screen", "emboss": "rgba(0,0,0,.35)", "tex": "237 226 203",
    },
}
# The mark's own colors, the same on both themes, as the site's header draws it.
MARK_BLUE, MARK_AMBER = "#5aa7ff", "#f5b54a"
# site/build.ts swaps this frame color of mark.svg for currentColor.
MARK_FRAME = "#edf1f7"
C30 = math.sqrt(3) / 2


def num(v):
    s = f"{v:.1f}"
    return s[:-2] if s.endswith(".0") else s


def tn(v):
    return f"{v:.4g}"


def compact(values):
    """Numbers joined with the fewest separators: a minus sign needs none."""
    out = ""
    for v in values:
        t = str(v)
        out += t if not out or t[0] == "-" else " " + t
    return out


class RelativePen(BasePen):
    """Writes a glyph as an SVG path of relative whole-unit commands, which is a third shorter than absolute ones."""

    def __init__(self, glyphs):
        super().__init__(glyphs)
        self.out, self.at = [], (0, 0)
        self._start = (0, 0)

    def _emit(self, cmd, *pts):
        flat = []
        for x, y in pts:
            flat += [round(x) - self.at[0], round(y) - self.at[1]]
        last = pts[-1]
        self.at = (round(last[0]), round(last[1]))
        self.out.append(cmd + compact(flat))

    def _moveTo(self, p):
        self._emit("m", p)
        self._start = self.at

    def _lineTo(self, p):
        self._emit("l", p)

    def _curveToOne(self, a, b, p):
        # control points are relative to the segment's start, not to each other
        start = self.at
        flat = []
        for x, y in (a, b, p):
            flat += [round(x) - start[0], round(y) - start[1]]
        self.at = (round(p[0]), round(p[1]))
        self.out.append("c" + compact(flat))

    def _qCurveToOne(self, a, p):
        start = self.at
        flat = []
        for x, y in (a, p):
            flat += [round(x) - start[0], round(y) - start[1]]
        self.at = (round(p[0]), round(p[1]))
        self.out.append("q" + compact(flat))

    def _closePath(self):
        self.out.append("z")
        self.at = self._start

    def _endPath(self):
        pass

    def commands(self):
        return "".join(self.out)


class Face:
    def __init__(self, file, **axes):
        font = TTFont(FONTS / file)
        font.flavor = None
        if "fvar" in font:
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
            pen = RelativePen(self.glyphs)
            self.glyphs[self.order[gid]].draw(pen)
            USED[name] = pen.commands()
        return USED[name]


USED = {}
EXTRA = {}
FACES = 0

display = Face("castoro.woff2")
display_italic = Face("castoro-italic.woff2")
ui = Face("instrument-sans.woff2", wght=400)
ui_bold = Face("instrument-sans.woff2", wght=600)
mono = Face("geist-mono.woff2", wght=400)
mono_bold = Face("geist-mono.woff2", wght=500)


def text(face, s, size, x, y, fill, tracking=0.0, anchor="start", extra=""):
    if anchor != "start":
        w = face.width(s, size, tracking)
        x -= w if anchor == "end" else w / 2
    uses, w = face.path(s, size, x, y, tracking)
    return f'<g fill="{fill}" {extra}>{uses}</g>', w


def runs(parts, size, x, y):
    """Draws (face, text, color) parts one after another on a line."""
    out = []
    for face, s, fill in parts:
        p, w = text(face, s, size, x, y, fill)
        out.append(p)
        x += w
    return "".join(out), x


def wrap(face, s, size, limit):
    lines, line = [], ""
    for word in s.split():
        trial = f"{line} {word}".strip()
        if line and face.width(trial, size) > limit:
            lines.append(line)
            line = word
        else:
            line = trial
    return lines + [line]


def document(w, h, title, body, style="", background="", desc=""):
    """A standalone SVG with its glyphs and patterns, after which the next image starts with none."""
    defs = "".join(f'<path id="{k}" d="{d}"/>' for k, d in USED.items() if d) + "".join(EXTRA.values())
    USED.clear()
    EXTRA.clear()
    return (
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {num(w)} {num(h)}" width="{num(w)}" height="{num(h)}" role="img">'
        f"<title>{title}</title>"
        + (f"<desc>{desc}</desc>" if desc else "")
        + (f"<style>{style}</style>" if style else "")
        + (f"<defs>{defs}</defs>" if defs else "")
        + f"{background}{body}</svg>\n"
    )


def write(path, doc):
    path.write_text(doc)
    print(f"{path.relative_to(ART.parent.parent)}: {len(doc.encode()) / 1024:.1f} kB")


def hatch(pid, gap, angle, ink, width, opacity):
    """A pattern of parallel engraver's lines, at angle degrees, filled with url(#pid)."""
    EXTRA[pid] = (
        f'<pattern id="{pid}" width="{tn(gap)}" height="{tn(gap)}" patternUnits="userSpaceOnUse" patternTransform="rotate({angle})">'
        f'<path d="M0 {tn(gap / 2)}H{tn(gap)}" stroke="{ink}" stroke-width="{width}" opacity="{opacity}"/></pattern>'
    )
    return f"url(#{pid})"


def rgb(token, k=1.0):
    color, a = token
    return f'rgb({color})', round(a * k, 3)


class Iso:
    """The site's isometric projection: x runs down to the right, y down to the left, z up."""

    def __init__(self, ox, oy, s):
        self.ox, self.oy, self.s = ox, oy, s

    def p(self, x, y, z=0.0):
        return (self.ox + (x - y) * C30 * self.s, self.oy + (x + y) * 0.5 * self.s - z * self.s)


def poly(pts, close=True):
    return "M" + "L".join(f"{num(x)} {num(y)}" for x, y in pts) + ("Z" if close else "")


def segs(iso, pairs):
    return "".join(f"M{num(iso.p(*a)[0])} {num(iso.p(*a)[1])}L{num(iso.p(*b)[0])} {num(iso.p(*b)[1])}" for a, b in pairs)


def dots(points):
    """A dot is a zero-length stroke with round caps. Moves are relative, so that two lists of the same length morph into each other."""
    out, (px, py) = [], (0, 0)
    for i, (x, y) in enumerate(points):
        x, y = round(x), round(y)
        out.append(f"{'M' if i == 0 else 'm'}{compact([x - px, y - py])}h0")
        px, py = x, y
    return "".join(out)


def hull(points):
    p = sorted(points)

    def cross(o, a, b):
        return (a[0] - o[0]) * (b[1] - o[1]) - (a[1] - o[1]) * (b[0] - o[0])

    lo, up = [], []
    for q in p:
        while len(lo) >= 2 and cross(lo[-2], lo[-1], q) <= 0:
            lo.pop()
        lo.append(q)
    for q in reversed(p):
        while len(up) >= 2 and cross(up[-2], up[-1], q) <= 0:
            up.pop()
        up.append(q)
    return lo[:-1] + up[:-1]


# ---- the engraved box, as on the site's hero plate ----------------------------------------------

N, HT = 12, 7
# One big main job and five small idle ones, as in the logo: (x, y, size, kind, period, offset).
JOBS = [
    (2, 2, 5, "n", 11, 0.1),
    (8, 2, 2, "i", 6.3, 4.1),
    (8, 5, 2, "i", 7.2, 2.4),
    (2, 8, 2, "i", 6.7, 5.6),
    (5, 8, 2, "i", 7.7, 0.9),
    (8, 8, 2, "i", 6.9, 3.2),
]
FORM, DISSOLVE, REST = 1.9, 1.7, 1.3


def case_layers(iso, pal):
    """The case, drawn in hatching: (back, front), with the jobs between them."""
    s, ink = iso.s, pal["ink"]
    g = s / 24 * 1.25
    rnd = random.Random(7)
    back = []

    # the cast shadow on the ground, light from the upper left
    base = [(0, 0), (N, 0), (N, N), (0, N)]
    shifted = [(x + 0.42 * HT, y - 0.1 * HT) for x, y in base]
    shadow = [iso.p(x, y) for x, y in hull(base + shifted)]
    ground = [iso.p(x, y) for x, y in reversed(base)]
    back.append(f'<path d="{poly(shadow)}{poly(ground)}" fill-rule="evenodd" fill="{hatch("hsh", 2.6 * g, 0, ink, 0.9, 0.6)}"/>')
    # short horizontal strokes, the engraver's way of laying a floor
    floor_y = iso.p(N, 0)[1] - 4
    strokes = []
    for _ in range(46):
        u, v = rnd.random(), rnd.random()
        x, y = iso.p(N + 0.2 + u * 6.5, -1 + v * (N + 2))
        length = (6 + rnd.random() * 26) * s / 24
        if y >= floor_y:
            strokes.append(f"M{round(x)} {round(y)}h{round(length)}")
    back.append(f'<path d="{"".join(strokes)}" stroke="{ink}" stroke-width=".9" stroke-linecap="round" opacity=".32" fill="none"/>')

    floor = [iso.p(x, y) for x, y in base]
    wall_l = [iso.p(*q) for q in [(0, 0, 0), (0, N, 0), (0, N, HT), (0, 0, HT)]]
    wall_r = [iso.p(*q) for q in [(0, 0, 0), (N, 0, 0), (N, 0, HT), (0, 0, HT)]]
    band = [iso.p(*q) for q in [(0, 0, 0), (N, 0, 0), (N, 1.1, 0), (1.1, 1.1, 0), (1.1, N, 0), (0, N, 0)]]
    back.append(f'<path d="{poly(floor)}" fill="{hatch("hfl", 5.2 * g, 30, ink, 0.9, 0.34)}"/>')
    back.append(f'<path d="{poly(wall_l)}" fill="{hatch("hwl", 4.4 * g, 90, ink, 0.9, 0.38)}"/>')
    back.append(f'<path d="{poly(wall_r)}" fill="{hatch("hwr", 4.2 * g, 90, ink, 0.9, 0.4)}"/>')
    back.append(f'<path d="{poly(wall_r)}" fill="{hatch("hwd", 5.2 * g, -30, ink, 0.85, 0.32)}"/>')
    back.append(f'<path d="{poly(band)}" fill="{hatch("hbd", 3 * g, -30, ink, 0.8, 0.38)}"/>')
    back.append(f'<path d="{segs(iso, [((0, 0, 0), (0, 0, HT)), ((0, 0, 0), (N, 0, 0)), ((0, 0, 0), (0, N, 0))])}" fill="none" stroke="{ink}" stroke-width="1.3" stroke-linecap="round" opacity=".7"/>')

    front = []
    edges = [((0, N, 0), (N, N, 0)), ((N, N, 0), (N, 0, 0)), ((N, N, 0), (N, N, HT)), ((0, N, 0), (0, N, HT)), ((N, 0, 0), (N, 0, HT)),
             ((0, 0, HT), (N, 0, HT)), ((N, 0, HT), (N, N, HT)), ((N, N, HT), (0, N, HT)), ((0, N, HT), (0, 0, HT))]
    front.append(f'<path d="{segs(iso, edges)}" fill="none" stroke="{ink}" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" opacity=".95"/>')
    k = 0.45
    rim = [((k, k, HT), (N - k, k, HT)), ((N - k, k, HT), (N - k, N - k, HT)), ((N - k, N - k, HT), (k, N - k, HT)), ((k, N - k, HT), (k, k, HT))]
    front.append(f'<path d="{segs(iso, rim)}" fill="none" stroke="{ink}" stroke-width="1" opacity=".6"/>')
    plinth_l = [iso.p(*q) for q in [(0, N, 0), (N, N, 0), (N, N, 0.55), (0, N, 0.55)]]
    plinth_r = [iso.p(*q) for q in [(N, 0, 0), (N, N, 0), (N, N, 0.55), (N, 0, 0.55)]]
    front.append(f'<path d="{poly(plinth_l)}" fill="{hatch("hpl", 2.8 * g, 90, ink, 0.9, 0.55)}"/>')
    front.append(f'<path d="{poly(plinth_r)}" fill="{hatch("hpr", 2.4 * g, 90, ink, 0.9, 0.65)}"/>')
    front.append(f'<path d="{segs(iso, [((0, N, 0.55), (N, N, 0.55)), ((N, N, 0.55), (N, 0, 0.55))])}" fill="none" stroke="{ink}" stroke-width="1.1" opacity=".8"/>')
    glints = [((N, 1.2 + i * 0.55, HT - 0.7), (N, 2.6 + i * 0.55, HT - 2.6)) for i in range(3)]
    glints += [((1.4 + i * 0.55, N, HT - 0.8), (2.4 + i * 0.55, N, HT - 2.2)) for i in range(2)]
    front.append(f'<path d="{segs(iso, glints)}" fill="none" stroke="{ink}" stroke-width="1" opacity=".4"/>')
    return "".join(back), "".join(front)


def job_shapes(iso, pal, scale=1.0):
    """For each job: its wash, its edges and the dots of its faces, formed and scattered."""
    rnd = random.Random(20261015)
    s = iso.s
    out = []
    for j, (x, y, w, kind, per, off) in enumerate(JOBS):
        h = w if kind == "n" else w
        x1, y1 = x + w, y + w
        faces = [
            (0, 2.4 * 0.5, lambda u, v: (x + u * w, y + v * w, h), w * w),
            (1, 5.5 * 0.5, lambda u, v: (x + u * w, y1, v * h), w * h),
            (2, 10 * 0.5, lambda u, v: (x1, y + u * w, v * h), w * h),
        ]
        groups = []
        for shade, dens, at, area in faces:
            n = max(6, round(dens * area * scale))
            early, late = [], []
            for _ in range(n):
                t = at(rnd.random(), rnd.random())
                cx, cy, sp = x + w / 2, y + w / 2, max(w, 3) * 0.95
                sc = (
                    min(11.4, max(0.6, cx + (rnd.random() + rnd.random() - 1) * sp)),
                    min(11.4, max(0.6, cy + (rnd.random() + rnd.random() - 1) * sp)),
                    0.3 + rnd.random() * 6.2,
                )
                (early if rnd.random() < 0.5 else late).append((iso.p(*t), iso.p(*sc)))
            early.sort(key=lambda q: (round(q[0][1] / 12), q[0][0]))
            late.sort(key=lambda q: (round(q[0][1] / 12), q[0][0]))
            groups.append((shade, early, late))
        top = [(x, y, h), (x1, y, h), (x1, y1, h), (x, y1, h)]
        left = [(x, y1, 0), (x1, y1, 0), (x1, y1, h), (x, y1, h)]
        right = [(x1, y, 0), (x1, y1, 0), (x1, y1, h), (x1, y, h)]
        wash = [(f, [iso.p(*q) for q in face]) for f, face in zip((0.45, 0.75, 1.0), (top, left, right))]
        edges = (
            poly([iso.p(*q) for q in top + [top[0]]], False)
            + poly([iso.p(*q) for q in [(x, y1, h), (x, y1, 0), (x1, y1, 0), (x1, y, 0), (x1, y, h)]], False)
            + poly([iso.p(*q) for q in [(x1, y1, 0), (x1, y1, h)]], False)
        )
        out.append({"kind": kind, "per": per, "off": off, "wash": wash, "edges": edges, "groups": groups, "order": x + y + w})
    return out


SHADE_OPACITY = [0.64, 0.78, 0.92]


def jobs_markup(iso, pal):
    """The jobs between the case's back and front, as (live, still).

    Live is SMIL: each job forms out of ink dots, runs and dissolves again. Still is the formed jobs, which a
    reduced-motion reader gets instead and which a static image uses alone.
    """
    shapes = sorted(job_shapes(iso, pal), key=lambda j: j["order"])
    ink, blend = pal["ink"], pal["blend"]
    k = iso.s / 24
    wash_layer, dot_layer, edge_layer, defs, still_dots = [], [], [], [], []
    for j, job in enumerate(shapes):
        main = job["kind"] == "n"
        colour = pal["wash_blue"] if main else pal["wash_amber"]
        faces = []
        for f, pts in job["wash"]:
            c, a = rgb(colour, f)
            shifted = [(px + 1.6 * k, py + 1.1 * k) for px, py in pts]
            faces.append(
                f'<path d="{poly(shifted)}" fill="{c}" fill-opacity="{a}" stroke="{c}" stroke-opacity="{round(a * 0.5, 3)}" '
                f'stroke-width="{round(2.4 * k, 2)}" stroke-linejoin="round"/>'
            )
        defs.append(f'<g id="w{j}">{"".join(faces)}</g>')
        defs.append(
            f'<path id="e{j}" d="{job["edges"]}" fill="none" stroke="{ink}" stroke-width="{1.7 if main else 1.4}" '
            f'stroke-linecap="round" stroke-linejoin="round" opacity=".9"/>'
        )

        per, off = job["per"], job["off"]
        end_run = per - DISSOLVE - REST
        total = end_run + DISSOLVE
        start = -(off % total)
        times = [0, 0.55 * FORM, FORM, end_run, end_run + 0.45 * DISSOLVE, total]
        kt = ";".join(tn(t / total) for t in times)
        ramp = ".4 0 .6 1"
        fade = (
            f'<animate attributeName="opacity" values="0;0;1;1;0;0" keyTimes="{kt}" calcMode="spline" '
            f'keySplines="0 0 1 1;{ramp};0 0 1 1;{ramp};0 0 1 1" dur="{tn(total)}s" begin="{tn(start)}s" repeatCount="indefinite"/>'
        )
        wash_layer.append(f'<use href="#w{j}" opacity="0">{fade}</use>')
        edge_layer.append(f'<use href="#e{j}" opacity="0">{fade}</use>')

        dkt = ";".join(tn(t) for t in (0, FORM / total, end_run / total, 1))
        splines = ".45 0 .2 1;0 0 1 1;.6 0 .85 1"
        for gi, (shade, early, late) in enumerate(job["groups"]):
            for li, grp in enumerate((early, late)):
                if not grp:
                    continue
                formed, scattered = dots([a for a, _ in grp]), dots([b for _, b in grp])
                timing = f'dur="{tn(total)}s" begin="{tn(start + 0.12 * gi + 0.32 * li)}s" repeatCount="indefinite"'
                pid, op = f"d{j}{gi}{li}", SHADE_OPACITY[shade]
                defs.append(f'<path id="{pid}" d="{formed}"/>')
                still_dots.append(f'<use href="#{pid}" opacity="{op}"/>')
                dot_layer.append(
                    f'<path fill="none" stroke="{ink}" stroke-width="{round(3.4 * k, 2)}" stroke-linecap="round" opacity=".22">'
                    f'<animate attributeName="d" values="{scattered};{formed};{formed};{scattered}" keyTimes="{dkt}" calcMode="spline" keySplines="{splines}" {timing}/>'
                    f'<animate attributeName="opacity" values=".22;{op};{op};.22" keyTimes="{dkt}" calcMode="spline" keySplines="{splines}" {timing}/></path>'
                )
    EXTRA["jobs"] = "".join(defs)
    live = f'<g style="mix-blend-mode:{blend}">{"".join(wash_layer)}</g>{"".join(dot_layer)}{"".join(edge_layer)}'
    still = (
        f'<g style="mix-blend-mode:{blend}">{"".join(f"<use href=\"#w{j}\"/>" for j in range(len(shapes)))}</g>'
        f'<g fill="none" stroke="{ink}" stroke-width="{round(3.4 * k, 2)}" stroke-linecap="round">{"".join(still_dots)}</g>'
        + "".join(f'<use href="#e{j}"/>' for j in range(len(shapes)))
    )
    return live, still


MOTION_STYLE = ".still{display:none}@media (prefers-reduced-motion:reduce){.live{display:none}.still{display:inline}}"


def plate_frame(pal, x, y, w, h, inner=14):
    """The plate: a sheet pressed into the paper, with the bevel mark inside its edge."""
    return (
        f'<rect x="{x}" y="{y + 1.5}" width="{w}" height="{h}" rx="3" fill="#000" opacity=".1"/>'
        f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="3" fill="{pal["plate"]}"/>'
        f'<rect x="{x}" y="{y - 1}" width="{w}" height="{h}" rx="3" fill="none" stroke="{pal["emboss"]}" stroke-width="1"/>'
    )


def plate_mark(pal, x, y, w, h, inner=14):
    return (
        f'<rect x="{x + inner + 1}" y="{y + inner + 1}" width="{w - 2 * inner}" height="{h - 2 * inner}" rx="2" fill="none" stroke="{pal["emboss"]}"/>'
        f'<rect x="{x + inner}" y="{y + inner}" width="{w - 2 * inner}" height="{h - 2 * inner}" rx="2" fill="none" stroke="{pal["rule2"]}"/>'
    )


def laid_paper(pal, w, h, rx):
    EXTRA["laid"] = (
        f'<pattern id="laid" width="38" height="4" patternUnits="userSpaceOnUse">'
        f'<path d="M0 0V4" stroke="rgb({pal["tex"]})" stroke-opacity=".05" stroke-width="1"/>'
        f'<path d="M0 3.5H38" stroke="rgb({pal["tex"]})" stroke-opacity=".03" stroke-width="1"/></pattern>'
    )
    return f'<rect width="{w}" height="{h}" rx="{rx}" fill="{pal["paper"]}"/><rect width="{w}" height="{h}" rx="{rx}" fill="url(#laid)"/>'


def slip(pal, x, y, w, rotate, rows, size=16.5, header="one line", file=".github/workflows/ci.yml"):
    """The workflow change as a slip of paper: a header and rows of code, one removed and one added."""
    lh, pad = 25, 16
    head = 40
    h = head + len(rows) * lh + 18
    out = [
        f'<g transform="rotate({rotate} {num(x + w / 2)} {num(y + h / 2)})">',
        f'<rect x="{x}" y="{y + 3}" width="{w}" height="{h}" rx="2" fill="#000" opacity=".1"/>',
        f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="2" fill="{pal["slip"]}"/>',
    ]
    p, _ = text(mono, file, 14.5, x + pad, y + 26, pal["dim"])
    out.append(p)
    p, _ = text(mono, header, 14.5, x + w - pad, y + 26, pal["dim"], anchor="end")
    out.append(p)
    out.append(f'<path d="M{x} {y + 37}H{x + w}" stroke="{pal["rule2"]}" stroke-dasharray="4 3"/>')
    cw = mono.width("m", size)
    for i, (kind, indent, body) in enumerate(rows):
        top = y + head + i * lh
        base = top + lh - 7
        if kind:
            c, a = rgb(pal["wash_del"] if kind == "-" else pal["wash_ok"])
            out.append(f'<rect x="{x}" y="{top + 1}" width="{w}" height="{lh}" fill="{c}" fill-opacity="{a}"/>')
            p, _ = text(mono, kind, size, x + 12, base, pal["key"] if kind == "-" else pal["ok"])
            out.append(p)
        px = x + 30 + indent * cw
        if kind == "-":
            p, w2 = text(mono, body, size, px, base, pal["dim"])
            out.append(p)
            out.append(f'<path d="M{num(px)} {num(base - size * 0.3)}h{num(w2)}" stroke="{pal["key"]}" stroke-width="1.3"/>')
        elif kind == "+":
            lead, tail = body.split(": ")
            p, w2 = text(mono, lead + ": ", size, px, base, pal["ink2"])
            out.append(p)
            p, _ = text(mono_bold, tail, size, px + w2, base, pal["ink"])
            out.append(p)
        else:
            p, _ = text(mono, body, size, px, base, pal["ink2"])
            out.append(p)
    out.append("</g>")
    return "".join(out)


HEADLINE = ["CI on the box", "you already pay for."]
LEDE = "Self-hosted GitHub Actions runners on one Linux box. Every job gets a fresh container that is gone when the job ends, and your GitHub App key never touches the box."
DIFF = [("", 0, "jobs:"), ("", 2, "build:"), ("-", 4, "runs-on: ubuntu-latest"), ("+", 4, "runs-on: boxrunner"), ("", 4, "steps:"), ("", 6, "- uses: actions/checkout@v4")]


def hero(theme):
    pal = PALETTES[theme]
    W, H = 1200, 722
    px, py, pw, ph = 18, 18, W - 36, 648
    iso = Iso(898, 316, 23.5)
    body = [laid_paper(pal, W, H, 10), plate_frame(pal, px, py, pw, ph)]
    clip = f'<clipPath id="plate"><rect x="{px}" y="{py}" width="{pw}" height="{ph}" rx="3"/></clipPath>'
    EXTRA["clip"] = clip
    back, front = case_layers(iso, pal)
    live, still = jobs_markup(iso, pal)
    body.append(f'<g clip-path="url(#plate)">{back}<g class="live">{live}</g><g class="still">{still}</g>{front}</g>')
    body.append(plate_mark(pal, px, py, pw, ph))

    size = 70
    for i, line in enumerate(HEADLINE):
        p, _ = text(display, line, size, 74, 148 + i * 78, pal["ink"], tracking=-0.01)
        body.append(p)
    for i, line in enumerate(wrap(ui, LEDE, 22, 500)):
        p, _ = text(ui, line, 22, 76, 292 + i * 33, pal["ink2"])
        body.append(p)
    body.append(slip(pal, 74, 430, 392, -1.0, DIFF))

    p, w = text(display_italic, "Plate I.", 17, 24, 702, pal["ink2"])
    body.append(p)
    p, _ = text(ui, "One box, one main job, five idle ones. Each container forms from ink, runs, then breaks back into dots.", 15, 24 + w + 8, 702, pal["dim"])
    body.append(p)
    p, _ = text(ui, "boxrunner.zabcik.me", 15, W - 24, 702, pal["dim"], anchor="end")
    body.append(p)
    desc = "A box drawn in hatching holds one main job and five idle ones. Each forms out of ink dots, washed blue or amber, runs, and breaks back into dots."
    write(ART / f"hero-{theme}.svg", document(W, H, "boxrunner: CI on the box you already pay for.", "".join(body), MOTION_STYLE, "", desc))



def sparkle_squares(pal, x, y, n, kind, size=11, gap=4):
    c, a = rgb(pal["wash_blue"] if kind == "n" else pal["wash_amber"], 1.0)
    out = []
    for i in range(n):
        out.append(
            f'<rect x="{num(x + i * (size + gap))}" y="{num(y)}" width="{size}" height="{size}" fill="{c}" fill-opacity="{a}" stroke="{pal["ink"]}" stroke-width="1.1"/>'
        )
    return "".join(out)


def bench(theme):
    pal = PALETTES[theme]
    W = 1200
    machines = [
        ("4-core ARM VPS", [428, 429, 2096], [0, 0.5, 1, 1.5, 2], "s"),
        ("4-core x86 VPS", [1393, 1438, 6905], [0, 2, 4, 6], "s"),
    ]
    rows = [("Alone", None), ("Beside four idle jobs", "i"), ("Beside four normal jobs", "n")]
    x0, bar_x, bar_w, right = 64, 440, 560, W - 64
    body = [laid_paper(pal, W, 100, 10)]  # resized below
    inner = []
    p, w = text(display_italic, "Fig. 2.", 22, x0, 78, pal["ink2"])
    inner.append(p)
    p, _ = text(display_italic, "4 threads of CPU work in a normal-priority job, timed alone and beside four busy jobs.", 22, x0 + w + 8, 78, pal["ink2"])
    inner.append(p)

    hatch_id = hatch("hwait", 5.5, -45, pal["ink"], 1, 0.6)
    y = 130
    n = 0
    anim = []
    for name, values, ticks, _unit in machines:
        p, _ = text(mono_bold, name.upper(), 14.5, x0, y, pal["dim"], tracking=0.1)
        inner.append(p)
        inner.append(f'<path d="M{x0} {y + 14}H{right}" stroke="{pal["rule2"]}"/>')
        y += 48
        top = max(values)
        for (label, kind), value in zip(rows, values):
            p, lw = text(ui, label, 21, x0, y + 7, pal["ink"])
            inner.append(p)
            if kind:
                inner.append(sparkle_squares(pal, x0 + lw + 14, y - 5, 4, kind))
            length = max(6, value / top * bar_w)
            bc, ba = rgb(pal["wash_blue"])
            delay = 0.12 * n
            if kind == "n":
                alone = values[0] / top * bar_w
                bar = (
                    f'<rect x="{bar_x}" y="{y - 14}" width="{num(alone)}" height="28" fill="{bc}" fill-opacity="{ba}" stroke="{pal["ink"]}" stroke-width="1.4"/>'
                    f'<rect x="{num(bar_x + alone)}" y="{y - 14}" width="{num(length - alone)}" height="28" fill="{hatch_id}" stroke="{pal["ink"]}" stroke-width="1.4"/>'
                )
            else:
                bar = f'<rect x="{bar_x}" y="{y - 14}" width="{num(length)}" height="28" fill="{bc}" fill-opacity="{ba}" stroke="{pal["ink"]}" stroke-width="1.4"/>'
            inner.append(f'<g class="b" style="animation-delay:{delay:.2f}s">{bar}</g>')
            shown = f"{value} ms" if value < 1000 else f"{value / 1000:.2f} s"
            slow = kind == "n"
            p, vw = text(mono_bold, shown, 20, right, y + 7, pal["key"] if slow else pal["ink"], anchor="end")
            inner.append(p)
            if kind:
                change = f"+{round((value / values[0] - 1) * 100)}%"
                p, _ = text(mono, change, 16, right - vw - 16, y + 6, pal["key"] if slow else pal["dim"], anchor="end")
                inner.append(p)
            y += 56
            n += 1
        # the axis
        y -= 14
        inner.append(f'<path d="M{bar_x} {y}H{bar_x + bar_w}" stroke="{pal["rule2"]}"/>')
        for t in ticks:
            tx = bar_x + t * 1000 / top * bar_w
            if tx > bar_x + bar_w + 1:
                continue
            inner.append(f'<path d="M{num(tx)} {y}v6" stroke="{pal["rule2"]}"/>')
            label = "0" if t == 0 else f"{t:g} s"
            p, _ = text(mono, label, 14, tx, y + 24, pal["dim"], anchor="middle" if t else "start")
            inner.append(p)
        y += 76
    y -= 30
    inner.append(f'<rect x="{x0}" y="{y - 12}" width="22" height="13" fill="{hatch_id}" stroke="{pal["ink"]}" stroke-width="1"/>')
    p, _ = text(ui, "time spent waiting for a core", 16.5, x0 + 32, y, pal["dim"])
    inner.append(p)
    y += 44
    line, _ = runs(
        [(ui, "Beside idle jobs the main job slowed ", pal["ink2"]), (ui_bold, "0 to 3%", pal["ink"]), (ui, ". Beside normal ones, ", pal["ink2"]), (ui_bold, "about 390%", pal["ink"]), (ui, ".", pal["ink2"])],
        20, x0, y,
    )
    inner.append(line)
    y += 34
    line, _ = runs([(ui, "Median of 3 runs. Run ", pal["dim"]), (mono, "sudo boxrunner bench", pal["ink2"]), (ui, " on your own box.", pal["dim"])], 17, x0, y)
    inner.append(line)
    H = y + 46
    body = [laid_paper(pal, W, H, 10)]
    card = (
        f'<rect x="24" y="27" width="{W - 48}" height="{H - 48}" rx="3" fill="#000" opacity=".1"/>'
        f'<rect x="24" y="24" width="{W - 48}" height="{H - 48}" rx="3" fill="{pal["slip"]}"/>'
    )
    body.append(card)
    body.append(f'<g transform="translate(0 0)">{"".join(inner)}</g>')
    style = (
        ".b{transform-box:fill-box;transform-origin:left;animation:grow 1.2s cubic-bezier(.2,.8,.2,1) both}"
        "@keyframes grow{from{transform:scaleX(0)}}@media (prefers-reduced-motion:reduce){.b{animation:none}}"
    )
    desc = "On a 4-core ARM VPS the job takes 428 ms alone, 429 ms beside four busy idle-priority jobs and 2.10 s beside four busy normal-priority jobs. On a 4-core x86 VPS it takes 1.39 s alone, 1.44 s beside idle jobs and 6.91 s beside normal jobs."
    write(ART / f"bench-{theme}.svg", document(W, H, "boxrunner bench: idle jobs leave the main job's speed alone", "".join(body), style, "", desc))



def keyfig():
    """The safe and the glass case of the site's Fig. 3, as markup of the site's own paths: (safe, case)."""
    html = (SITE / "index.html").read_text()
    start = html.index('<svg viewBox="80 -12 935 400"')
    body = html[html.index("</defs>", start) + 7 : html.index('<path id="route"', start)]
    cut = body.index('<text class="letter" x="206"')
    safe, case = body[:cut], body[body.index("</text>", cut) + 7 :]
    return safe, re.sub(r'<text class="letter".*?</text>', "", case)


def figure_style(pal):
    """The site's figure rules (.keyfig in index.html) for the paths copied from it."""
    ink = pal["ink"]
    jobs = {
        "n": ("90 167 255", (0.28, 0.45, 0.62)),
        "i": ("245 181 74", (0.3, 0.48, 0.66)),
    }
    css = [
        f'.ed{{fill:none;stroke:{ink};stroke-width:1.4;stroke-linejoin:round;stroke-linecap:round}}.ed.thin{{stroke-width:.8;opacity:.75}}',
        f'.key path{{fill:rgb({pal["wash_key"][0]} / {pal["wash_key"][1]});stroke:{pal["key"]};stroke-width:1.3;stroke-linejoin:round;mix-blend-mode:{pal["blend"]}}}',
        f'.job .wl,.job .wr,.job .wt{{stroke:none;mix-blend-mode:{pal["blend"]}}}.job .ed{{stroke-width:1}}',
    ]
    for kind, (c, (t, l, r)) in jobs.items():
        css.append(f".job.{kind} .wt{{fill:rgb({c} / {t})}}.job.{kind} .wl{{fill:rgb({c} / {l})}}.job.{kind} .wr{{fill:rgb({c} / {r})}}")
    return "".join(css)


def figure_patterns(pal):
    ink = pal["ink"]
    return (
        f'<pattern id="hv" width="4.5" height="4.5" patternUnits="userSpaceOnUse"><path d="M1 0V4.5" stroke="{ink}" stroke-width=".7" opacity=".55"/></pattern>'
        f'<pattern id="hx" width="4.2" height="4.2" patternUnits="userSpaceOnUse" patternTransform="rotate(30)"><path d="M0 1H4.2M1 0V4.2" stroke="{ink}" stroke-width=".65" opacity=".55"/></pattern>'
        f'<pattern id="hs" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(-30)"><path d="M0 1H6" stroke="{ink}" stroke-width=".55" opacity=".5"/></pattern>'
        f'<pattern id="hh" width="6" height="3.2" patternUnits="userSpaceOnUse"><path d="M0 1H6" stroke="{ink}" stroke-width=".6" opacity=".6"/></pattern>'
    )


def bezier(p0, c1, c2, p1, t):
    u = 1 - t
    return tuple(u**3 * a + 3 * u * u * t * b + 3 * u * t * t * c + t**3 * d for a, b, c, d in zip(p0, c1, c2, p1))


def route(pal, n, p0, c1, c2, p1, color):
    """A dotted route with an arrowhead, its step number halfway and the path for a token."""
    tangent = (p1[0] - c2[0], p1[1] - c2[1])
    ang = math.atan2(tangent[1], tangent[0])
    tip = p1
    back = (tip[0] - 10 * math.cos(ang), tip[1] - 10 * math.sin(ang))
    arrow = "".join(
        f"M{num(tip[0] - 13 * math.cos(ang + s))} {num(tip[1] - 13 * math.sin(ang + s))}L{num(tip[0])} {num(tip[1])}" for s in (0.5, -0.5)
    )
    end = (p1[0] - 4 * math.cos(ang), p1[1] - 4 * math.sin(ang))
    d = f"M{num(p0[0])} {num(p0[1])}C{num(c1[0])} {num(c1[1])} {num(c2[0])} {num(c2[1])} {num(end[0])} {num(end[1])}"
    mx, my = bezier(p0, c1, c2, p1, 0.5)
    badge = f'<circle cx="{num(mx)}" cy="{num(my)}" r="16" fill="{pal["slip"]}" stroke="{pal["ink"]}" stroke-width="1.2"/>'
    nm, _ = text(display_italic, str(n), 22, mx, my + 7.5, pal["ink"], anchor="middle")
    start = f'<circle cx="{num(p0[0])}" cy="{num(p0[1])}" r="3.6" fill="{pal["ink"]}" opacity=".75"/>'
    return {
        "line": start + f'<path d="{d}" fill="none" stroke="{pal["ink"]}" stroke-width="2.2" stroke-linecap="round" stroke-dasharray=".1 7.5" opacity=".75"/>'
        f'<path d="{arrow}" fill="none" stroke="{pal["ink"]}" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>',
        "badge": badge + nm,
        "d": d,
        "color": color,
        "mid": (mx, my),
    }


def pad(iso, w, d, pal, hid):
    """A hatched ground tile with a cast shadow, for an object to stand on."""
    base = [(0, 0), (w, 0), (w, d), (0, d)]
    shifted = [(x + 2.2, y - 0.5) for x, y in base]
    shadow = [iso.p(x, y) for x, y in hull(base + shifted)]
    ground = [iso.p(x, y) for x, y in reversed(base)]
    return (
        f'<path d="{poly(shadow)}{poly(ground)}" fill-rule="evenodd" fill="{hatch(hid + "s", 3.4, 0, pal["ink"], 0.9, 0.55)}"/>'
    )


def block(iso, x, y, w, d, h, ids=("hv", "hx", "hs")):
    """A hatched block: left face (+y), right face (+x) and top, with the plate's color under the lines."""
    x1, y1 = x + w, y + d
    left = [iso.p(x, y1, 0), iso.p(x1, y1, 0), iso.p(x1, y1, h), iso.p(x, y1, h)]
    right = [iso.p(x1, y, 0), iso.p(x1, y1, 0), iso.p(x1, y1, h), iso.p(x1, y, h)]
    top = [iso.p(x, y, h), iso.p(x1, y, h), iso.p(x1, y1, h), iso.p(x, y1, h)]
    return left, right, top


def github_object(iso, pal):
    """GitHub's queue as an engraved cabinet: one slot per class, blue for normal and amber for idle."""
    left, right, top = block(iso, 0, 0, 6, 6, 8)
    out = []
    for face, pat, op in ((left, "hv", 1), (right, "hx", 1), (top, "hs", 0.6)):
        out.append(f'<path d="{poly(face)}" fill="{pal["plate"]}"/><path d="{poly(face)}" fill="url(#{pat})" opacity="{op}"/>')
    edges = poly(top) + "".join(f"M{num(a[0])} {num(a[1])}L{num(b[0])} {num(b[1])}" for a, b in [(left[0], left[3]), (left[1], left[2]), (right[0], right[3]), (left[0], left[1]), (left[1], right[0])])
    out.append(f'<path class="ed" d="{edges}"/>')
    for (z0, z1), kind in (((5.0, 6.6), "n"), ((2.2, 3.8), "i")):
        slot = [iso.p(6, 0.9, z0), iso.p(6, 5.1, z0), iso.p(6, 5.1, z1), iso.p(6, 0.9, z1)]
        c, a = rgb(pal["wash_blue"] if kind == "n" else pal["wash_amber"], 1.1)
        out.append(f'<path d="{poly(slot)}" fill="{pal["slip"]}" opacity=".9"/>')
        out.append(f'<path d="{poly(slot)}" fill="{c}" fill-opacity="{a}" style="mix-blend-mode:{pal["blend"]}"/>')
        out.append(f'<path class="ed thin" d="{poly(slot)}"/>')
    return "".join(out)


def job_object(iso, pal, rnd):
    """One fresh container, mid-way from dots to a cube."""
    x, y, w = 0.5, 0.5, 4
    top = [iso.p(x, y, w), iso.p(x + w, y, w), iso.p(x + w, y + w, w), iso.p(x, y + w, w)]
    left = [iso.p(x, y + w, 0), iso.p(x + w, y + w, 0), iso.p(x + w, y + w, w), iso.p(x, y + w, w)]
    right = [iso.p(x + w, y, 0), iso.p(x + w, y + w, 0), iso.p(x + w, y + w, w), iso.p(x + w, y, w)]
    out = []
    c, a = rgb(pal["wash_blue"])
    for face, k in ((top, 0.45), (left, 0.75), (right, 1.0)):
        out.append(f'<path d="{poly(face)}" fill="{c}" fill-opacity="{round(a * k, 3)}" stroke="{c}" stroke-opacity="{round(a * k * 0.5, 3)}" stroke-width="2" style="mix-blend-mode:{pal["blend"]}"/>')
    pts = []
    for _ in range(46):
        pts.append(iso.p(x + rnd.random() * w, y + w, rnd.random() * w))
        pts.append(iso.p(x + w, y + rnd.random() * w, rnd.random() * w))
    for _ in range(12):
        pts.append(iso.p(x + rnd.random() * w, y + rnd.random() * w, w))
    out.append(f'<path d="{dots(pts)}" fill="none" stroke="{pal["ink"]}" stroke-width="2.4" stroke-linecap="round" opacity=".8"/>')
    scattered = [iso.p(x + w / 2 + (rnd.random() * 2 - 1) * 4.5, y + w / 2 + (rnd.random() * 2 - 1) * 4.5, w * 0.5 + rnd.random() * 7) for _ in range(34)]
    out.append(f'<path d="{dots(scattered)}" fill="none" stroke="{pal["ink"]}" stroke-width="2.2" stroke-linecap="round" opacity=".28"/>')
    edges = poly(top + [top[0]], False) + poly([iso.p(x, y + w, w), iso.p(x, y + w, 0), iso.p(x + w, y + w, 0), iso.p(x + w, y, 0), iso.p(x + w, y, w)], False) + poly([iso.p(x + w, y + w, 0), iso.p(x + w, y + w, w)], False)
    out.append(f'<path class="ed" d="{edges}"/>')
    return "".join(out)


def label(pal, x, y, title, sub, holds, color, anchor="start"):
    out = []
    p, _ = text(ui_bold, title, 24, x, y, pal["ink"], anchor=anchor)
    out.append(p)
    p, _ = text(mono, sub, 15, x, y + 26, pal["dim"], anchor=anchor)
    out.append(p)
    p, w = text(mono_bold, holds, 15, x, y + 52, pal[color], anchor=anchor)
    mark = x - w - 18 if anchor == "end" else x
    out.append(f'<circle cx="{num(mark + 6)}" cy="{num(y + 47)}" r="5" fill="{pal[color]}"/>')
    px = x if anchor == "end" else x + 18
    p, _ = text(mono_bold, holds, 15, px, y + 52, pal[color], anchor=anchor)
    out.append(p)
    return "".join(out)


def caption(pal, lines, x, y, anchor="start", size=17):
    out = []
    for i, line in enumerate(lines):
        p, _ = text(ui, line, size, x, y + i * 22, pal["ink2"], anchor=anchor)
        out.append(p)
    return "".join(out)


def token(pal, d, color, begin, period):
    """A slip of paper that carries a secret along a route: washed in the secret's color, with a ring for the seal."""
    c, a = rgb(pal["wash_" + color])
    life = f'dur="{period}s" begin="{begin}s" repeatCount="indefinite"'
    return (
        f'<g class="tok" opacity="0"><g fill="none"><rect x="-17" y="-11" width="34" height="22" rx="2" fill="{pal["slip"]}" stroke="{pal["ink"]}" stroke-width="1.3"/>'
        f'<rect x="-13" y="-7" width="19" height="14" fill="{c}" fill-opacity="{min(1, a * 1.3):.2f}"/><circle cx="10.5" cy="0" r="2.6" fill="{pal["plate"]}" stroke="{pal["ink"]}" stroke-width="1"/></g>'
        f'<animateMotion path="{d}" keyPoints="0;1;1" keyTimes="0;.16;1" calcMode="linear" {life}/>'
        f'<animate attributeName="opacity" values="0;1;1;0;0" keyTimes="0;.02;.15;.17;1" {life}/></g>'
    )


def flow(theme):
    pal = PALETTES[theme]
    W, H = 1200, 740
    EXTRA["figure"] = figure_patterns(pal)
    safe, case = keyfig()
    rnd = random.Random(11)
    body = [laid_paper(pal, W, H, 10)]
    safe = safe.replace("var(--plate)", pal["plate"])
    case = case.replace("var(--plate)", pal["plate"])
    sc_safe, sc_case = 0.72, 0.78
    s_pos, c_pos = (480, 140), (62, 300)
    body.append(f'<g transform="translate({num(s_pos[0] - 85 * sc_safe)} {num(s_pos[1] - 48 * sc_safe)}) scale({sc_safe})">{safe}</g>')
    body.append(f'<g transform="translate({num(c_pos[0] - 625 * sc_case)} {num(c_pos[1] - 75 * sc_case)}) scale({sc_case})">{case}</g>')
    gh_iso, job_iso = Iso(982, 330, 20), Iso(600, 580, 19)
    body.append(pad(gh_iso, 6, 6, pal, "gp") + github_object(gh_iso, pal))
    body.append(pad(job_iso, 5, 5, pal, "jp") + job_object(job_iso, pal, rnd))

    routes = [
        route(pal, 1, (262, 330), (300, 250), (380, 235), (470, 250), "amber"),
        route(pal, 2, (668, 160), (730, 80), (860, 70), (956, 160), "key"),
        route(pal, 3, (330, 388), (520, 392), (700, 360), (868, 322), "blue"),
        route(pal, 4, (272, 492), (310, 600), (430, 630), (516, 598), "ok"),
        route(pal, 5, (676, 584), (770, 600), (840, 540), (886, 430), "ok"),
    ]
    for r in routes:
        body.append(r["line"])
    for r in routes:
        body.append(r["badge"])
    for i, r in enumerate(routes):
        body.append(token(pal, r["d"], r["color"], i * 2, 10))

    body.append(label(pal, 480, 50, "Your Worker", "Cloudflare, free plan", "GitHub App key", "key"))
    body.append(label(pal, 64, 596, "Your box", "boxrunner agent, no open port", "box token", "amber"))
    body.append(label(pal, 968, 56, "GitHub", "Actions queue", "scale set per class", "blue"))
    body.append(label(pal, 746, 628, "Job container", "fresh for every job", "one-job runner config", "ok"))

    mx = lambda r: r["mid"]
    body.append(caption(pal, ["open a session", "with the box token"], mx(routes[0])[0] - 28, mx(routes[0])[1] - 16, "end"))
    body.append(caption(pal, ["create the scale set", "with the App key"], mx(routes[1])[0], mx(routes[1])[1] + 44, "middle"))
    body.append(caption(pal, ["long-poll the queue with a session token"], mx(routes[2])[0], mx(routes[2])[1] + 44, "middle"))
    body.append(caption(pal, ["fetch one runner config", "and start a container"], mx(routes[3])[0] - 70, mx(routes[3])[1] + 46))
    body.append(caption(pal, ["run exactly one job,", "then it is removed"], mx(routes[4])[0] + 28, mx(routes[4])[1] - 6))

    p, w = text(display_italic, "Fig. 3.", 20, 64, H - 28, pal["ink2"])
    body.append(p)
    p, _ = text(display_italic, "How a job reaches your box. The key stays in the Worker; the box holds only a token.", 20, 64 + w + 8, H - 28, pal["ink2"])
    body.append(p)
    style = figure_style(pal) + ".tok{display:inline}@media (prefers-reduced-motion:reduce){.tok{display:none}}"
    desc = "How a job reaches your box. 1: the agent on your box opens a session with the Worker, using the box token. 2: the Worker, which holds the GitHub App key, creates the scale set and session on GitHub. 3: the agent long-polls GitHub's queue with a session token. 4: when a job waits, the agent fetches one runner config from the Worker and starts a container. 5: the container runs exactly one job and is then removed."
    write(ART / f"flow-{theme}.svg", document(W, H, "How boxrunner gets a job onto your box", "".join(body), style, "", desc))


# ---- the mark, icons and logos --------------------------------------------------------------------

# The mark, on a 16-unit grid so that a 16 px favicon lands on whole pixels: the box
# you already own, the main job's container in it, and idle jobs in the room it leaves.
BOX = "M2 2h12v12H2zM3 3v10h10V3z"
MAIN = "M4 4h5v5H4z"
IDLE_CELLS = [(10, 4), (10, 7), (4, 10), (7, 10), (10, 10)]
IDLE = "".join(f"M{x} {y}h2v2h-2z" for x, y in IDLE_CELLS)


def mark_paths(frame, outline=None):
    """The mark on its grid; frame paints the box, with a fill or a class. outline adds a hairline to the jobs."""
    line = f' stroke="{outline}" stroke-width=".18"' if outline else ""
    return (
        f'<path d="{BOX}" fill-rule="evenodd" {frame}/><path d="{MAIN}" fill="{MARK_BLUE}"{line}/><path d="{IDLE}" fill="{MARK_AMBER}"{line}/>'
    )


def mark(x, y, size, frame):
    """The mark's 12 units, from (2, 2) to (14, 14), drawn size wide with its corner at (x, y)."""
    return f'<g transform="translate({num(x)} {num(y)}) scale({size / 12:.4g}) translate(-2 -2)">{mark_paths(frame)}</g>'


def lockup(fg, x=0.0, y=0.0):
    """The mark beside the name in Instrument Sans, as the site's header sets it; returns (markup, width)."""
    size, h, gap = 28, 34, 12
    cap = ui_bold.font["OS/2"].sCapHeight / ui_bold.upem * size
    words, w = text(ui_bold, "boxrunner", size, x + h + gap, y + h / 2 + cap / 2, fg, tracking=-0.01)
    return mark(x, y, h, f'fill="{fg}"') + words, h + gap + w


def icons():
    """The feature icons: engraved, in washes and a line that follows the reader's theme."""
    paper_ink, dark_ink = PALETTES["light"]["ink"], PALETTES["dark"]["ink"]
    style = f".l{{stroke:{paper_ink};fill:none}}.f{{fill:{paper_ink}}}@media (prefers-color-scheme:dark){{.l{{stroke:{dark_ink}}}.f{{fill:{dark_ink}}}}}"
    blue, amber, ok, key = "rgb(90 167 255 / ", "rgb(245 181 74 / ", "rgb(69 212 138 / ", "rgb(255 106 61 / "
    line = 'class="l" stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round"'
    shapes = {
        "container": (
            f'<path d="M16 17L6 11V23L16 29Z" fill="{blue}.5)"/><path d="M16 17L26 11V23L16 29Z" fill="{blue}.7)"/><path d="M16 17L6 11L16 5L26 11Z" fill="{blue}.3)"/>'
            f'<path d="M16 5L26 11V23L16 29L6 23V11ZM6 11L16 17L26 11M16 17V29" {line}/>'
        ),
        "key": (
            f'<circle cx="10" cy="15" r="6" fill="{key}.45)" stroke="{PALETTES["light"]["key"]}" stroke-width="1.6"/><circle cx="8.6" cy="13.6" r="1.8" class="l" stroke-width="1.2"/>'
            f'<path d="M15.5 17.5L28 25M23 22.5l-1.6 3M26 24.3l-1.6 3" class="l" stroke-width="1.6" stroke-linecap="round"/>'
        ),
        "priority": (
            f'<rect x="4" y="5" width="24" height="9" fill="{blue}.6)"/><rect x="4" y="18" width="14" height="9" fill="{amber}.7)"/>'
            f'<path d="M4 5h24v9H4zM4 18h14v9H4z" {line}/>'
        ),
        "speed": (
            f'<circle cx="16" cy="18" r="10" fill="{ok}.4)"/><circle cx="16" cy="18" r="10" {line}/><path d="M16 18l5-6M13 5h6M16 5v3" {line}/>'
            f'<path d="M16 10v1.4M24 18h-1.4M16 26v-1.4M8 18h1.4" class="l" stroke-width="1.2" stroke-linecap="round"/>'
        ),
        "light": (
            f'<rect x="4" y="9" width="24" height="16" rx="1.5" fill="{ok}.18)"/><rect x="4" y="9" width="24" height="16" rx="1.5" {line}/>'
            f'<circle cx="10.5" cy="17" r="2.6" fill="{ok}.8)" stroke="none"/><path d="M16 17h8" {line}/>'
        ),
        "shield": (
            f'<path d="M16 4l11 4v8c0 7-5 11-11 13C10 27 5 23 5 16V8z" fill="{ok}.4)"/><path d="M16 4l11 4v8c0 7-5 11-11 13C10 27 5 23 5 16V8z" {line}/>'
            f'<path d="M11 16l4 4 6-7" class="l" stroke-width="1.8" stroke-linejoin="round" stroke-linecap="round"/>'
        ),
    }
    for name, shape in shapes.items():
        doc = f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32" aria-hidden="true"><style>{style}</style>{shape}</svg>\n'
        (ART / f"icon-{name}.svg").write_text(doc)


def terminal():
    """Recolors svg-term's render of install.cast as the site's code block: ink ground, paper text.

    npx svg-term-cli@2.1.1 --in docs/art/install.cast --out docs/art/install.svg --window --width 92 --height 29 --padding 12
    """
    file = ART / "install.svg"
    doc = file.read_text()
    dark = PALETTES["dark"]
    recolor = {
        "#070a10": PALETTES["light"]["ink"], "#0c1119": PALETTES["light"]["ink"],
        "#edf1f7": PALETTES["light"]["paper"], "#45d48a": dark["ok"], "#56637a": "#7f725e",
        "#ff5f58": "#5b4e3e", "#ffbd2e": "#5b4e3e", "#18c132": "#5b4e3e",
        # an unrecolored run (already paper) stays as it is
    }
    for theirs, ours in recolor.items():
        doc = doc.replace(theirs, ours)
    doc = re.sub(r'rx="\d+" ry="\d+"', 'rx="6" ry="6"', doc, count=1)
    file.write_text(doc)


# ---- the tile and the PNGs ---------------------------------------------------------------------------

def chrome():
    for found in [os.environ.get("CHROME"), shutil.which("google-chrome"), shutil.which("chromium"), "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"]:
        if found and Path(found).exists():
            return found
    raise SystemExit("build.py: no Chrome found; set CHROME to its binary")


def png(doc, w, h, path):
    """Screenshots an SVG at w by h CSS pixels."""
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


def tile(rounded, detail=True):
    """The app icon: the mark on a sheet of paper, with an engraver's hairline around its jobs and, in the large sizes, hatching inside the box."""
    pal = PALETTES["light"]
    corner = ' rx="3.5"' if rounded else ""
    hatching = ""
    if detail:
        hatching = f'<path d="{"".join(f"M3 {y / 2}h10" for y in range(7, 26))}" stroke="{pal["ink"]}" stroke-width=".08" opacity=".35"/>'
    frame = 'fill="' + pal["ink"] + '"'
    return (
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><rect width="16" height="16"{corner} fill="{pal["paper"]}"/>'
        f'{hatching}{mark_paths(frame, pal["ink"])}</svg>\n'
    )


def social():
    """The repository's social preview: the site's open graph card, with the box engraved on its plate."""
    pal = PALETTES["light"]
    W, H = 1280, 640
    px, py, pw, ph = 28, 28, W - 56, H - 56
    iso = Iso(934, 322, 24)
    body = [laid_paper(pal, W, H, 0), plate_frame(pal, px, py, pw, ph)]
    EXTRA["clip"] = f'<clipPath id="plate"><rect x="{px}" y="{py}" width="{pw}" height="{ph}" rx="3"/></clipPath>'
    back, front = case_layers(iso, pal)
    _, still = jobs_markup(iso, pal)
    body.append(f'<g clip-path="url(#plate)">{back}{still}{front}</g>')
    body.append(plate_mark(pal, px, py, pw, ph))
    body.append(lockup(pal["ink"], 82, 76)[0])
    for i, line in enumerate(HEADLINE):
        p, _ = text(display, line, 66, 80, 316 + i * 72, pal["ink"], tracking=-0.01)
        body.append(p)
    return document(W, H, "boxrunner: CI on the box you already pay for.", "".join(body))


def brand():
    pal = PALETTES["light"]
    favicon = tile(rounded=True, detail=False)
    write(SITE / "favicon.svg", favicon)
    uri = "data:image/svg+xml," + urllib.parse.quote(favicon.strip(), safe=" =/:.-")
    write(WORKER / "favicon.ts", f"// Drawn by docs/art/build.py, the same as site/favicon.svg.\nexport const favicon = '{uri}';\n")
    write(SITE / "mark.svg", f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="2 2 12 12" aria-hidden="true">{mark_paths(f"fill=\"{MARK_FRAME}\"")}</svg>\n')
    style = f'.box{{fill:{pal["ink"]}}}@media (prefers-color-scheme:dark){{.box{{fill:{PALETTES["dark"]["ink"]}}}}}'
    write(ART / "logo.svg", document(96, 96, "boxrunner", mark(0, 0, 96, 'class="box"'), style))
    for name, fg in [("logo-lockup-dark.svg", PALETTES["dark"]["ink"]), ("logo-lockup-light.svg", pal["ink"])]:
        body, w = lockup(fg)
        write(ART / name, document(w, 34, "boxrunner", body))
    for size, rounded, name in [(180, False, "apple-touch-icon.png"), (192, True, "icon-192.png"), (512, True, "icon-512.png")]:
        png(tile(rounded), size, size, SITE / name)
    png(social(), 1280, 640, ART / "social-preview.png")


if __name__ == "__main__":
    for t in ("light", "dark"):
        hero(t)
        bench(t)
        flow(t)
    icons()
    terminal()
    brand()
