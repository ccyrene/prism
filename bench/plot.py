#!/usr/bin/env python3
"""Generate Prism eval plots (dark theme) from the benchmark CSV/JSON outputs."""
import csv, json, os
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np

R = os.path.join(os.path.dirname(__file__), "results")
NAVY = "#0a1929"; PANEL = "#132c44"; TEXT = "#d6e2f0"; DIM = "#8ba2bc"
BLUE = "#5a8ef0"; GREEN = "#00d68f"; AMBER = "#ffb547"; MAG = "#e94f8a"

plt.rcParams.update({
    "figure.facecolor": NAVY, "axes.facecolor": NAVY, "savefig.facecolor": NAVY,
    "text.color": TEXT, "axes.labelcolor": TEXT, "xtick.color": DIM, "ytick.color": DIM,
    "axes.edgecolor": "#2a4a6a", "grid.color": "#1d3a57", "font.size": 11,
})


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
    ax.step(s, y, where="post", color=color, lw=2.4, label=label)
    p99 = np.percentile(s, 99)
    ax.scatter([p99], [99], color=color, s=45, zorder=5)


def money_plot():
    p, b = load_resolution()
    fig, ax = plt.subplots(figsize=(8.6, 4.4))
    cdf(ax, p, GREEN, f"Prism  O(1) map lookup  (p50={np.median(p):.1f} ns)")
    cdf(ax, b, AMBER, f"Baseline  cgroup-path classify  (p50={np.median(b):.0f} ns)")
    ax.set_xscale("log")
    ax.set_xlabel("per-decision identity-resolution latency  (ns/op, log scale)")
    ax.set_ylabel("cumulative probability (%)")
    ax.set_ylim(0, 102)
    ax.axhline(99, color="#2a4a6a", ls="--", lw=0.8)
    ax.text(np.median(p), 50, "  read y=99% → drop to x", color=DIM, fontsize=8.5, va="center")
    ax.set_title("Money plot — Prism vs scx_layered-style classification (Go control-plane path)",
                 color=TEXT, fontsize=12, pad=12)
    ax.grid(True, which="both", alpha=0.35)
    ax.legend(facecolor=PANEL, edgecolor="#2a4a6a", labelcolor=TEXT, loc="center right")
    fig.tight_layout()
    fig.savefig(os.path.join(R, "money_plot_cdf.png"), dpi=130)
    print("wrote money_plot_cdf.png")


def scale_plot():
    counts, lookup, base, alloc = [], [], [], []
    with open(os.path.join(R, "scale.csv")) as f:
        for row in csv.DictReader(f):
            counts.append(int(row["live_identities"]))
            lookup.append(float(row["lookup_ns_per_op"]))
            base.append(float(row["baseline_ns_per_op"]))
            alloc.append(float(row["alloc_ns_per_op"]))
    fig, ax = plt.subplots(figsize=(8.6, 4.4))
    ax.plot(counts, lookup, "-o", color=GREEN, lw=2.2, label="Prism lookup (Go map)")
    ax.plot(counts, base, "-o", color=AMBER, lw=2.2, label="Baseline classify (population-independent)")
    ax.plot(counts, alloc, "-o", color=BLUE, lw=1.8, ls="--", label="Prism allocate (control-plane)")
    ax.set_xscale("log"); ax.set_yscale("log")
    ax.set_xlabel("live identities in the map (log)")
    ax.set_ylabel("ns / op (log)")
    ax.axvspan(64, 1024, color=GREEN, alpha=0.07)
    ax.text(230, ax.get_ylim()[1]*0.5, "per-node\noperating region\n(~64–1024 workloads)",
            color=DIM, fontsize=8.5, ha="center", va="top")
    ax.set_title("Scale sweep — lookup vs classify as the population grows", color=TEXT, fontsize=12, pad=12)
    ax.grid(True, which="both", alpha=0.35)
    ax.legend(facecolor=PANEL, edgecolor="#2a4a6a", labelcolor=TEXT, loc="upper left")
    fig.tight_layout()
    fig.savefig(os.path.join(R, "scale.png"), dpi=130)
    print("wrote scale.png")


def layers_bar():
    # Go sim vs native C, prism vs baseline (ns/op, log)
    d = json.load(open(os.path.join(R, "results.json")))
    go_prism = next(s for s in d["scenarios"] if s["scenario"] == "resolution_prism")["p50"]
    go_base = next(s for s in d["scenarios"] if s["scenario"] == "resolution_baseline")["p50"]
    # native C microbench numbers (passed via env or hard-read file)
    c = json.load(open(os.path.join(R, "native.json")))
    c_prism, c_base = c["prism_ns"], c["baseline_ns"]
    fig, ax = plt.subplots(figsize=(7.6, 4.4))
    groups = ["Go control-plane\n(userspace map)", "Native C\n(≈ kernel BPF map)"]
    prism_vals = [go_prism, c_prism]; base_vals = [go_base, c_base]
    x = np.arange(len(groups)); w = 0.36
    b1 = ax.bar(x - w/2, prism_vals, w, color=GREEN, label="Prism (O(1) lookup)")
    b2 = ax.bar(x + w/2, base_vals, w, color=AMBER, label="Baseline (classify)")
    ax.set_yscale("log"); ax.set_ylabel("ns / op (log)")
    ax.set_xticks(x); ax.set_xticklabels(groups)
    for bars in (b1, b2):
        for r in bars:
            ax.annotate(f"{r.get_height():.0f}" if r.get_height() >= 10 else f"{r.get_height():.1f}",
                        (r.get_x()+r.get_width()/2, r.get_height()), ha="center", va="bottom",
                        color=TEXT, fontsize=9)
    for i, (pv, bv) in enumerate(zip(prism_vals, base_vals)):
        ax.text(i, max(base_vals)*2.0, f"{bv/pv:.0f}×", ha="center", color=MAG, fontsize=15, fontweight="bold")
    ax.set_ylim(1, max(base_vals)*4)
    ax.set_title("Identity resolution: Prism vs baseline, by layer", color=TEXT, fontsize=12, pad=18)
    ax.grid(True, which="both", axis="y", alpha=0.3)
    ax.legend(facecolor=PANEL, edgecolor="#2a4a6a", labelcolor=TEXT, loc="lower center", ncol=2)
    fig.tight_layout()
    fig.savefig(os.path.join(R, "layers_bar.png"), dpi=130)
    print("wrote layers_bar.png")


if __name__ == "__main__":
    money_plot()
    scale_plot()
    layers_bar()


def scale_sinks_plot():
    import csv as _csv
    pop, sim, fast, comp = [], [], [], []
    with open(os.path.join(R, "scale_sinks.csv")) as f:
        for row in _csv.DictReader(f):
            pop.append(int(row["population"])); sim.append(float(row["sim_ns"]))
            fast.append(float(row["fast_ns"])); comp.append(float(row["compact_ns"]))
    fig, ax = plt.subplots(figsize=(8.6, 4.6))
    ax.plot(pop, sim, "-o", color=DIM, lw=1.8, label="SimSink  map + RWMutex")
    ax.plot(pop, fast, "-o", color=AMBER, lw=2.0, label="FastSink  pointer, lock-free (2 misses)")
    ax.plot(pop, comp, "-o", color=GREEN, lw=2.6, label="CompactSink  inline seqlock (1 miss)")
    ax.set_xscale("log")
    ax.set_xlabel("live identities in ONE map (log)"); ax.set_ylabel("lookup ns/op")
    ax.axvspan(64, 256, color=BLUE, alpha=0.10)
    ax.text(140, ax.get_ylim()[1]*0.92, "real per-node\n(<=256 pods)\nin-cache, flat",
            color=BLUE, fontsize=8.5, ha="center", va="top")
    ax.axvspan(1_000_000, 4_000_000, color=GREEN, alpha=0.06)
    ax.text(2_000_000, comp[-1]+18, "millions: ~flat\n(57->66 ns,\npast-LLC floor)",
            color=GREEN, fontsize=8.5, ha="center")
    ax.set_title("Scale consistency — inline storage flattens the curve into the millions",
                 color=TEXT, fontsize=12, pad=12)
    ax.grid(True, which="both", alpha=0.33)
    ax.legend(facecolor=PANEL, edgecolor="#2a4a6a", labelcolor=TEXT, loc="upper left")
    fig.tight_layout(); fig.savefig(os.path.join(R, "scale_sinks.png"), dpi=130)
    print("wrote scale_sinks.png")


scale_sinks_plot()
