#!/usr/bin/env python3
"""selective_summary.py — summarize the Case 4+5 selective/deterministic eval.

Reads selective_eval_<arch>.csv (scheduler,probe,trial,p50_us,p99_us,p999_us) and
prints, per (scheduler, probe), the median across trials of each percentile plus a
percentile-bootstrap 95% CI on that median. Same bootstrap construction/seed as
sched_eval_plot.py so the numbers are cross-tool reproducible.

Usage: selective_summary.py <selective_eval.csv> [out_summary.csv]
"""
import csv
import math
import random
import sys

BOOTSTRAP_RESAMPLES = 2000
BOOTSTRAP_CONF = 0.95
BOOTSTRAP_SEED = 0x123456789ABCDEF
PCT_COLS = ["p50_us", "p99_us", "p999_us"]
PROBE_ORDER = ["A_critical", "B_normal", "C_batch"]
SCHED_ORDER = ["baseline", "bpfland", "bpfland_prism"]


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


def bootstrap_median_ci(values):
    n = len(values)
    if n < 3:
        s = sorted(values)
        return (s[0], s[-1]) if s else (float("nan"), float("nan"))
    rng = random.Random(BOOTSTRAP_SEED)
    meds = []
    for _ in range(BOOTSTRAP_RESAMPLES):
        sample = sorted(values[rng.randrange(n)] for _ in range(n))
        meds.append(percentile(sample, 50))
    meds.sort()
    alpha = (1 - BOOTSTRAP_CONF) / 2
    return percentile(meds, alpha * 100), percentile(meds, (1 - alpha) * 100)


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(2)
    path = sys.argv[1]
    groups = {}  # (sched, probe) -> {pct_col: [values]}
    with open(path) as f:
        for row in csv.DictReader(f):
            key = (row["scheduler"], row["probe"])
            g = groups.setdefault(key, {c: [] for c in PCT_COLS})
            for c in PCT_COLS:
                try:
                    v = float(row[c])
                except (KeyError, ValueError, TypeError):
                    continue
                if v > 0:
                    g[c].append(v)

    out_rows = [["scheduler", "probe", "percentile", "median_us",
                 "ci95_low_us", "ci95_high_us", "n"]]
    scheds = [s for s in SCHED_ORDER if any(k[0] == s for k in groups)]
    print(f"{'scheduler':<15}{'probe':<13}{'p50':>8}{'p99':>9}"
          f"{'  p99 95% CI':<20}{'p99.9':>9}")
    for s in scheds:
        for pr in PROBE_ORDER:
            g = groups.get((s, pr))
            if not g:
                continue
            meds = {}
            for c in PCT_COLS:
                sv = sorted(g[c])
                med = percentile(sv, 50)
                lo, hi = bootstrap_median_ci(sv)
                meds[c] = (med, lo, hi, len(sv))
                out_rows.append([s, pr, c, f"{med:.0f}", f"{lo:.0f}",
                                 f"{hi:.0f}", len(sv)])
            p50, p99, p999 = meds["p50_us"], meds["p99_us"], meds["p999_us"]
            ci = f"[{p99[1]:.0f}, {p99[2]:.0f}]"
            print(f"{s:<15}{pr:<13}{p50[0]:>8.0f}{p99[0]:>9.0f}"
                  f"  {ci:<18}{p999[0]:>9.0f}")
        print()

    if len(sys.argv) > 2:
        with open(sys.argv[2], "w", newline="") as f:
            csv.writer(f).writerows(out_rows)
        print(f"wrote {sys.argv[2]}")


if __name__ == "__main__":
    main()
