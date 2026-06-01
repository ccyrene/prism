# Prism Identity Bus — ABI Specification

**Status:** frozen (v1)
**Audience:** authors of an independent BPF subsystem (sched_ext scheduler, tc/XDP
network-policy datapath, Tetragon-style observer) who want to interoperate with
Prism without coordinating with the Prism daemon at build time.
**Normative sources:** `pkg/abi/abi.go` (Go side), `bpf/prism_maps.bpf.h` (C side),
`pkg/identity` (allocation/canonicalization semantics). Where this document and
those files disagree, **the source files win** — this spec describes them, it does
not override them.

The key words MUST, MUST NOT, SHOULD, SHOULD NOT, MAY are used as in RFC 2119.

---

## 1. Overview — identity as substrate

Prism's thesis is that *workload identity* is the right shared substrate for an
entire class of Kubernetes-aware kernel subsystems. Network policy, scheduling, and
observability all independently re-derive "which workload is this?" from pods,
cgroups, IPs, and labels. Prism computes that answer **once**, in userspace, and
publishes it into **one shared, pinned BPF map** that any subsystem can read.

That single map *is* the bus. There is no RPC, no socket, no message broker — the
coupling between producer and consumers is the byte layout defined here plus the
pin path. A subsystem joins the bus simply by opening the pinned map and reading
values keyed by the workload key.

### 1.1 Three-way composability via flag facets

A workload has exactly one numeric identity, but multiple subsystems may act on it
simultaneously. Each subsystem owns one **facet bit** in the `flags` field:

- **net** — `PRISM_FLAG_NET_POLICY`: a network-policy engine has a rule for this id.
- **sched** — `PRISM_FLAG_SCHED_MANAGED`: a sched_ext scheduler is managing this id.
- **observe** — `PRISM_FLAG_OBSERVED`: an observer is tracking this id.

Composability is the contract that a consumer **sets and clears only its own bit**
and **only reads** the others'. Because all three operate on the same identity and
the same map entry, a scheduler can cheaply ask "is this workload also under a
network policy?" by reading a flag, with zero new plumbing. This is the concrete
form of the "3-way composability" claim: net ∘ sched ∘ observe over one identity.

```
            pods + labels (K8s API)
                     │
                     ▼
            ┌──────────────────┐
            │  prismd (Go)     │  sole writer of identity + label_hash
            │  identity alloc  │
            └──────────────────┘
                     │ Upsert/Delete
                     ▼
   ╔═════════════════════════════════════════╗
   ║  pinned map  prism_identity  (THE BUS)   ║
   ║  key __u64 → struct prism_identity (24B) ║
   ╚═════════════════════════════════════════╝
        ▲              ▲               ▲
        │ read id      │ read id       │ read id
        │ RMW NET bit  │ RMW SCHED bit │ RMW OBSERVE bit
   ┌─────────┐    ┌──────────┐    ┌──────────┐
   │ net/tc  │    │ sched_ext│    │ observer │
   └─────────┘    └──────────┘    └──────────┘
```

---

## 2. The bus map

| Property        | Value                                                              |
|-----------------|--------------------------------------------------------------------|
| Name            | `prism_identity`                                                   |
| Pin path        | `/sys/fs/bpf/prism/prism_identity`                                 |
| Map type        | `BPF_MAP_TYPE_HASH`                                                |
| Key             | `__u64` workload key (see §2.1)                                    |
| Value           | `struct prism_identity` (24 bytes, §3)                            |
| `max_entries`   | `1 << 20` = 1,048,576                                              |
| `map_flags`     | `BPF_F_RDONLY_PROG` (`0x80`) — read-only to all BPF programs (§2.2) |
| Pinning mode    | `LIBBPF_PIN_BY_NAME` (pinned under the bpffs subdir, name = map name) |

Both `MapName` and the basename of `PinPath` are the literal string
`prism_identity`. Consumers MUST locate the map by opening the pin path; they MUST
NOT create the map themselves (doing so races the daemon and may create a second,
disconnected map). If the pin does not exist, the daemon is not running and the bus
is empty — consumers SHOULD treat a missing pin as "no identities yet", not an error.

The map is created and pinned by `prismd` on a real cgroup-v2 kernel. Consumers
attach to the existing pin (`bpf_obj_get` / libbpf `bpf_map__reuse_fd` against the
pin path, or cilium/ebpf `LoadPinnedMap`). The C definition that produces a
bus-compatible map is in `bpf/prism_maps.bpf.h`; an independent BPF object SHOULD
`#include` that header verbatim rather than re-declaring the map, so the type, key,
value and pinning attributes are guaranteed identical (libbpf de-duplicates
identically-named, identically-typed pinned maps across objects).

### 2.1 Workload key (`__u64`)

The key identifies a *workload instance* (a running pod / its cgroup), **not** the
identity. Many keys may map to the same identity (every replica of a Deployment
shares one identity but has a distinct cgroup).

- **Real kernel (target, 6.12+):** the key is the **cgroup v2 id**, i.e. the value
  returned by `bpf_get_current_cgroup_id()` from a BPF program, or the kernfs id of
  the pod's cgroup directory as read by the daemon. A consumer running in a hook
  with a current task (e.g. sched_ext, LSM, tracepoint) obtains the key directly via
  `bpf_get_current_cgroup_id()` and looks it up.
- **Simulation / benchmark (this host, 5.15 WSL2):** the key is a **synthetic stable
  `u64`** assigned per pod by the daemon (e.g. a hash/counter), used identically.
  The ABI width and meaning ("opaque stable per-workload-instance handle") are fixed
  regardless of source; consumers MUST NOT assume the key encodes anything.

  On a real kernel the per-workload key is the **pod-ancestor** cgroup id: an in-task
  consumer obtains it via the pod-ancestor cgroup-id helper (a bounded walk to the pod
  level), and the daemon writes the matching value (the `stat(2)` inode of the pod's
  cgroup directory). A consumer in a deeply nested container cgroup therefore still
  keys on the *pod's* id, matching what `prismd` wrote.

### 2.2 Map integrity — `BPF_F_RDONLY_PROG` (read-only to programs)

The bus map is created by `prismd` with **`BPF_F_RDONLY_PROG`** (`map_flags = 0x80`),
which makes it **read-only to every BPF program**. This is a normative property of
the bus, not a deployment option:

- Any consumer BPF program that attempts `bpf_map_update_elem` /
  `bpf_map_delete_elem` on `prism_identity` is **rejected by the kernel verifier at
  load time** (`-EACCES`). A buggy or malicious consumer that tries to corrupt an
  identity therefore **cannot load at all** — integrity is a *load-time invariant*,
  not a convention consumers are trusted to honor.
- `BPF_F_RDONLY_PROG` restricts **BPF-program** access only; it does **not** restrict
  the userspace `bpf(2)` syscall. `prismd` (a privileged userspace process) remains
  the **sole writer** of the map via syscall-side `Update`/`Delete`. This is the
  syscall/program asymmetry the flag is designed for.
- Consequence for the facet contract (§5, §6.2): because consumers cannot write the
  bus map, **facet/`flags` RMW by consumers is not possible on the `RDONLY_PROG` bus
  map itself**. A consumer that needs to publish a facet bit MUST do so in a
  *companion* writable map it owns (e.g. the network consumer's own
  `prism_net_stats`), keyed identically by identity or key — never by writing
  `prism_identity`. The bus map carries the daemon-authored identity; consumer state
  lives in consumer-owned maps. (Earlier drafts of §6.2 described in-place facet RMW;
  on the hardened `RDONLY_PROG` bus that path is closed and replaced by consumer-owned
  companion maps. The `flags` field remains daemon-written for any daemon-published
  facets and as reserved space per §7.)
- **Verified on a real kernel (Linux 6.1):** `bpftool map show` reports
  `flags 0x80`; a read-only `cgroup_skb` consumer loads against the pinned map, while
  a writer consumer fails to load with `-EACCES`.

---

## 3. Value layout — `struct prism_identity`

Total size **24 bytes**, natural alignment, **little-endian** (x86-64 / aarch64 LE).
The layout is identical between `abi.PrismIdentity` (Go) and `struct prism_identity`
(C). There is **no padding** — `4+4+8+8 = 24` with 8-byte alignment satisfied.

### 3.1 Byte-offset table

| Offset | Size | C field      | Go field    | Type  | Meaning                                                        |
|-------:|-----:|--------------|-------------|-------|----------------------------------------------------------------|
| 0      | 4    | `identity`   | `Identity`  | `u32` | Numeric identity, 24-bit value in low bits; high byte MUST be 0. |
| 4      | 4    | `flags`      | `Flags`     | `u32` | Facet bits `PRISM_FLAG_*` `[0:2]` (§5) + scheduling-policy sub-fields `[8:17]` (§5.1). Other bits reserved, MUST be 0. |
| 8      | 8    | `label_hash` | `LabelHash` | `u64` | FNV-1a/64 of the canonical label set (§4.3). Change-detect only. |
| 16     | 8    | `updated_ns` | `UpdatedNs` | `u64` | Wall-clock nanoseconds of the daemon's last write to this entry. |

```
byte:  0   1   2   3   4   5   6   7   8 ........ 15  16 ........ 23
      [   identity   ][    flags    ][   label_hash   ][   updated_ns   ]
       u32 LE          u32 LE          u64 LE            u64 LE
```

### 3.2 Field semantics

- **`identity`** — the resolved `NumericIdentity` (§4). Only bits `[0,24)` are
  meaningful; mask with `PRISM_IDENTITY_MASK` (`0x00FFFFFF`) before comparing if you
  want to be defensive, though the daemon always writes the high byte as 0.
- **`flags`** — facet bits `[0:2]` (§5) plus the daemon-authored scheduling-policy
  sub-fields `[8:17]` (§5.1). A reader interested in only one facet MUST test its bit
  (`flags & PRISM_FLAG_X`) and ignore the rest; a scheduler reads the policy class via
  `(flags & PRISM_SCHED_CLASS_MASK) >> PRISM_SCHED_CLASS_SHIFT`. Bits `[3:7]` and `[18:31]`
  are reserved and MUST be 0.
- **`label_hash`** — a 64-bit fingerprint of the canonical label set, **for cheap
  change detection only**. It is *not* the identity and MUST NOT be used as one: two
  different label sets could in principle collide, and the authoritative
  label-set→id mapping lives in the allocator (§4.2). Its intended use: a consumer
  that caches per-identity derived state can compare the stored `label_hash` against
  a remembered one to notice "the labels behind this id changed" without re-reading
  labels. Because the daemon keeps one identity per canonical label set, in practice
  `label_hash` is stable for the life of an identity.
- **`updated_ns`** — set by the daemon on every `Upsert`. Monotonic in practice
  (wall clock); useful for staleness heuristics and debugging. Consumers MUST NOT
  rely on it for ordering correctness, only as a hint.

---

## 4. Identity numbering

**Identity input (spoof-resistant).** The identity for a workload is derived from a
**canonical descriptor** that includes the workload's **security-relevant labels plus
its namespace and service account** (`identity.WorkloadCanonical`). Namespace and
service account are **API/RBAC-controlled and not workload-settable** — a pod cannot
place itself in another namespace or assume a service account it has no RBAC for — so
folding them into the identity input makes the identity **spoof-resistant**: copying a
victim's labels is no longer sufficient to inherit its identity. Throughout this spec
"canonical label set" refers to this canonical descriptor (labels + namespace +
service account); `label_hash` (§4.3) is the FNV-1a/64 of that canonical string and is
for change-detection only.

### 4.1 Space and reserved range

Identities live in a **24-bit space** (`PRISM_IDENTITY_MASK = 0x00FFFFFF`), stored in
the low 24 bits of the `u32` `identity` field. This mirrors the Cilium security-id
model so the bus is interoperable with that ecosystem's mental model.

Reserved IDs `[0, 8]` are well-known and shared across subsystems:

| ID | Name             | Constant (C / Go)                               |
|---:|------------------|-------------------------------------------------|
| 0  | `unknown`        | `PRISM_ID_UNKNOWN` / `IDUnknown`               |
| 1  | `host`           | `PRISM_ID_HOST` / `IDHost`                     |
| 2  | `world`          | `PRISM_ID_WORLD` / `IDWorld`                   |
| 3  | `unmanaged`      | `PRISM_ID_UNMANAGED` / `IDUnmanaged`           |
| 4  | `health`         | `PRISM_ID_HEALTH` / `IDHealth`                 |
| 5  | `init`           | `PRISM_ID_INIT` / `IDInit`                     |
| 6  | `remote-node`    | `PRISM_ID_REMOTE_NODE` / `IDRemoteNode`        |
| 7  | `kube-apiserver` | `PRISM_ID_KUBE_APISERVER` / `IDKubeAPIServer`  |
| 8  | `ingress`        | `PRISM_ID_INGRESS` / `IDIngress`               |

IDs `[9, 255]` are **reserved for future well-known identities** and MUST NOT be
dynamically allocated. An id is "reserved" iff `id < PRISM_ID_MIN_DYNAMIC` (256).

### 4.2 Dynamic range and allocation policy

Dynamic identities occupy **`[256, 0x00FFFFFF]`** (`PRISM_ID_MIN_DYNAMIC` ..
`PRISM_ID_MAX`), i.e. 256 .. 16,777,215.

Allocation is **deterministic, smallest-free, and refcounted** (`pkg/identity`
`Allocator`):

- The mapping **canonical label set → numeric identity is the source of truth.** The
  same canonical label set always resolves to the same identity (idempotent
  `Allocate`).
- A brand-new label set is assigned the **smallest currently-free** id `>= 256`:
  released ids are reused smallest-first (min-heap of freed ids), otherwise the
  high-water mark advances. This keeps the space dense and reproducible across runs
  given the same input order.
- Each `Allocate` adds one reference; `Release` drops one. The id is freed (and
  becomes eligible for reuse) only when its **last** reference is released.
- Exhaustion of the 24-bit dynamic space returns `ErrSpaceExhausted`.

> **Identity is allocated, not hashed.** `label_hash` exists only for change
> detection (§3.2). A consumer that needs to know "are these two workloads the same
> identity?" MUST compare the `identity` field, never `label_hash`.

The reference allocator is in-process. A production deployment would back the
*same* policy with a kvstore/CRD for cluster-wide agreement, but the ABI (the byte
layout and the smallest-free, label-set-keyed semantics) is unchanged — that is the
point of pinning the contract here rather than in the allocator implementation.

### 4.3 `label_hash` computation

`label_hash = FNV-1a/64(canonical)` over the UTF-8 bytes of the canonical string
(§4), with the standard 64-bit parameters:

- offset basis = `1469598103934665603` (`0xCBF29CE484222325`)
- prime        = `1099511628211` (`0x100000001B3`)

```
h = offset_basis
for each byte b of canonical:    # bytes, not runes
    h = (h XOR b) * prime        # mod 2^64
```

An empty canonical string hashes to the offset basis. A consumer never needs to
*compute* this (the daemon writes it); the algorithm is specified so an external
tool can reproduce it for verification.

---

## 5. Flags / facets

```
bit  0     PRISM_FLAG_NET_POLICY    (1 << 0)   owned by: network-policy datapath
bit  1     PRISM_FLAG_SCHED_MANAGED (1 << 1)   owned by: sched_ext scheduler
bit  2     PRISM_FLAG_OBSERVED      (1 << 2)   owned by: observer
bits 3..7                            reserved, MUST be 0
bits 8..10 PRISM_SCHED_CLASS        (3 bits)   scheduling-policy class (§5.1)
bits 11..17 PRISM_SCHED_WEIGHT      (7 bits)   scheduling weight, 0 = unset (§5.1)
bits 18..31                          reserved, MUST be 0
```

| Facet  | Bit                       | Set/cleared by              | Read by             |
|--------|---------------------------|-----------------------------|---------------------|
| net    | `PRISM_FLAG_NET_POLICY`   | network-policy subsystem    | any consumer        |
| sched  | `PRISM_FLAG_SCHED_MANAGED`| sched_ext scheduler         | any consumer        |
| observe| `PRISM_FLAG_OBSERVED`     | observer subsystem          | any consumer        |

**Composability contract (normative):**

1. A consumer MAY set and clear **only its own** facet bit.
2. A consumer MUST treat all other bits (including reserved bits) as **read-only**
   and MUST preserve them unchanged on any write (read-modify-write, §6.2).
3. A consumer MAY read any bit to coordinate behaviour (e.g. the scheduler reading
   `PRISM_FLAG_NET_POLICY` to know a workload is policy-constrained).
4. The daemon (identity producer) does **not** own any facet bit. When it creates an
   entry it writes the facet bits as `0` (plus any scheduling-policy sub-field, §5.1);
   thereafter it MUST preserve consumer-set facet bits on updates (see §6.1). Subsystem
   bits therefore survive identity/`label_hash` refreshes.

### 5.1 Scheduling-policy sub-fields (the "thick" bus)

The bus carries not only *which* workload an entry is (the numeric `identity`) but a
small **per-identity scheduling-policy descriptor** telling a scheduler *how* to treat
it. This makes the bus "thick in meaning, thin in bytes": the descriptor is packed into
**previously-reserved bits of `flags`**, so the value stays **24 bytes, byte-identical**
(no `value_size` change — §7). The policy sub-fields are **disjoint** from the facet bits
[0:2], which are unchanged and frozen.

```
bits [8:10]  PRISM_SCHED_CLASS   (3 bits)   latency class
bits [11:17] PRISM_SCHED_WEIGHT  (7 bits)   relative weight, 0 = unset
```

**Latency class** (`(flags & PRISM_SCHED_CLASS_MASK) >> PRISM_SCHED_CLASS_SHIFT`):

| Value | Constant (C / Go)                                  | Meaning                                            |
|------:|----------------------------------------------------|----------------------------------------------------|
| 0     | `PRISM_SCHED_CLASS_UNSET` / `SchedClassUnset`       | no policy — the scheduler uses its own heuristic   |
| 1     | `PRISM_SCHED_CLASS_CRITICAL` / `SchedClassCritical` | latency-critical — strongest priority/boost        |
| 2     | `PRISM_SCHED_CLASS_NORMAL` / `SchedClassNormal`     | explicitly normal — no boost                       |
| 3     | `PRISM_SCHED_CLASS_BATCH` / `SchedClassBatch`       | batch — deprioritized                              |
| 4..7  | —                                                  | reserved for future classes                        |

**Weight** is an optional 7-bit relative weight (`1..127`; `PRISM_SCHED_WEIGHT_NEUTRAL`
= 64 is the consumer-side "1×"); `0` means unset. It **refines** a class (e.g. amplifies
a critical task's boost) and a consumer that ignores it loses no correctness.

**Who writes it.** The descriptor is **daemon-authored**, like `identity`/`label_hash`:
`prismd` derives it from a pod label (`prism.io/latency-class`, optional `prism.io/weight`)
and stamps it on every write. The `prism.io/` labels are **excluded from the identity
canonical form** (§4), so policy is **decoupled from identity** — two replicas that differ
only in a policy label keep one identity, and retuning a pod's class does not renumber it.
Because the bus map is `BPF_F_RDONLY_PROG` (§2.2), consumers **read** these bits (never
write them), exactly as for the facet bits.

**Semantics for consumers.** `UNSET` (and "no entry" / "off-bus") MUST be treated as "use
my own policy/heuristic", so the sub-field is **strictly opt-in and backward compatible**:
a consumer built before §5.1 that reads only the facet bits stays correct, and an identity
with no class behaves exactly as it did before this field existed. A consumer maps the
class to **its own knobs** — the bus conveys the operator's *intent*, never a mechanism.
(Reference mapping: the `scx_bpfland` retrofit applies the class as a *one-sided priority
floor* on bpfland's own deadline knob — `CRITICAL`→`min(stock_deadline, vtime_now − slice_lag)`
(at least a `slice_lag` boost, but never below what bpfland's heuristic already grants, so it
only ever *adds* priority; weight amplifies the floor above neutral), `BATCH`→a deadline penalty
from vruntime alone, `NORMAL`/`UNSET`→its stock sleep heuristic. Measured in-kernel on 6.17: the
floor preserves bpfland's sleepy-case win with no regression *and* protects a CPU-bound
latency-critical task the sleep heuristic mislabels batch (measured in-kernel on Linux 6.17). An earlier
flat *replace* mapping regressed the sleepy case and is retained only as `main.bpf.c.replace`.)

**Normative source:** `EncodeSchedPolicy` / `SchedClass*` in `pkg/abi/abi.go` and the
`PRISM_SCHED_*` macros in `bpf/prism_maps.bpf.h` (kept byte-identical; cross-checked by
`pkg/abi/abi_test.go`).

---

## 6. Concurrency and ownership

The map is a lock-free shared-memory channel. Ownership is partitioned by field, so
that concurrent writers never need a lock if they respect their lane.

### 6.1 The daemon is the sole writer of `identity` and `label_hash`

`prismd` is the **only** writer of the `identity` and `label_hash` fields, and the
only creator/deleter of entries. It also stamps `updated_ns`. Consumers MUST NOT
write these fields. **This is enforced by the kernel, not by convention:** the map is
created `BPF_F_RDONLY_PROG` (§2.2), so a consumer BPF program cannot write *any* field
of the bus map — it would fail the verifier at load (`-EACCES`). The daemon writes via
the userspace `bpf(2)` syscall, which the flag does not restrict, so it remains the
sole writer.

On refresh of an existing entry (labels changed, or periodic re-sync) the daemon
preserves any consumer facet bits and overwrites `identity` / `label_hash` / `updated_ns`.
It also (re)derives and writes the **scheduling-policy sub-fields** (§5.1) from the pod's
`prism.io/` labels on every write — those are daemon-authored, like `identity`, and on the
hardened `RDONLY_PROG` bus (§2.2) consumers cannot write *any* `flags` bit, so the daemon
is effectively the sole writer of the whole word; the "preserve consumer bits" rule is the
softer-model invariant retained for the facet bits.

### 6.2 Consumers RMW only their own flag bit

> **Updated for the hardened (`RDONLY_PROG`) bus (§2.2):** on the read-only bus map a
> consumer BPF program **cannot** write `prism_identity` at all — the in-place facet
> RMW shown below is **rejected by the verifier**. The supported pattern is to keep a
> consumer-owned **companion** writable map (keyed identically) for any facet/flag a
> consumer needs to publish (e.g. the network consumer's `prism_net_stats`), and to
> treat `prism_identity` as **read-only** (`Lookup` only). The example below is retained
> as the historical/userspace pattern and as the shape a daemon-published facet takes
> on the daemon's own (syscall) write path; a `RDONLY_PROG`-bus consumer MUST NOT
> attempt it against `prism_identity`.

A consumer that wants to set/clear its facet performs an atomic-style
read-modify-write of the whole 24-byte value:

```c
struct prism_identity v;
if (bpf_map_lookup_elem(&prism_identity, &key, &v))   // copy out
    return;                 // no identity yet — nothing to mark
v.flags |= PRISM_FLAG_SCHED_MANAGED;                  // touch ONLY your bit
bpf_map_update_elem(&prism_identity, &key, &v, BPF_EXIST);  // write back
```

Notes:

- Use `BPF_EXIST` so a consumer never *creates* an entry (only the daemon creates).
- The RMW is not strictly atomic against a concurrent daemon write; the daemon's own
  RMW (§6.1) preserves flags, so the only true race is two writes to *different*
  bits interleaving. Because each subsystem touches a disjoint bit and preserves the
  rest, a lost update can only transiently drop a peer's bit; both writers re-run
  their RMW on the next event, so the system converges. Subsystems that need hard
  atomicity on a single facet MAY use a `BPF_MAP_TYPE_HASH` with `BPF_F_LOCK`-style
  spin locking in a *companion* map keyed identically — but the bus map itself stays
  lock-free for read-mostly access. (This is a deliberate simplicity/perf tradeoff;
  see Prism's decision log.)
- On a real kernel from a per-CPU/SMP datapath the same pattern applies; cilium/ebpf
  userspace consumers use `Map.Lookup` then `Map.Update(..., ebpf.UpdateExist)`.

### 6.3 Lifecycle

- **Create:** the daemon creates a map entry for a workload instance the first time
  it observes a pod carrying a non-empty canonical label set, allocating (or reusing)
  the identity for that label set. (A pod whose labels canonicalize to the empty
  string — see §4 — is `unmanaged` and SHOULD map to `IDUnmanaged`, not a dynamic id.)
- **Update:** label changes recompute canonical → identity (which may change the
  `identity`/`label_hash` if the security-relevant labels changed) and refresh
  `updated_ns`, preserving `flags` (§6.1).
- **Delete:** the daemon deletes the entry when the last pod instance for that key is
  gone, and `Release`s the identity reference. When an identity's refcount reaches
  zero its number is returned to the free pool for reuse.
- Consumers SHOULD NOT cache map values across a delete without revalidating via
  `Lookup`; a stale key may have been reused for a different cgroup/identity.

---

## 7. Versioning

The ABI is **v1**. Evolution rules, in priority order:

1. **Append-only struct growth.** New fields MAY be appended after `updated_ns`
   (offset 24+). Existing offsets/sizes/meanings are frozen forever. Because the map
   stores fixed-size values, growing the struct changes the map's `value_size`,
   which is a *map-level* incompatibility — see rule 3.
2. **Flag bits are append-only, by region.** The `flags` word is partitioned: facet
   bits `[0:2]` (frozen) grow into the reserved `[3:7]`; the scheduling-policy sub-fields
   occupy `[8:17]` (§5.1, frozen); future descriptors claim the reserved `[18:31]`. New
   facets/classes claim the next free bit in their region; existing bits and their meanings
   are frozen. Reserved bits MUST remain 0 until assigned, so an old consumer ignoring them
   stays correct. None of this changes `value_size`, so it is **not** a map-breaking change
   (contrast rule 3).
3. **Struct/layout-breaking changes get a new map.** Any change to an existing
   field's offset, size, or meaning, or any `value_size` change, MUST be shipped as a
   **new pinned map with a new name** (e.g. `prism_identity_v2`) so v1 and v2
   consumers can coexist during migration. Never silently repurpose `prism_identity`.
4. **Version discovery via a separate meta key.** Producers and consumers negotiate
   version out-of-band through a small companion map `prism_meta`
   (`BPF_MAP_TYPE_ARRAY`, one entry) carrying `{abi_version u32, value_size u32,
   feature_flags u64}`. A consumer SHOULD read `prism_meta[0]` on attach; if
   `abi_version` exceeds the one it was built against it MUST fall back to reading
   only the v1 prefix it understands (safe because of rule 1) or refuse to attach.
   *(`prism_meta` is reserved by this spec; absence means v1.)*

A consumer built against this document and reading only fields `[0,24)` will remain
correct against any future append-only producer.

---

## 8. Worked example

A pod in namespace `shop` with labels:

```yaml
labels:
  app: checkout
  io.kubernetes.pod.namespace: shop
  pod-template-hash: 7d9f8c            # volatile  → dropped
  kubectl.kubernetes.io/last-applied: "..."  # ignored prefix → dropped
```

**Step 1 — canonicalize (§4).** Drop `pod-template-hash` (in `ignoredKeys`) and the
`kubectl.kubernetes.io/` prefix (in `ignoredPrefixes`). Keep the rest, render as
sorted `k=v` joined by `;`:

```
canonical = "app=checkout;io.kubernetes.pod.namespace=shop"   (45 bytes)
```

**Step 2 — label_hash (§4.3).** FNV-1a/64 over those 45 bytes:

```
label_hash = 0x1A3CD5C42D7A7A5F   (= 1890620982142073439)
```

**Step 3 — allocate identity (§4.2).** First dynamic label set on a fresh daemon →
smallest free id `>= 256`:

```
identity = 256   (0x00000100)
```

**Step 4 — facets.** Suppose only the observer has marked it so far:

```
flags = PRISM_FLAG_OBSERVED = 0x00000004
```

**Step 5 — map entry.** Key = the pod's cgroup id (real kernel) or synthetic u64
(sim). Value (`updated_ns` shown as an example timestamp
`1716998400000000000` = `0x17D400F29FEA0000`):

```
prism_identity[ <cgroup_id> ] = {
    identity   = 256                    // 0x00000100
    flags      = 0x00000004             // OBSERVED
    label_hash = 0x1A3CD5C42D7A7A5F
    updated_ns = 1716998400000000000
}
```

**Raw 24 value bytes (little-endian):**

```
offset  field        bytes (LE)
0..3    identity     00 01 00 00
4..7    flags        04 00 00 00
8..15   label_hash   5f 7a 7a 2d c4 d5 3c 1a
16..23  updated_ns   00 00 ea 9f f2 00 d4 17
```

A sched_ext scheduler that later starts managing this workload would read the value,
OR in its own bit, and write back:

```
flags: 0x00000004  →  0x00000006     // OBSERVED | SCHED_MANAGED
```

leaving `identity`, `label_hash`, and the observer's bit untouched — the
composability contract (§5) in one operation.
