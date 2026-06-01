#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# three-leg-demo.sh — prove the ONE pinned `prism_identity` bus is read by all
# THREE BPF subsystems AT ONCE on a real kernel:
#   * sched : scx_bpfland+Prism      (struct_ops, the fixed-core scheduler)
#   * net   : prism_net_egress       (cgroup_skb/egress)
#   * trace : execsnoop_prism_*      (execve tracepoints)
# The scheduler creates+pins the bus; net and trace REUSE that exact pinned map
# (LIBBPF_PIN_BY_NAME, RDONLY_PROG) — no second copy. We seed one identity, run a
# workload in its cgroup that does CPU + egress + exec, then show all three
# programs are loaded, all reference the SAME map id, and the net leg attributed
# the workload's packets to the seeded identity.
#
# Run inside the privileged prism-eval container (--cgroupns=host, 6.17):
#   docker exec --privileged prism-eval bash /work/prism/scripts/three-leg-demo.sh
set -uo pipefail
SCX="${SCX:-/opt/scx/target/release}"
PRISM_SCHED="${PRISM_SCHED:-${SCX}/scx_bpfland_prism}"
NET_O="${NET_O:-/tmp/net_policy.bpf.o}"
TRACE_O="${TRACE_O:-/tmp/execsnoop.bpf.o}"
PIN=/sys/fs/bpf
CG=/sys/fs/cgroup/prism_probe
FAST_ID="${FAST_ID:-256}"
SP=""
u64h(){ printf "%016x" "$1"|sed 's/../& /g'|awk '{for(i=NF;i>=1;i--)printf "%s ",$i}'; }
u32h(){ printf "%08x"  "$1"|sed 's/../& /g'|awk '{for(i=NF;i>=1;i--)printf "%s ",$i}'; }
cleanup(){
  bpftool cgroup detach "$CG" egress pinned "$PIN/net/prism_net_egress" 2>/dev/null || true
  rm -rf "$PIN/net" "$PIN/trace" 2>/dev/null || true
  [[ -n "$SP" ]] && { kill -INT "$SP" 2>/dev/null || true; wait "$SP" 2>/dev/null || true; }
  if [[ -d "$CG" ]]; then
    [[ -f "$CG/cgroup.procs" ]] && while read -r p; do echo "$p" > /sys/fs/cgroup/cgroup.procs 2>/dev/null || true; done < "$CG/cgroup.procs"
    rmdir "$CG" 2>/dev/null || true
  fi
  rm -f "$PIN/prism_identity" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

mount | grep -qw 'type bpf' || mount -t bpf bpf /sys/fs/bpf
# tracefs is needed for the trace leg: libbpf resolves the tracepoint id from
# /sys/kernel/tracing/events/.../id (or the debugfs path) at autoattach time.
mount | grep -qw 'type tracefs' || mount -t tracefs nodev /sys/kernel/tracing 2>/dev/null || \
  mount -t debugfs nodev /sys/kernel/debug 2>/dev/null || true
echo 1 > /proc/sys/kernel/bpf_stats_enabled 2>/dev/null || true   # so prog show reports run_cnt
rm -f "$PIN/prism_identity" 2>/dev/null || true

echo "================= LEG 1: sched (creates + pins the bus) ================="
"$PRISM_SCHED" > /tmp/3leg.sched.log 2>&1 & SP=$!
sleep 4
[[ "$(cat /sys/kernel/sched_ext/state 2>/dev/null)" == "enabled" ]] \
  && echo "[sched] scx_bpfland+Prism: state=enabled nr_rejected=$(cat /sys/kernel/sched_ext/nr_rejected)" \
  || { echo "[sched] FAILED to enable"; tail -5 /tmp/3leg.sched.log; exit 1; }

echo "================= bus: seed one identity ================================"
mkdir -p "$CG"; K="$(stat -c %i "$CG")"
FLAGS=$(( 2 | (1<<8) ))   # SCHED_MANAGED(0x2) | class=critical(1<<8)
bpftool map update name prism_identity key hex $(u64h "$K") \
  value hex $(u32h "$FAST_ID") $(u32h "$FLAGS") 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 \
  && echo "[bus] seeded prism_identity[$K] = {identity=$FAST_ID, class=critical}" || echo "[bus] seed FAILED"

echo "================= LEG 2: net (cgroup_skb/egress, reuses the bus) ========"
bpftool prog loadall "$NET_O" "$PIN/net" map name prism_identity pinned "$PIN/prism_identity" \
  && echo "[net] loaded, prism_identity reused from the pin" || { echo "[net] load FAILED"; exit 1; }
bpftool cgroup attach "$CG" egress pinned "$PIN/net/prism_net_egress" \
  && echo "[net] cgroup_skb/egress attached to the probe cgroup" || echo "[net] attach FAILED"

echo "================= LEG 3: trace (execve tracepoints, reuses the bus) ====="
bpftool prog loadall "$TRACE_O" "$PIN/trace" map name prism_identity pinned "$PIN/prism_identity" autoattach \
  && echo "[trace] loaded + tracepoints auto-attached, prism_identity reused" || { echo "[trace] load FAILED"; exit 1; }

echo "================= workload in the probe cgroup: CPU + egress + exec ====="
(
  echo $BASHPID > "$CG/cgroup.procs"
  for i in $(seq 1 30); do /bin/true; /bin/date >/dev/null; done          # execs   -> trace
  ping -c 10 -i 0.1 127.0.0.1 >/dev/null 2>&1 || true                     # ICMP egress -> net
  for i in $(seq 1 100); do echo hi > /dev/udp/127.0.0.1/9999 2>/dev/null || true; done  # UDP egress -> net
  timeout 2 bash -c 'x=0; while :; do x=$((x+1)); done' || true           # CPU      -> sched
)
echo "[workload] done"

echo "================= VERIFY: one bus, three readers, all live ============="
MAPID=$(bpftool -j map show name prism_identity | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
echo "shared prism_identity map id = $MAPID"
echo "--- the three legs, all referencing the SAME bus map id=$MAPID ---"
bpftool -j prog show > /tmp/3leg.progs.json 2>/dev/null
python3 - "$MAPID" /tmp/3leg.progs.json <<'PY'
import sys,json
mapid=int(sys.argv[1]); progs=json.load(open(sys.argv[2]))
def reads(p): return mapid in p.get('map_ids',[])
# sched leg = the struct_ops program(s) that read the bus (task_dl is inlined into enqueue/select_cpu)
sched=[p for p in progs if p.get('type')=='struct_ops' and reads(p)]
net  =[p for p in progs if p.get('name')=='prism_net_egress']
trace=[p for p in progs if p.get('name','').startswith('execsnoop_prism')]
def line(tag,p):
    print(f"  {tag:18} {p.get('name',''):24} type={p.get('type',''):11} "
          f"run_cnt={p.get('run_cnt','?'):>8} reads_bus={'YES' if reads(p) else 'no'}")
for p in sched: line('sched (struct_ops)',p)
for p in net:   line('net (cgroup_skb)',p)
for p in trace: line('trace (tracepoint)',p)
allreaders=sched+net+trace
n=sum(1 for p in allreaders if reads(p))
print(f"  => {n} live programs across {len({'sched' if p in sched else 'net' if p in net else 'trace' for p in allreaders})} subsystems read the one bus map id={mapid}")
PY
echo "--- net leg attributed the workload's packets to the seeded identity ---"
bpftool map dump name prism_net_stats 2>/dev/null | python3 -c '
import sys,json
try: d=json.load(sys.stdin)
except Exception: d=[]
for e in d:
    f=e.get("formatted",e); k=f.get("key"); v=f.get("value",{})
    print(f"  identity={k}  packets={v.get(\"packets\")}  bytes={v.get(\"bytes\")}")
' 2>/dev/null || bpftool map dump name prism_net_stats
echo "================= DONE (cleanup on exit) ==============================="
