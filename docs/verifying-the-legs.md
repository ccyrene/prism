# Verifying the legs — how to know a consumer actually works

A Prism consumer can *look* healthy while enforcing nothing: if it reads an empty
or private copy of the tag map, every task resolves to identity `0` and it silently
allows / no-ops. So a real check must be able to **fail**. Every leg is verified the
same way:

1. **Loaded + attached?** — the program is live in the kernel, not just on disk.
2. **Reading the *right* bus?** — its `map_ids` include the *shared* `prism_identity`
   map id (not a fresh empty copy).
3. **Decision correct per workload?** — its own counters + the observable effect.

…plus the load-bearing one:

4. **Attribution control** — flip *only* the identity and show the effect appears
   **iff** the tag is present. This is what rules out the empty-bus false-pass.

> Prereqs throughout: root, `bpftool`, bpffs mounted (`mount -t bpf bpf /sys/fs/bpf`),
> and `echo 1 > /proc/sys/kernel/bpf_stats_enabled` so `run_cnt` is reported. The
> shared bus must already be pinned at `/sys/fs/bpf/prism_identity` (by `prismd` or a
> Prism scheduler).

---

## sched — identity-aware scheduling

`correct` = a seeded latency-critical workload's tail latency drops **only when its
tag is on the bus**.

```sh
# (1) loaded+attached
sudo OUT=$PWD/scripts/eval/results bash scripts/eval/run-sched-eval-contended.sh
cat /sys/kernel/sched_ext/state      # -> enabled
cat /sys/kernel/sched_ext/root/ops   # -> prism   (or bpfland for the retrofit)

# (2) reading the right bus + (2b) the seed actually landed (verify the BYTES, the
#     harness only checks the exit code):
PROBE=$(stat -c %i /sys/fs/cgroup/prism_probe)
sudo bpftool map lookup name prism_identity \
     key hex $(printf '%016x' $PROBE | sed 's/../& /g' | awk '{for(i=NF;i>=1;i--)printf "%s ",$i}')
# -> value's first u32 (LE) == 256 (FAST_ID), flags u32 == 0x2 (or 0x102 for bpfland class=critical)
```

**(4) Attribution control — the key one.** The contended harness runs three legs on
the *same* box, *same* contention, *same* probe:

| leg | what it is | expect |
|---|---|---|
| `baseline` | default EEVDF, no scx | high p99 |
| `scx_prism_nobus` | scx_prism attached, **bus empty** | ≈ baseline |
| `scx_prism` | same scheduler, **tag seeded** | **p99 drops** |

Compare the rows in `scripts/eval/results/sched_eval_$(uname -m).csv`; the gap must
clear the bootstrap 95% CI in the `_summary` file. `scx_prism` beating
`scx_prism_nobus` proves the win is **identity routing**, not merely "running scx".

Extra controls (bpfland retrofit): `diff integrations/bpfland/main.bpf.c.orig
integrations/bpfland/main.bpf.c` is the bus-read block + one wrapped `return` (all
knobs untouched), so a win can't be hand-tuning; `GAMER=1 …/run-bpfland-eval.sh`
shows a batch job that *sleeps to look interactive* still gets deprioritized —
identity beats the heuristic, so it can't be gamed.

**Gotchas:** an idle box shows no gap (no contention → scx == baseline) — that's why
the harness manufactures load. A skipped leg (WARN, empty CSV) is **not** a pass. A
silently-missed seed looks like an honest negative — always verify the bytes (2b).

---

## net — per-packet attribution

`correct` = a seeded workload's packets land under its tag; an unseeded one lands
under `0`.

```sh
# build the real object (build.sh does NOT build this one) and run the demo:
clang -O2 -g -target bpf -D__TARGET_ARCH_x86 -I bpf -I bpf/include \
      -c bpf/consumers/net_policy_prism.bpf.c -o /tmp/net_policy.bpf.o
sudo bash scripts/three-leg-demo.sh

# (1) loaded+attached+running
sudo bpftool prog show name prism_net_egress        # run_cnt > 0
sudo bpftool cgroup show /sys/fs/cgroup/prism_probe  # lists prism_net_egress (egress)

# (1b) RIGHT consumer (not the compose_demo look-alike!) — this map exists ONLY in
#      net_policy_prism.bpf.c, so its presence disambiguates:
sudo bpftool map show name prism_net_stats           # must be a HASH map

# (3) attribution correct
sudo bpftool map dump name prism_net_stats -j | \
  python3 -c 'import sys,json;[print("identity="+str(e.get("formatted",e).get("key"))+" "+str(e.get("formatted",e).get("value"))) for e in json.load(sys.stdin)]'
# -> identity=256 with packets>0,bytes>0 (256 = FAST_ID). identity=0 rows = unmanaged host egress, expected.
```

**(4) Attribution control.** Generate identical traffic from `prism_probe` **without**
seeding `prism_identity` → packets bucket under `identity=0`. Then seed it (one
`bpftool map update name prism_identity …`) and re-run → the *same* packets now
bucket under `identity=256`. The only change is one map write, so the bucket move
`0 → 256` is caused by the tag.

**Userspace surface (Prometheus):**
```sh
go build -o prism-net-exporter ./cmd/prism-net-exporter
sudo ./prism-net-exporter -bpftool &      # robust path; -bpftool is a bare flag
curl -s localhost:9465/metrics | grep -E 'prism_net_(packets|bytes)_total|prism_net_up'
```

**Gotchas:** two programs are named `prism_net_egress` (the demo's `compose_demo`
also defines one with **no** counters) — confirm the loaded one binds the
`prism_net_stats` map. Same *name* ≠ same *map*: check map-id equality. The demo's
`trap cleanup` removes the pins on exit — dump/scrape **before** it exits.

---

## sec — LSM enforcement (the 4th leg)

`correct` = a denied workload actually gets `EPERM` on `execve()`; a non-denied /
off-bus / reserved task runs fine; counters key off the right tag.

```sh
# enforcement REQUIRES bpf-LSM enabled:
cat /sys/kernel/security/lsm     # must CONTAIN `bpf` (comma-separated list)
# if absent: reboot with kernel cmdline lsm=...,bpf  (the prog loads+verifies but
# can't attach/enforce without it)

clang -O2 -g -target bpf -D__TARGET_ARCH_x86 -I bpf -I bpf/include \
      -c bpf/consumers/lsm_policy_prism.bpf.c -o lsm_policy_prism.bpf.o
sudo bpftool prog loadall lsm_policy_prism.bpf.o /sys/fs/bpf/lsm_policy_prism \
     map name prism_identity pinned /sys/fs/bpf/prism_identity autoattach

# (1) loaded+attached: a prog of type lsm AND a link (the link = it will fire)
sudo bpftool prog show name prism_lsm_bprm
sudo bpftool link show                # an lsm link whose prog_id matches
```

**(3)+(4) Decision + attribution control** — flip *only* deny-set membership on a
fixed seeded identity:

```sh
# a) NOT denied -> exec allowed
sudo bash -c 'echo $BASHPID > /sys/fs/cgroup/prism_probe/cgroup.procs; /bin/true; echo rc=$?'  # rc=0
# b) deny identity 256 (LE key), re-run the SAME exec -> EPERM
sudo bpftool map update name prism_lsm_denyset key hex 00 01 00 00 value hex 01
sudo bash -c 'echo $BASHPID > /sys/fs/cgroup/prism_probe/cgroup.procs; /bin/true; echo rc=$?'  # rc=126, "Operation not permitted"
# c) remove it -> allowed again
sudo bpftool map delete name prism_lsm_denyset key hex 00 01 00 00
# proof: only deny membership changed across (a)(b)(c) -> the EPERM is caused by identity.

sudo bpftool map dump name prism_lsm_decis   # per-id allowed/denied counters (name truncates to 15 chars!)
```

Negative control: run the same `/bin/true` from a cgroup **not** on the bus (id `0`)
→ allowed even with the deny rule on. Equivalent path: seed the tag with
class=`BATCH` (`flags = 0x300`) instead of using the deny-set → denied via rule 2.

**Gotchas:** the #1 false-pass is bpf-LSM not enabled — a loaded-but-unlinked `lsm`
prog gates nothing (check `link show`, not just `prog show`). Map names truncate to
15 chars (`prism_lsm_decisions` → `prism_lsm_decis`). Reserved ids (`<256`) are never
gated — expected. Forgetting `map name prism_identity pinned …` makes libbpf create a
fresh empty bus → everything allowed → fake "healthy".

---

## The one rule

> A verification that **can't fail is worthless.** Every leg's real test is the
> attribution control: hold everything constant, flip only the identity, and show
> the effect (EPERM / bucket-256 / lower p99) appears **only** when the tag is there.
