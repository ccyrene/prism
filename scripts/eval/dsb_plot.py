#!/usr/bin/env python3
"""Plot the DeathStarBench rate-vs-tail sweep from run-deathstarbench.sh.

Reads:  scheduler,arch,kernel,rate_rps,p50_ms,p99_ms,p999_ms
Writes: <out>/dsb_money_plot_<arch>.png  — offered load (req/s) on x, tail
        latency (ms, log y) on y, one curve per (scheduler, percentile). The
        "knee" where a curve hockey-sticks up is the load at which that
        scheduler's tail collapses; a higher knee under scx_prism is the win.

Because wrk2 (-R, constant-throughput) is coordinated-omission correct, these
tails are honest — the whole point of using wrk2 over wrk (see the script).

Matplotlib optional: if absent we just print the parsed table so the run still
yields a readable result. Usage: dsb_plot.py <csv> <out_dir>
"""
import csv
import os
import sys

SCHED_ORDER = ["baseline", "scx_layered", "scx_prism"]


def load(csv_path):
    rows = []
    meta = {"arch": "?", "kernel": "?"}
    with open(csv_path) as f:
        for r in csv.DictReader(f):
            meta["arch"] = r.get("arch", meta["arch"])
            meta["kernel"] = r.get("kernel", meta["kernel"])
            try:
                rows.append({
                    "scheduler": r["scheduler"],
                    "rate": float(r["rate_rps"]),
                    "p50": float(r["p50_ms"]),
                    "p99": float(r["p99_ms"]),
                    "p999": float(r["p999_ms"]),
                })
            except (KeyError, ValueError, TypeError):
                continue
    return rows, meta


def by_sched(rows):
    out = {}
    for r in rows:
        out.setdefault(r["scheduler"], []).append(r)
    for s in out:
        out[s].sort(key=lambda x: x["rate"])
    return out


def ordered(groups):
    present = [s for s in SCHED_ORDER if s in groups]
    return present + sorted(s for s in groups if s not in SCHED_ORDER)


def print_table(groups):
    print("    DeathStarBench socialNetwork — tail latency (ms) vs offered rate (req/s)")
    for s in ordered(groups):
        print(f"    [{s}]")
        print("      {:>9} {:>9} {:>9} {:>9}".format("rate", "p50", "p99", "p99.9"))
        for r in groups[s]:
            print("      {:>9.0f} {:>9.3f} {:>9.3f} {:>9.3f}".format(
                r["rate"], r["p50"], r["p99"], r["p999"]))


def plot(groups, out_dir, meta):
    try:
        import matplotlib
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
    except Exception as e:  # noqa: BLE001
        print(f"    NOTE: matplotlib not importable ({e}); printed table instead.")
        return None

    NAVY = "#0a1929"; PANEL = "#132c44"; TEXT = "#d6e2f0"; DIM = "#8ba2bc"
    GREEN = "#00d68f"; AMBER = "#ffb547"; BLUE = "#5a8ef0"
    plt.rcParams.update({
        "figure.facecolor": NAVY, "axes.facecolor": NAVY, "savefig.facecolor": NAVY,
        "text.color": TEXT, "axes.labelcolor": TEXT, "xtick.color": DIM,
        "ytick.color": DIM, "axes.edgecolor": "#2a4a6a", "grid.color": "#1d3a57",
        "font.size": 11,
    })
    color = {"baseline": AMBER, "scx_layered": BLUE, "scx_prism": GREEN}
    # p99 solid (headline), p99.9 dashed (deeper tail), p50 dotted (reference).
    style = {"p99": ("-", 2.6), "p999": ("--", 1.8), "p50": (":", 1.2)}
    label = {"p99": "p99", "p999": "p99.9", "p50": "p50"}

    fig, ax = plt.subplots(figsize=(8.8, 4.8))
    for s in ordered(groups):
        rows = groups[s]
        rates = [r["rate"] for r in rows]
        for pk in ("p99", "p999", "p50"):
            ls, lw = style[pk]
            ys = [r[pk] for r in rows]
            ax.plot(rates, ys, ls, color=color.get(s, "#888"), lw=lw,
                    marker="o" if pk == "p99" else None, markersize=4,
                    label=f"{s} {label[pk]}")
    ax.set_xscale("log"); ax.set_yscale("log")
    ax.set_xlabel("offered load (req/s, log) — wrk2 constant-throughput, CO-corrected")
    ax.set_ylabel("latency (ms, log)")
    ax.set_title(
        f"DeathStarBench socialNetwork tail latency vs load\n"
        f"{meta['arch']} kernel {meta['kernel']} — higher knee = scheduler sustains more load",
        color=TEXT, fontsize=11.5, pad=12)
    ax.grid(True, which="both", alpha=0.33)
    ax.legend(facecolor=PANEL, edgecolor="#2a4a6a", labelcolor=TEXT,
              loc="upper left", fontsize=8.5, ncol=len(ordered(groups)) or 1)
    fig.tight_layout()
    path = os.path.join(out_dir, f"dsb_money_plot_{meta['arch']}.png")
    fig.savefig(path, dpi=130)
    print(f"    wrote {path}")
    return path


def main():
    if len(sys.argv) < 3:
        print("usage: dsb_plot.py <csv> <out_dir>", file=sys.stderr)
        return 2
    csv_path, out_dir = sys.argv[1], sys.argv[2]
    if not os.path.isfile(csv_path):
        print(f"ERROR: {csv_path} not found", file=sys.stderr)
        return 1
    rows, meta = load(csv_path)
    if not rows:
        print(f"    NOTE: no data rows in {csv_path}.")
        return 0
    groups = by_sched(rows)
    print_table(groups)
    plot(groups, out_dir, meta)
    return 0


if __name__ == "__main__":
    sys.exit(main())
