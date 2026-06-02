#!/usr/bin/env python3
"""Generate Prism eval plots from the benchmark CSV/JSON outputs.

Clean, light, print-friendly style: a restrained palette (indigo = Prism, slate =
baseline), Liberation Sans where available, despined axes and minimal gridlines.
"""
import csv, glob, json, os
import matplotlib
matplotlib.use("Agg")
import matplotlib.font_manager as fm
import matplotlib.pyplot as plt
import numpy as np

R = os.path.join(os.path.dirname(__file__), "results")

# ---- palette -------------------------------------------------------------
PRISM  = "#4f46e5"   # indigo — Prism (the hero series)
BASE   = "#94a3b8"   # slate  — baseline / "the old slow way" (recedes)
TEAL   = "#0d9488"   # teal   — third series
ACCENT = "#ea580c"   # warm orange — speedup callouts, used sparingly
INK    = "#0f172a"   # near-black slate — titles / labels
SUB    = "#64748b"   # secondary text
GRID   = "#e5e9f0"   # very light grid
BG     = "#ffffff"


def _clean_font():
    """Prefer a Helvetica-like sans (Liberation/Arimo) over matplotlib's DejaVu."""
    pats = ("LiberationSans-*.ttf", "Arimo-*.ttf")
    for d in ("/usr/share/fonts", "/usr/local/share/fonts", os.path.expanduser("~/.fonts")):
        for pat in pats:
            for f in glob.glob(os.path.join(d, "**", pat), recursive=True):
                try: fm.fontManager.addfont(f)
                except Exception: pass
    have = {f.name for f in fm.fontManager.ttflist}
    for fam in ("Liberation Sans", "Arimo", "Helvetica", "Arial", "DejaVu Sans"):
        if fam in have:
            return fam
    return "DejaVu Sans"


plt.rcParams.update({
    "figure.facecolor": BG, "axes.facecolor": BG, "savefig.facecolor": BG,
    "font.family": _clean_font(), "font.size": 12,
    "text.color": INK, "axes.labelcolor": SUB, "axes.titlecolor": INK,
    "xtick.color": SUB, "ytick.color": SUB, "axes.edgecolor": "#cbd5e1",
    "axes.linewidth": 1.0, "grid.color": GRID, "grid.linewidth": 1.0,
    "legend.frameon": False, "figure.dpi": 150, "svg.fonttype": "none",
})


def _style(ax, title, subtitle=None, ygrid=True):
    """Despine, light grid, left-aligned title + optional subtitle."""
    for s in ("top", "right"):
        ax.spines[s].set_visible(False)
    for s in ("left", "bottom"):
        ax.spines[s].set_color("#cbd5e1")
    ax.tick_params(length=0)
    if ygrid:
        ax.set_axisbelow(True)
        ax.grid(True, axis="y", color=GRID, lw=1.0)
    ax.set_title(title, loc="left", fontsize=15, fontweight="bold", pad=18 if subtitle else 10)
    if subtitle:
        ax.text(0, 1.02, subtitle, transform=ax.transAxes, fontsize=10.5,
                color=SUB, va="bottom")


def load_resolution():
    p, b = [], []
    with open(os.path.join(R, "resolution_samples.csv")) as f:
        for row in csv.DictReader(f):
            p.append(float(row["prism_ns_per_op"]))
            b.append(float(row["baseline_ns_per_op"]))
    return np.array(p), np.array(b)


def cdf(ax, data, color, label):
    s = np.sort(data)
    y = np.arange(1, len(s) + 1) / len(s) * 100
    ax.step(s, y, where="post", color=color, lw=2.6, label=label)
    ax.scatter([np.percentile(s, 99)], [99], color=color, s=40, zorder=5, ec=BG, lw=1.2)


def money_plot():
    p, b = load_resolution()
    fig, ax = plt.subplots(figsize=(8.8, 4.6))
    cdf(ax, p, PRISM, f"Prism — O(1) map lookup  (p50 = {np.median(p):.0f} ns)")
    cdf(ax, b, BASE,  f"Baseline — cgroup-path classify  (p50 = {np.median(b):.0f} ns)")
    ax.set_xscale("log")
    ax.set_xlabel("per-decision identity-resolution latency — ns/op (log)")
    ax.set_ylabel("cumulative %")
    ax.set_ylim(0, 104)
    _style(ax, "Identity resolution latency",
           "Prism vs scx_layered-style classification (Go control-plane path)", ygrid=False)
    ax.grid(True, which="major", color=GRID, lw=1.0); ax.set_axisbelow(True)
    ax.legend(loc="center right", fontsize=11)
    fig.tight_layout()
    fig.savefig(os.path.join(R, "money_plot_cdf.png"))
    print("wrote money_plot_cdf.png")


def scale_plot():
    counts, lookup, base, alloc = [], [], [], []
    with open(os.path.join(R, "scale.csv")) as f:
        for row in csv.DictReader(f):
            counts.append(int(row["live_identities"]))
            lookup.append(float(row["lookup_ns_per_op"]))
            base.append(float(row["baseline_ns_per_op"]))
            alloc.append(float(row["alloc_ns_per_op"]))
    fig, ax = plt.subplots(figsize=(8.8, 4.6))
    ax.axvspan(64, 1024, color=PRISM, alpha=0.06, lw=0)
    ax.plot(counts, lookup, "-o", color=PRISM, lw=2.6, ms=6, label="Prism lookup (Go map)")
    ax.plot(counts, base, "-o", color=BASE, lw=2.2, ms=5, label="Baseline classify (population-independent)")
    ax.plot(counts, alloc, "--o", color=TEAL, lw=1.8, ms=4, label="Prism allocate (control-plane)")
    ax.set_xscale("log"); ax.set_yscale("log")
    ax.set_xlabel("live identities in the map (log)"); ax.set_ylabel("ns/op (log)")
    ax.text(256, ax.get_ylim()[1] * 0.55, "per-node region\n~64–1024 workloads",
            color=SUB, fontsize=9.5, ha="center", va="top")
    _style(ax, "Scale sweep", "lookup vs classify as the population grows", ygrid=False)
    ax.grid(True, which="major", color=GRID, lw=1.0); ax.set_axisbelow(True)
    ax.legend(loc="upper left", fontsize=10.5)
    fig.tight_layout()
    fig.savefig(os.path.join(R, "scale.png"))
    print("wrote scale.png")


def layers_bar():
    d = json.load(open(os.path.join(R, "results.json")))
    go_prism = next(s for s in d["scenarios"] if s["scenario"] == "resolution_prism")["p50"]
    go_base = next(s for s in d["scenarios"] if s["scenario"] == "resolution_baseline")["p50"]
    c = json.load(open(os.path.join(R, "native.json")))
    c_prism, c_base = c["prism_ns"], c["baseline_ns"]
    fig, ax = plt.subplots(figsize=(8.0, 4.8))
    groups = ["Go control-plane\n(userspace map)", "Native C\n(≈ kernel BPF map)"]
    prism_vals = [go_prism, c_prism]; base_vals = [go_base, c_base]
    x = np.arange(len(groups)); w = 0.34
    b1 = ax.bar(x - w/2, prism_vals, w, color=PRISM, label="Prism — O(1) lookup", zorder=3)
    b2 = ax.bar(x + w/2, base_vals, w, color=BASE, label="Baseline — classify", zorder=3)
    ax.set_yscale("log"); ax.set_ylabel("ns/op (log)")
    ax.set_xticks(x); ax.set_xticklabels(groups, fontsize=11, color=INK)
    for bars in (b1, b2):
        for r in bars:
            h = r.get_height()
            ax.annotate(f"{h:.0f}" if h >= 10 else f"{h:.1f}",
                        (r.get_x() + r.get_width()/2, h), ha="center", va="bottom",
                        color=INK, fontsize=10, xytext=(0, 2), textcoords="offset points")
    for i, (pv, bv) in enumerate(zip(prism_vals, base_vals)):
        ax.text(i, max(base_vals) * 2.3, f"{bv/pv:.0f}×", ha="center",
                color=ACCENT, fontsize=18, fontweight="bold")
    ax.set_ylim(1, max(base_vals) * 4.5)
    _style(ax, "Identity resolution: Prism vs baseline",
           "per-decision cost by layer — lower is better")
    ax.legend(loc="upper center", bbox_to_anchor=(0.5, -0.16), ncol=2, fontsize=11)
    fig.tight_layout()
    fig.savefig(os.path.join(R, "layers_bar.png"))
    print("wrote layers_bar.png")


def scale_sinks_plot():
    pop, sim, fast, comp = [], [], [], []
    with open(os.path.join(R, "scale_sinks.csv")) as f:
        for row in csv.DictReader(f):
            pop.append(int(row["population"])); sim.append(float(row["sim_ns"]))
            fast.append(float(row["fast_ns"])); comp.append(float(row["compact_ns"]))
    fig, ax = plt.subplots(figsize=(8.8, 4.8))
    ax.axvspan(64, 256, color=PRISM, alpha=0.06, lw=0)
    ax.plot(pop, sim, "-o", color=BASE, lw=2.0, ms=5, label="SimSink — map + RWMutex")
    ax.plot(pop, fast, "-o", color=TEAL, lw=2.2, ms=5, label="FastSink — lock-free pointer (2 misses)")
    ax.plot(pop, comp, "-o", color=PRISM, lw=2.8, ms=6, label="CompactSink — inline seqlock (1 miss)")
    ax.set_xscale("log")
    ax.set_xlabel("live identities in ONE map (log)"); ax.set_ylabel("lookup ns/op")
    ax.text(128, ax.get_ylim()[1] * 0.96, "per-node\n≤256 pods", color=SUB,
            fontsize=9.5, ha="center", va="top")
    _style(ax, "Scale consistency",
           "inline storage keeps the curve flat into the millions")
    ax.legend(loc="upper left", fontsize=10.5)
    fig.tight_layout()
    fig.savefig(os.path.join(R, "scale_sinks.png"))
    print("wrote scale_sinks.png")


if __name__ == "__main__":
    money_plot()
    scale_plot()
    layers_bar()
    scale_sinks_plot()
