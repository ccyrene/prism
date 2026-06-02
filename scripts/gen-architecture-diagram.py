#!/usr/bin/env python3
"""Render the Prism diagrams as clean, light, GitHub/LinkedIn-friendly PNGs.

PNGs (not SVG) so fonts are baked in and they render identically everywhere.
Light background, indigo = Prism, restrained palette, Liberation Sans.
Outputs docs/architecture.png and docs/facets.png.
Run: python3 scripts/gen-architecture-diagram.py
"""
import glob, os
import matplotlib
matplotlib.use("Agg")
import matplotlib.font_manager as fm
import matplotlib.pyplot as plt
from matplotlib.patches import FancyBboxPatch, FancyArrowPatch

HERE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DOCS = os.path.join(HERE, "docs")

PRISM = "#4f46e5"; PRISM_D = "#4338ca"; INK = "#0f172a"; SUB = "#64748b"
TEAL = "#0d9488"; ACCENT = "#ea580c"; SLATE = "#94a3b8"
INDIGO_50 = "#eef2ff"; INDIGO_100 = "#c7d2fe"; VIOLET_50 = "#f5f3ff"
LINE = "#cbd5e1"; PANEL = "#f8fafc"; RESV = "#e2e8f0"


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


def box(ax, cx, cy, w, h, face, edge, lw=1.5, r=0.06, ls="-"):
    ax.add_patch(FancyBboxPatch((cx - w/2, cy - h/2), w, h,
                 boxstyle=f"round,pad=0,rounding_size={r*min(w,h)}",
                 fc=face, ec=edge, lw=lw, ls=ls, mutation_aspect=1, zorder=2))


def arrow(ax, x1, y1, x2, y2, color=SLATE, lw=2.0):
    ax.add_patch(FancyArrowPatch((x1, y1), (x2, y2), arrowstyle="-|>",
                 mutation_scale=16, color=color, lw=lw, zorder=1, shrinkA=0, shrinkB=0))


def text(ax, x, y, s, size, color=INK, weight="normal", mono=False, ha="center", va="center"):
    ax.text(x, y, s, fontsize=size, color=color, fontweight=weight, ha=ha, va=va,
            family=(MONO if mono else SANS), zorder=3)


def accent_card(ax, cx, cy, w, h, color, name, role, ls="-"):
    box(ax, cx, cy, w, h, "white", (color if ls == "--" else LINE), lw=1.4, r=0.16, ls=ls)
    ax.add_patch(FancyBboxPatch((cx - w/2 + 0.7, cy - h/2 + 0.7), 1.5, h - 1.4,
                 boxstyle="round,pad=0,rounding_size=0.45", fc=color, ec=color, zorder=3))
    return cx - w/2 + 4  # left text anchor


# ---------------------------------------------------------------------------
def draw_architecture():
    fig, ax = plt.subplots(figsize=(13.4, 7))
    ax.set_xlim(0, 134); ax.set_ylim(0, 70); ax.set_aspect("equal"); ax.axis("off")

    text(ax, 3, 67, "Prism — a workload-identity bus for Kubernetes-aware eBPF", 19, INK, "bold", ha="left")
    text(ax, 3, 62.5, "prismd tags each workload once; every in-kernel subsystem reads the same identity  O(1).",
         12.5, SUB, ha="left")

    box(ax, 115, 32, 34, 48, PANEL, "#e8edf3", lw=1.2, r=0.04)
    text(ax, 115, 53.8, "in-kernel consumers · read O(1)", 10.5, SUB, "bold")

    box(ax, 16, 40, 24, 16, INDIGO_50, INDIGO_100)
    text(ax, 16, 43, "Kubernetes API", 13, INK, "bold")
    text(ax, 16, 38.5, "pods · labels", 11, SUB)

    box(ax, 48, 40, 26, 20, PRISM, PRISM_D, lw=2)
    text(ax, 48, 44.5, "prismd", 15, "white", "bold")
    text(ax, 48, 40.6, "per-node daemon", 10.5, "#dbe2ff")
    text(ax, 48, 36.5, "1 identity / workload", 10.5, "#dbe2ff")

    box(ax, 80, 40, 28, 26, VIOLET_50, PRISM, lw=3)
    text(ax, 80, 47.5, "prism_identity", 14, PRISM_D, "bold", mono=True)
    text(ax, 80, 43.8, "the shared bus", 11, SUB)
    text(ax, 80, 39.5, "cgroup-id → 24-bit id", 10.5, INK, mono=True)
    text(ax, 80, 35, "read-only to consumers", 10, SUB)

    arrow(ax, 28.2, 40, 34.8, 40); text(ax, 31.5, 42, "watch", 9.5, SUB)
    arrow(ax, 61.2, 40, 65.8, 40, color=PRISM); text(ax, 63.4, 42, "writes", 9.5, PRISM)

    cons = [(PRISM, "sched", "CPU priority"), (TEAL, "net", "packet allow / deny"),
            (SLATE, "trace", "tag events"), (ACCENT, "sec", "LSM allow / deny")]
    for (col, name, role), cy in zip(cons, [47, 36.5, 26, 15.5]):
        lx = accent_card(ax, 115, cy, 30, 8.5, col, name, role)
        text(ax, lx, cy + 1.4, name, 12.5, INK, "bold", mono=True, ha="left")
        text(ax, lx, cy - 1.9, role, 9.5, SUB, ha="left")
        arrow(ax, 94.4, 40, 99.6, cy, color="#b8c2d4", lw=1.6)

    fig.tight_layout(pad=0.4)
    fig.savefig(os.path.join(DOCS, "architecture.png"), dpi=150, bbox_inches="tight", pad_inches=0.15)
    print("wrote docs/architecture.png")


# ---------------------------------------------------------------------------
def draw_facets():
    fig, ax = plt.subplots(figsize=(13, 7))
    ax.set_xlim(0, 122); ax.set_ylim(0, 70); ax.set_aspect("equal"); ax.axis("off")

    text(ax, 3, 67, "One identity · many facets", 19, INK, "bold", ha="left")
    text(ax, 3, 62.5, "prismd stamps a per-subsystem flag bit into the one identity; any consumer reads any bit to compose.",
         12.5, SUB, ha="left")

    # central identity (hero)
    cx, cy = 27, 36
    box(ax, cx, cy, 42, 38, VIOLET_50, PRISM, lw=3)
    text(ax, cx, cy + 13.5, "one identity", 12, SUB)
    text(ax, cx, cy + 7.5, "0x01F4A2", 23, PRISM_D, "bold", mono=True)
    text(ax, cx, cy + 2, "24-bit · derived once", 10.5, SUB)
    # flags bit-strip (low byte): bits 0-2 = facets, 3-7 reserved
    text(ax, cx, cy - 3.6, "flags (u32)", 10, INK, "bold", mono=True)
    bit_cols = [TEAL, PRISM, SLATE, RESV, RESV, RESV, RESV, RESV]
    n = len(bit_cols); cw = 3.6; x0 = cx - n * cw / 2
    for i, col in enumerate(bit_cols):
        bx = x0 + i * cw
        box(ax, bx + cw / 2, cy - 8.5, cw - 0.7, 4.4, col, "#cbd5e1", lw=0.8, r=0.12)
        text(ax, bx + cw / 2, cy - 8.5, str(i), 8, ("white" if i < 3 else SUB))
    text(ax, cx, cy - 13.5, "bits 0–2 = facets   ·   ~19 bits reserved for more", 9.5, SUB)

    # facet cards on the right
    facets = [
        (TEAL,   "net",     "PRISM_FLAG_NET_POLICY",   "a network-policy rule exists", "-"),
        (PRISM,  "sched",   "PRISM_FLAG_SCHED_MANAGED", "a scheduler manages it",       "-"),
        (SLATE,  "observe", "PRISM_FLAG_OBSERVED",      "an observer is tracking it",   "-"),
        (ACCENT, "your facet", "reserved bit → yours",  "+ N more — compose freely",    "--"),
    ]
    fx, fw = 90, 50
    for (col, name, flag, role, ls), fy in zip(facets, [55, 43, 31, 17]):
        lx = accent_card(ax, fx, fy, fw, 10, col, name, role, ls=ls)
        text(ax, lx, fy + 2.4, name, 12.5, INK, "bold", ha="left")
        text(ax, lx, fy - 0.3, flag, 10, col, "bold", mono=True, ha="left")
        text(ax, lx, fy - 3.2, role, 9.5, SUB, ha="left")
        arrow(ax, 48.5, 36, 64.4, fy, color="#b8c2d4", lw=1.6)

    text(ax, 3, 5.5,
         "composability:  a consumer asks \"is this workload net-policy'd AND scheduler-managed?\" by reading flags — zero new plumbing.",
         10.5, SUB, ha="left")

    fig.tight_layout(pad=0.4)
    fig.savefig(os.path.join(DOCS, "facets.png"), dpi=150, bbox_inches="tight", pad_inches=0.15)
    print("wrote docs/facets.png")


if __name__ == "__main__":
    draw_architecture()
    draw_facets()
