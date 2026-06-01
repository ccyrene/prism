#!/usr/bin/env python3
"""Summarise + plot the sched-eval trial CSV produced by run-sched-eval.sh.

Reads the long-format trial CSV:
    scheduler,arch,kernel,workload,trial,p50_us,p99_us,p999_us

and produces:
  1. <out>/sched_eval_summary_<arch>.csv  — per-scheduler median of each
     percentile across trials, plus a percentile-bootstrap 95% CI on the median
     (the SAME methodology as bench/cmd/prismbench/stats.go and the project's
     module-07 stats rules: report the distribution, lead with tails, use a
     heavy-tail-safe bootstrap CI on the median rather than a Gaussian CI).
  2. <out>/sched_money_plot_<arch>.png — the "money plot": grouped bars of
     p50/p99/p99.9 (log y) per scheduler, so scx_prism's tail vs baseline vs
     scx_layered is read at a glance. (Skipped with a note if matplotlib absent.)

Numpy is optional (pure-python percentile + bootstrap fallback). Matplotlib is
optional (summary CSV is still written). Deterministic bootstrap seed so the
report is reproducible — identical to stats.go's fixed-seed approach.

Usage:
    sched_eval_plot.py <trials.csv> <out_dir>
"""
import csv
import math
import os
import random
import sys

BOOTSTRAP_RESAMPLES = 2000
BOOTSTRAP_CONF = 0.95
BOOTSTRAP_SEED = 0x123456789ABCDEF  # match stats.go for cross-tool reproducibility

# Canonical leg order for stable plotting/printing.
SCHED_ORDER = ["baseline", "scx_layered", "scx_prism"]
PCT_COLS = ["p50_us", "p99_us", "p999_us"]
PCT_LABELS = {"p50_us": "p50", "p99_us": "p99", "p999_us": "p99.9"}


def percentile(sorted_vals, p):
    """p-th percentile (0..100) of an already-sorted list, linear interpolation.
    Mirrors stats.go percentile()."""
    n = len(sorted_vals)
    if n == 0:
        return float("nan")
    if n == 1:
        return sorted_vals[0]
    rank = (p / 100.0) * (n - 1)
    lo = math.floor(rank)
    hi = math.ceil(rank)
    if lo == hi:
        return sorted_vals[int(lo)]
    frac = rank - lo
    return sorted_vals[int(lo)] * (1 - frac) + sorted_vals[int(hi)] * frac


def bootstrap_median_ci(values, resamples=BOOTSTRAP_RESAMPLES, conf=BOOTSTRAP_CONF):
    """Percentile-bootstrap CI on the median (heavy-tail safe). Returns (lo, hi).
    Deterministic seed for reproducibility — same construction as stats.go."""
    n = len(values)
    if n < 3:
        s = sorted(values)
        return (s[0], s[-1]) if s else (float("nan"), float("nan"))
    rng = random.Random(BOOTSTRAP_SEED)
    meds = []
    for _ in range(resamples):
        sample = [values[rng.randrange(n)] for _ in range(n)]
        sample.sort()
        meds.append(percentile(sample, 50))
    meds.sort()
    alpha = (1 - conf) / 2
    return percentile(meds, alpha * 100), percentile(meds, (1 - alpha) * 100)


def load(csv_path):
    """Return rows grouped: {scheduler: {pct_col: [values...]}} plus meta."""
    groups = {}
    meta = {"arch": "?", "kernel": "?", "workload": "?"}
    with open(csv_path) as f:
        for row in csv.DictReader(f):
            sched = row["scheduler"]
            meta["arch"] = row.get("arch", meta["arch"])
            meta["kernel"] = row.get("kernel", meta["kernel"])
            meta["workload"] = row.get("workload", meta["workload"])
            g = groups.setdefault(sched, {c: [] for c in PCT_COLS})
            for c in PCT_COLS:
                try:
                    v = float(row[c])
                except (KeyError, ValueError, TypeError):
                    continue
                # drop zeros that mean "tool produced no sample" so they don't
                # poison the medians; a genuinely-zero latency is impossible.
                if v > 0:
                    g[c].append(v)
    return groups, meta


def summarize(groups):
    """{scheduler: {pct_col: {median, ci_low, ci_high, n}}}."""
    out = {}
    for sched, cols in groups.items():
        s = {}
        for c, vals in cols.items():
            if not vals:
                s[c] = {"median": float("nan"), "ci_low": float("nan"),
                        "ci_high": float("nan"), "n": 0}
                continue
            sv = sorted(vals)
            med = percentile(sv, 50)
            lo, hi = bootstrap_median_ci(sv)
            s[c] = {"median": med, "ci_low": lo, "ci_high": hi, "n": len(vals)}
        out[sched] = s
    return out


def ordered_scheds(summary):
    present = [s for s in SCHED_ORDER if s in summary]
    extra = [s for s in summary if s not in SCHED_ORDER]
    return present + sorted(extra)


def write_summary_csv(summary, out_dir, arch):
    path = os.path.join(out_dir, f"sched_eval_summary_{arch}.csv")
    with open(path, "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["scheduler", "percentile", "median_us", "ci95_low_us",
                    "ci95_high_us", "n_trials"])
        for sched in ordered_scheds(summary):
            for c in PCT_COLS:
                st = summary[sched][c]
                w.writerow([sched, PCT_LABELS[c],
                            f"{st['median']:.3f}", f"{st['ci_low']:.3f}",
                            f"{st['ci_high']:.3f}", st["n"]])
    return path


def print_summary(summary, meta):
    print(f"    sched-eval summary  (arch={meta['arch']} kernel={meta['kernel']} "
          f"workload={meta['workload']})")
    print("    {:<13} {:>10} {:>10} {:>10}".format(
        "scheduler", "p50(us)", "p99(us)", "p99.9(us)"))
    base = summary.get("baseline")
    for sched in ordered_scheds(summary):
        s = summary[sched]
        cells = []
        for c in PCT_COLS:
            med = s[c]["median"]
            cell = f"{med:.2f}" if med == med else "n/a"  # NaN check
            cells.append(cell)
        line = "    {:<13} {:>10} {:>10} {:>10}".format(sched, *cells)
        # relative-to-baseline tail callout (the headline comparison).
        if base and sched != "baseline":
            bp99 = base["p99_us"]["median"]
            sp99 = s["p99_us"]["median"]
            if bp99 == bp99 and sp99 == sp99 and bp99 > 0:
                line += f"   p99 vs baseline: {sp99 / bp99:.2f}x"
        print(line)


def money_plot(summary, out_dir, arch, meta):
    try:
        import matplotlib
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
    except Exception as e:  # noqa: BLE001
        print(f"    NOTE: matplotlib not importable ({e}); skipping plot, summary CSV written.")
        return None

    # Dark theme matching bench/plot.py for a consistent paper look.
    NAVY = "#0a1929"; PANEL = "#132c44"; TEXT = "#d6e2f0"; DIM = "#8ba2bc"
    GREEN = "#00d68f"; AMBER = "#ffb547"; BLUE = "#5a8ef0"
    plt.rcParams.update({
        "figure.facecolor": NAVY, "axes.facecolor": NAVY, "savefig.facecolor": NAVY,
        "text.color": TEXT, "axes.labelcolor": TEXT, "xtick.color": DIM,
        "ytick.color": DIM, "axes.edgecolor": "#2a4a6a", "grid.color": "#1d3a57",
        "font.size": 11,
    })
    color = {"baseline": AMBER, "scx_layered": BLUE, "scx_prism": GREEN}

    scheds = ordered_scheds(summary)
    if not scheds:
        print("    NOTE: no scheduler legs present; skipping plot.")
        return None

    fig, ax = plt.subplots(figsize=(8.6, 4.6))
    ngroups = len(PCT_COLS)
    nscheds = len(scheds)
    width = 0.8 / max(nscheds, 1)
    for i, sched in enumerate(scheds):
        meds = [summary[sched][c]["median"] for c in PCT_COLS]
        # asymmetric error bars from the bootstrap CI
        err_lo = [max(0.0, summary[sched][c]["median"] - summary[sched][c]["ci_low"])
                  for c in PCT_COLS]
        err_hi = [max(0.0, summary[sched][c]["ci_high"] - summary[sched][c]["median"])
                  for c in PCT_COLS]
        xs = [g + (i - (nscheds - 1) / 2.0) * width for g in range(ngroups)]
        ax.bar(xs, meds, width, color=color.get(sched, "#888"), label=sched,
               yerr=[err_lo, err_hi], capsize=3, ecolor="#cbd9ea")

    ax.set_yscale("log")
    ax.set_ylabel("latency (us, log scale)")
    ax.set_xticks(range(ngroups))
    ax.set_xticklabels([PCT_LABELS[c] for c in PCT_COLS])
    ax.set_title(
        f"Tail latency by scheduler — {meta['workload']} on {meta['arch']} "
        f"(kernel {meta['kernel']})\nmedian of {_max_n(summary)} trials, "
        f"error bars = bootstrap 95% CI on the median",
        color=TEXT, fontsize=11.5, pad=12)
    ax.grid(True, which="both", axis="y", alpha=0.33)
    ax.legend(facecolor=PANEL, edgecolor="#2a4a6a", labelcolor=TEXT, loc="upper left")
    fig.tight_layout()
    path = os.path.join(out_dir, f"sched_money_plot_{arch}.png")
    fig.savefig(path, dpi=130)
    print(f"    wrote {path}")
    return path


def _max_n(summary):
    n = 0
    for s in summary.values():
        for c in PCT_COLS:
            n = max(n, s[c]["n"])
    return n


def main():
    if len(sys.argv) < 3:
        print("usage: sched_eval_plot.py <trials.csv> <out_dir>", file=sys.stderr)
        return 2
    csv_path, out_dir = sys.argv[1], sys.argv[2]
    if not os.path.isfile(csv_path):
        print(f"ERROR: {csv_path} not found", file=sys.stderr)
        return 1
    groups, meta = load(csv_path)
    if not groups:
        print(f"    NOTE: no trial rows in {csv_path}; nothing to summarise.")
        return 0
    summary = summarize(groups)
    arch = meta["arch"]
    out_csv = write_summary_csv(summary, out_dir, arch)
    print(f"    wrote {out_csv}")
    print_summary(summary, meta)
    money_plot(summary, out_dir, arch, meta)
    return 0


if __name__ == "__main__":
    sys.exit(main())
