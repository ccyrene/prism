#!/usr/bin/env python3
"""Cross-ISA comparison from the merged sched-eval CSV (cross-isa-compare.sh).

Reads the merged long-format trial CSV (same schema as run-sched-eval.sh):
    scheduler,arch,kernel,workload,trial,p50_us,p99_us,p999_us
with rows from MULTIPLE arches (x86_64 + aarch64).

Writes:
  * <out>/cross_isa_summary.csv — per (arch, scheduler) median + bootstrap 95%
    CI on the median of each percentile (same methodology as stats.go).
  * <out>/cross_isa_money_plot.png — grouped bars: for each scheduler, the p99
    median on each arch side by side, so "the identity-aware win holds on both
    ISAs" is read at a glance.

Reuses the bootstrap/percentile from sched_eval_plot.py (imported if importable,
else inlined). Matplotlib optional. Usage: cross_isa_plot.py <merged.csv> <out>
"""
import csv
import math
import os
import random
import sys

# Reuse the canonical stats helpers if the sibling module is importable; else
# fall back to local copies (identical math) so this script is standalone.
try:
    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    from sched_eval_plot import percentile, bootstrap_median_ci  # noqa: E402
except Exception:  # noqa: BLE001
    BOOTSTRAP_SEED = 0x123456789ABCDEF

    def percentile(sorted_vals, p):
        n = len(sorted_vals)
        if n == 0:
            return float("nan")
        if n == 1:
            return sorted_vals[0]
        rank = (p / 100.0) * (n - 1)
        lo, hi = math.floor(rank), math.ceil(rank)
        if lo == hi:
            return sorted_vals[int(lo)]
        frac = rank - lo
        return sorted_vals[int(lo)] * (1 - frac) + sorted_vals[int(hi)] * frac

    def bootstrap_median_ci(values, resamples=2000, conf=0.95):
        n = len(values)
        if n < 3:
            s = sorted(values)
            return (s[0], s[-1]) if s else (float("nan"), float("nan"))
        rng = random.Random(BOOTSTRAP_SEED)
        meds = []
        for _ in range(resamples):
            sample = sorted(values[rng.randrange(n)] for _ in range(n))
            meds.append(percentile(sample, 50))
        meds.sort()
        alpha = (1 - conf) / 2
        return percentile(meds, alpha * 100), percentile(meds, (1 - alpha) * 100)

SCHED_ORDER = ["baseline", "scx_layered", "scx_prism"]
PCT_COLS = ["p50_us", "p99_us", "p999_us"]
PCT_LABEL = {"p50_us": "p50", "p99_us": "p99", "p999_us": "p99.9"}


def load(path):
    # groups[(arch, sched)][pct_col] = [values]
    groups = {}
    meta = {"workload": "?"}
    with open(path) as f:
        for r in csv.DictReader(f):
            meta["workload"] = r.get("workload", meta["workload"])
            # Skip rows from a wrong-schema CSV (e.g. a summary file) that lack
            # the trial columns — DictReader leaves missing fields as None.
            if r.get("arch") is None or r.get("scheduler") is None:
                continue
            key = (r["arch"], r["scheduler"])
            g = groups.setdefault(key, {c: [] for c in PCT_COLS})
            for c in PCT_COLS:
                try:
                    v = float(r[c])
                except (KeyError, ValueError, TypeError):
                    continue
                if v > 0:
                    g[c].append(v)
    return groups, meta


def summarize(groups):
    out = {}
    for key, cols in groups.items():
        s = {}
        for c, vals in cols.items():
            if not vals:
                s[c] = {"median": float("nan"), "lo": float("nan"),
                        "hi": float("nan"), "n": 0}
                continue
            sv = sorted(vals)
            lo, hi = bootstrap_median_ci(sv)
            s[c] = {"median": percentile(sv, 50), "lo": lo, "hi": hi, "n": len(vals)}
        out[key] = s
    return out


def arches(summary):
    return sorted({a for (a, _) in summary})


def scheds(summary):
    present = {s for (_, s) in summary}
    ordered = [s for s in SCHED_ORDER if s in present]
    return ordered + sorted(s for s in present if s not in SCHED_ORDER)


def write_summary(summary, out_dir):
    path = os.path.join(out_dir, "cross_isa_summary.csv")
    with open(path, "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["arch", "scheduler", "percentile", "median_us",
                    "ci95_low_us", "ci95_high_us", "n_trials"])
        for a in arches(summary):
            for s in scheds(summary):
                if (a, s) not in summary:
                    continue
                st = summary[(a, s)]
                for c in PCT_COLS:
                    w.writerow([a, s, PCT_LABEL[c], f"{st[c]['median']:.3f}",
                                f"{st[c]['lo']:.3f}", f"{st[c]['hi']:.3f}",
                                st[c]["n"]])
    return path


def print_summary(summary):
    print("    cross-ISA p99 (us), median across trials:")
    print("    {:<12} {:<12} {:>12}".format("arch", "scheduler", "p99(us)"))
    for a in arches(summary):
        for s in scheds(summary):
            if (a, s) not in summary:
                continue
            med = summary[(a, s)]["p99_us"]["median"]
            cell = f"{med:.2f}" if med == med else "n/a"
            print("    {:<12} {:<12} {:>12}".format(a, s, cell))


def plot(summary, out_dir, meta):
    try:
        import matplotlib
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
    except Exception as e:  # noqa: BLE001
        print(f"    NOTE: matplotlib not importable ({e}); summary CSV written.")
        return None

    NAVY = "#0a1929"; PANEL = "#132c44"; TEXT = "#d6e2f0"; DIM = "#8ba2bc"
    plt.rcParams.update({
        "figure.facecolor": NAVY, "axes.facecolor": NAVY, "savefig.facecolor": NAVY,
        "text.color": TEXT, "axes.labelcolor": TEXT, "xtick.color": DIM,
        "ytick.color": DIM, "axes.edgecolor": "#2a4a6a", "grid.color": "#1d3a57",
        "font.size": 11,
    })
    # color by arch; group by scheduler. p99 is the headline percentile here.
    arch_color = {"x86_64": "#5a8ef0", "aarch64": "#00d68f", "arm64": "#00d68f"}
    fallback = ["#ffb547", "#e94f8a", "#9b8cff"]

    aa = arches(summary)
    ss = scheds(summary)
    if not aa or not ss:
        print("    NOTE: nothing to plot.")
        return None

    fig, ax = plt.subplots(figsize=(8.8, 4.6))
    width = 0.8 / max(len(aa), 1)
    for i, a in enumerate(aa):
        meds, los, his = [], [], []
        for s in ss:
            st = summary.get((a, s))
            if st:
                m = st["p99_us"]["median"]
                meds.append(m)
                los.append(max(0.0, m - st["p99_us"]["lo"]))
                his.append(max(0.0, st["p99_us"]["hi"] - m))
            else:
                meds.append(0.0); los.append(0.0); his.append(0.0)
        xs = [g + (i - (len(aa) - 1) / 2.0) * width for g in range(len(ss))]
        ax.bar(xs, meds, width, label=a,
               color=arch_color.get(a, fallback[i % len(fallback)]),
               yerr=[los, his], capsize=3, ecolor="#cbd9ea")

    ax.set_yscale("log")
    ax.set_ylabel("p99 latency (us, log)")
    ax.set_xticks(range(len(ss)))
    ax.set_xticklabels(ss)
    ax.set_title(
        f"Cross-ISA: p99 by scheduler on each architecture ({meta['workload']})\n"
        "same identity-aware policy, only clang -D__TARGET_ARCH_* differs",
        color=TEXT, fontsize=11.5, pad=12)
    ax.grid(True, which="both", axis="y", alpha=0.33)
    ax.legend(facecolor=PANEL, edgecolor="#2a4a6a", labelcolor=TEXT, loc="upper left")
    fig.tight_layout()
    path = os.path.join(out_dir, "cross_isa_money_plot.png")
    fig.savefig(path, dpi=130)
    print(f"    wrote {path}")
    return path


def main():
    if len(sys.argv) < 3:
        print("usage: cross_isa_plot.py <merged.csv> <out_dir>", file=sys.stderr)
        return 2
    csv_path, out_dir = sys.argv[1], sys.argv[2]
    if not os.path.isfile(csv_path):
        print(f"ERROR: {csv_path} not found", file=sys.stderr)
        return 1
    groups, meta = load(csv_path)
    if not groups:
        print(f"    NOTE: no rows in {csv_path}.")
        return 0
    summary = summarize(groups)
    print(f"    wrote {write_summary(summary, out_dir)}")
    print_summary(summary)
    plot(summary, out_dir, meta)
    return 0


if __name__ == "__main__":
    sys.exit(main())
