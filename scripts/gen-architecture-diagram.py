#!/usr/bin/env python3
"""Render docs/architecture.png — a clean, light, GitHub/LinkedIn-friendly diagram.

A PNG (not SVG) so fonts are baked in and it renders identically everywhere.
Same visual system as bench/plot.py: light background, indigo = Prism, restrained
palette, Liberation Sans. Run: python3 scripts/gen-architecture-diagram.py
"""
import glob, os
import matplotlib
matplotlib.use("Agg")
import matplotlib.font_manager as fm
import matplotlib.pyplot as plt
from matplotlib.patches import FancyBboxPatch, FancyArrowPatch

HERE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(HERE, "docs", "architecture.png")

PRISM = "#4f46e5"; PRISM_D = "#4338ca"; INK = "#0f172a"; SUB = "#64748b"
TEAL = "#0d9488"; ACCENT = "#ea580c"; SLATE = "#94a3b8"
INDIGO_50 = "#eef2ff"; INDIGO_100 = "#c7d2fe"; VIOLET_50 = "#f5f3ff"
LINE = "#cbd5e1"; PANEL = "#f8fafc"


def _fonts():
    for d in ("/usr/share/fonts", "/usr/local/share/fonts", os.path.expanduser("~/.fonts")):
        for pat in ("LiberationSans-*.ttf", "LiberationMono-*.ttf", "Arimo-*.ttf"):
            for f in glob.glob(os.path.join(d, "**", pat), recursive=True):
                try: fm.fontManager.addfont(f)
                except Exception: pass
    have = {f.name for f in fm.fontManager.ttflist}
    sans = next((s for s in ("Liberation Sans", "Arimo", "Helvetica", "DejaVu Sans") if s in have), "DejaVu Sans")
    mono = next((m for m in ("Liberation Mono", "DejaVu Sans Mono") if m in have), "monospace")
    return sans, mono


SANS, MONO = _fonts()
plt.rcParams.update({"font.family": SANS, "savefig.facecolor": "white", "figure.facecolor": "white"})


def box(ax, cx, cy, w, h, face, edge, lw=1.5, r=0.06):
    ax.add_patch(FancyBboxPatch((cx - w/2, cy - h/2), w, h,
                 boxstyle=f"round,pad=0,rounding_size={r*min(w,h)}",
                 fc=face, ec=edge, lw=lw, mutation_aspect=1, zorder=2))


def arrow(ax, x1, y1, x2, y2, color=SLATE, lw=2.0):
    ax.add_patch(FancyArrowPatch((x1, y1), (x2, y2), arrowstyle="-|>",
                 mutation_scale=16, color=color, lw=lw, zorder=1,
                 shrinkA=0, shrinkB=0))


def text(ax, x, y, s, size, color=INK, weight="normal", mono=False, ha="center", va="center"):
    ax.text(x, y, s, fontsize=size, color=color, fontweight=weight, ha=ha, va=va,
            family=(MONO if mono else SANS), zorder=3)


fig, ax = plt.subplots(figsize=(13.4, 7))
ax.set_xlim(0, 134); ax.set_ylim(0, 70); ax.set_aspect("equal"); ax.axis("off")

# title
text(ax, 3, 67, "Prism — a workload-identity bus for Kubernetes-aware eBPF",
     19, INK, "bold", ha="left")
text(ax, 3, 62.5, "prismd tags each workload once; every in-kernel subsystem reads the same identity  O(1).",
     12.5, SUB, ha="left")

# in-kernel substrate band (behind the consumers)
box(ax, 115, 32, 34, 48, PANEL, "#e8edf3", lw=1.2, r=0.04)
text(ax, 115, 53.8, "in-kernel consumers · read O(1)", 10.5, SUB, "bold")

# 1. Kubernetes API
box(ax, 16, 40, 24, 16, INDIGO_50, INDIGO_100)
text(ax, 16, 43, "Kubernetes API", 13, INK, "bold")
text(ax, 16, 38.5, "pods · labels", 11, SUB)

# 2. prismd (the tagger — emphasized)
box(ax, 48, 40, 26, 20, PRISM, PRISM_D, lw=2)
text(ax, 48, 44.5, "prismd", 15, "white", "bold")
text(ax, 48, 40.6, "per-node daemon", 10.5, "#dbe2ff")
text(ax, 48, 36.5, "1 identity / workload", 10.5, "#dbe2ff")

# 3. the bus (hero)
box(ax, 80, 40, 28, 26, VIOLET_50, PRISM, lw=3)
text(ax, 80, 47.5, "prism_identity", 14, PRISM_D, "bold", mono=True)
text(ax, 80, 43.8, "the shared bus", 11, SUB)
text(ax, 80, 39.5, "cgroup-id → 24-bit id", 10.5, INK, mono=True)
text(ax, 80, 35, "read-only to consumers", 10, SUB)

# arrows along the spine
arrow(ax, 28.2, 40, 34.8, 40); text(ax, 31.5, 42, "watch", 9.5, SUB)
arrow(ax, 61.2, 40, 65.8, 40, color=PRISM); text(ax, 63.4, 42, "writes", 9.5, PRISM)

# 4. consumers (right), fanned out from the bus
cons = [(PRISM, "sched", "CPU priority"),
        (TEAL,  "net",   "packet allow / deny"),
        (SLATE, "trace", "tag events"),
        (ACCENT, "sec",  "LSM allow / deny")]
ys = [47, 36.5, 26, 15.5]
for (col, name, role), cy in zip(cons, ys):
    box(ax, 115, cy, 30, 8.5, "white", LINE, lw=1.3, r=0.18)
    ax.add_patch(FancyBboxPatch((115 - 15 + 0.7, cy - 8.5/2 + 0.7), 1.5, 8.5 - 1.4,
                 boxstyle="round,pad=0,rounding_size=0.45", fc=col, ec=col, zorder=3))
    text(ax, 115 - 10.5, cy + 1.4, name, 12.5, INK, "bold", mono=True, ha="left")
    text(ax, 115 - 10.5, cy - 1.9, role, 9.5, SUB, ha="left")
    arrow(ax, 94.4, 40, 99.6, cy, color="#b8c2d4", lw=1.6)

fig.tight_layout(pad=0.4)
fig.savefig(OUT, dpi=150, bbox_inches="tight", pad_inches=0.15)
print("wrote", OUT)
