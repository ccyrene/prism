#!/usr/bin/env python3
"""Plot the load-sweep "knee": probe tail latency vs CPU contention, per scheduler.

Input CSV: scheduler,noise,trial,p50_us,p99_us,p999_us
Output: <out>/sched_knee_plot_<arch>.png  +  <out>/sched_sweep_summary.csv
For each (scheduler, noise) we take the median across trials, with a
percentile-bootstrap 95% CI on the median (fixed seed, matches stats.go).
"""
import csv, math, os, random, sys

SEED = 0x123456789ABCDEF
def pct(s, p):
    n=len(s)
    if n==0: return float('nan')
    if n==1: return s[0]
    r=(p/100.0)*(n-1); lo=math.floor(r); hi=math.ceil(r)
    return s[int(lo)] if lo==hi else s[int(lo)]*(1-(r-lo))+s[int(hi)]*(r-lo)
def ci(v):
    n=len(v)
    if n<3:
        s=sorted(v); return (s[0],s[-1]) if s else (float('nan'),)*2
    rng=random.Random(SEED); meds=[]
    for _ in range(2000):
        samp=sorted(v[rng.randrange(n)] for _ in range(n)); meds.append(pct(samp,50))
    meds.sort(); return pct(meds,2.5), pct(meds,97.5)

def main():
    csv_path, out = sys.argv[1], sys.argv[2]
    data={}  # (sched, noise) -> {col:[vals]}
    arch = os.environ.get("ARCH","x86_64")
    for row in csv.DictReader(open(csv_path)):
        k=(row["scheduler"], int(row["noise"]))
        d=data.setdefault(k, {"p50_us":[], "p99_us":[], "p999_us":[]})
        for c in d:
            try:
                v=float(row[c])
                if v>0: d[c].append(v)
            except (KeyError,ValueError): pass
    scheds=sorted({s for s,_ in data}); noises=sorted({n for _,n in data})
    # summary csv
    sp=os.path.join(out,"sched_sweep_summary.csv")
    with open(sp,"w",newline="") as f:
        w=csv.writer(f); w.writerow(["scheduler","noise","percentile","median_us","ci_lo","ci_hi","n"])
        for s in scheds:
            for n in noises:
                d=data.get((s,n));
                if not d: continue
                for c,lab in (("p50_us","p50"),("p99_us","p99"),("p999_us","p99.9")):
                    v=sorted(d[c]);
                    if not v: continue
                    m=pct(v,50); lo,hi=ci(v); w.writerow([s,n,lab,f"{m:.1f}",f"{lo:.1f}",f"{hi:.1f}",len(v)])
    print(f"    wrote {sp}")
    try:
        import matplotlib; matplotlib.use("Agg"); import matplotlib.pyplot as plt
    except Exception as e:
        print(f"    matplotlib missing ({e}); summary only"); return
    NAVY="#0a1929"; TEXT="#d6e2f0"; DIM="#8ba2bc"; AMBER="#ffb547"; GREEN="#00d68f"
    plt.rcParams.update({"figure.facecolor":NAVY,"axes.facecolor":NAVY,"savefig.facecolor":NAVY,
        "text.color":TEXT,"axes.labelcolor":TEXT,"xtick.color":DIM,"ytick.color":DIM,
        "axes.edgecolor":"#2a4a6a","grid.color":"#1d3a57","font.size":11})
    col={"baseline":AMBER,"scx_prism":GREEN}
    fig,ax=plt.subplots(figsize=(8.0,4.8))
    for s in scheds:
        for c,style,lab in (("p99_us","-","p99"),("p999_us","--","p99.9")):
            xs=[]; ys=[]; el=[]; eh=[]
            for n in noises:
                d=data.get((s,n))
                if not d or not d[c]: continue
                v=sorted(d[c]); m=pct(v,50); lo,hi=ci(v)
                xs.append(n); ys.append(m); el.append(max(0,m-lo)); eh.append(max(0,hi-m))
            if xs:
                ax.errorbar(xs,ys,yerr=[el,eh],marker="o",linestyle=style,color=col.get(s,"#888"),
                    capsize=3,label=f"{s} {lab}",linewidth=2 if c=="p99_us" else 1.3,
                    markersize=5,alpha=1.0 if c=="p99_us" else 0.75)
    ax.axvline(16,color="#44607f",linestyle=":",linewidth=1); ax.text(16.2,ax.get_ylim()[0] if False else 1, "")
    ax.set_yscale("log"); ax.set_xlabel("CPU contention (number of stress-ng hogs; host has 16 CPUs)")
    ax.set_ylabel("probe wakeup latency (us, log)")
    ax.set_title("Tail latency vs offered CPU load — baseline EEVDF vs scx_prism\n"
                 "(schbench probe; medians of trials, error bars bootstrap 95% CI)",color=TEXT,fontsize=11.5,pad=10)
    ax.grid(True,which="both",alpha=0.3); ax.legend(facecolor="#132c44",edgecolor="#2a4a6a",labelcolor=TEXT,fontsize=9,ncol=2)
    fig.tight_layout(); p=os.path.join(out,f"sched_knee_plot_{arch}.png"); fig.savefig(p,dpi=130)
    print(f"    wrote {p}")

if __name__=="__main__": sys.exit(main())
