# Prism Central Coordinator — Cluster-Wide Identity Coherence

**Status:** design (not yet implemented)
**Audience:** Prism maintainers; reviewers who already know the ABI spec (`spec/README.md`) and the in-process allocator (`pkg/identity/allocator.go`).
**Scope:** *only* the numeric-identity assignment authority. The 24-byte bus ABI, the canonicalization rules, and the per-node datapath are unchanged by this doc — that is a hard constraint, not an aspiration (§7).

> **Normative anchors.** This document builds on the *exact* allocator methods in
> `pkg/identity/allocator.go` — `Allocate(canonical) (id, created, err)`,
> `Adopt(canonical, id) bool`, `Reserve(id)`, `Release(canonical) bool`,
> `Lookup(canonical) (id, bool)` — and the per-node controller event loop in
> `pkg/sync/controller.go` / `pkg/sync/robustness.go`. Where this doc and those
> files disagree, the source files win. Spec §4.2 already states the intended end
> state: *"A production deployment would back the same policy with a kvstore/CRD
> for cluster-wide agreement, but the ABI … is unchanged."* This is the design of
> that backing.

---

## 1. Problem — node-local allocation is not coherent across nodes

### 1.1 What is coherent today, and what is not

`prismd` runs as a **DaemonSet, one Pod per node** (`deploy/daemonset.yaml`), each
scoped to *its own node's* pods via the `spec.nodeName` field selector
(`-node=$(NODE_NAME)`, `Controller.NodeName`). Each node's `prismd` owns a **private,
in-process** `identity.Allocator` (`NewController` → `alloc: identity.NewAllocator()`).
Nothing in the system shares allocator state between nodes: the RBAC
(`deploy/rbac.yaml`) is **read-only on Pods** (`get/list/watch`, "No write verbs:
prismd never mutates the cluster"), so a node cannot even publish what it decided.

Two pieces of the 24-byte `struct prism_identity` therefore behave very differently
across nodes:

| Field (ABI §3.1) | Cross-node behavior | Why |
|---|---|---|
| `label_hash` (`u64`, offset 8) | **Coherent.** Same canonical → same hash on every node. | Pure function `identity.LabelHash(WorkloadCanonical(...))` — no allocator state. |
| `identity` (`u32`, offset 0) | **NOT coherent.** Same workload may get a *different* number on each node. | Output of the **smallest-free** policy over a **per-node** allocator. |

The numeric `identity` is assigned by `Allocator.mintLocked()`: the smallest free id
`>= MinDynamicID` (256), reused-first from the `freed` min-heap, else the high-water
`next`. That number is a pure function of **the order in which a given node observed
its label sets** — which differs node to node. Node A that first sees `checkout` then
`cart` assigns `checkout=256, cart=257`; Node B that sees them in the other order (or
only sees `cart`) assigns `cart=256`. Both are internally correct and both honor the
ABI; they simply **disagree on the number for the same workload**.

### 1.2 Concrete breakage

The ABI is explicit (§4.2, §3.2): *"A consumer that needs to know 'are these two
workloads the same identity?' MUST compare the `identity` field, never `label_hash`."*
`label_hash` is "change-detect only" and "could in principle collide". So every
consumer that reasons about *identity equality* reasons over the one field that is
**not** coherent across nodes:

- **Cross-node network policy.** A tc/XDP datapath enforces "allow `frontend` →
  `backend`" by matching the peer's numeric `identity` (the whole point of the
  Cilium-style security-id model the ABI mirrors, §4.1). When a packet crosses from
  Node A to Node B, the policy id baked into the packet path / derived locally is
  Node A's number for `backend`, but Node B's datapath holds Node B's number for the
  same workload. The match **fails or, worse, aliases a different workload** that
  happens to hold that number on the other node. Identity-based policy is only sound
  if the id is a cluster-global name. Today it is a node-local name.

- **Distributed tracing / observation.** The observer facet (`PRISM_FLAG_OBSERVED`)
  and any cross-node correlation (a span attributed to `identity=4711` on Node A,
  then a downstream span on Node B) silently mis-join: `4711` means different
  workloads on the two nodes. Aggregation by `identity` across nodes produces
  garbage; the operator cannot ask "show me all traffic for this workload" cluster-wide.

- **Migration / reschedule.** A pod evicted from Node A and rescheduled to Node B
  changes numeric identity even though its labels (hence `label_hash`) are identical.
  Any consumer state keyed on the old number is orphaned.

`label_hash` *is* coherent, but the ABI forbids using it as identity (collision risk
+ it is explicitly "not the identity"). So coherence must be delivered on the
`identity` field itself, by making **assignment cluster-global** instead of node-local.

---

## 2. Design — a cluster identity authority as the single source of truth

### 2.1 The authority and what it stores

Introduce a **Prism Coordinator**: the cluster-wide authority for the mapping

```
canonical label set  ->  NumericIdentity
```

This is the *same* mapping `Allocator.byLabels` holds in-process today, **lifted to
cluster scope**. The canonical string is exactly `identity.WorkloadCanonical(labels,
namespace, serviceAccount)` — the spoof-resistant form already used in
`Controller.HandlePod` — so the authority keys on the identical, API/RBAC-anchored
string every node computes. The policy is identical to spec §4.2 (deterministic,
smallest-free, refcounted); only the *owner* of the counter moves from per-node to
cluster.

Two interchangeable backends (operator's choice; the controller code is agnostic):

**(A) CRD `PrismIdentity` (recommended default).** A cluster-scoped custom resource,
one object per *live identity*:

```yaml
apiVersion: prism.io/v1alpha1
kind: PrismIdentity
metadata:
  # name is derived from the label_hash so the object name is itself
  # content-addressed and collision-checkable: "id-<hex64(label_hash)>".
  name: id-1a3cd5c42d7a7a5f
spec:
  canonical: "io.prism/namespace=shop;io.prism/serviceaccount=default;app=checkout"
  labelHash: 1890620982142073439        # FNV-1a/64, for fast index + cross-check
  identity: 256                          # the cluster-global NumericIdentity
status:
  refs: 7                                # cluster-wide live references (optional, see §3.4)
  observedNodes: ["node-a","node-b"]     # optional, for ops/debug only
```

The CRD is attractive because: it reuses the API server prismd already talks to (no
new datastore to operate); RBAC and audit come for free; `kubectl get prismidentity`
makes the bus introspectable; and a `Watch` gives every node a live feed for free
(§2.3). A small leader-elected **prism-coordinator Deployment** owns *minting* (it is
the only writer of `spec.identity`), so the smallest-free counter has exactly one
authority — no two nodes ever race to mint.

**(B) kvstore (etcd / Consul) — for Cilium-parity deployments.** A transactional
key space `prism/identities/<canonical>` → `{identity, refs}` plus a monotonically
guarded free-list, mutated under a CAS/transaction so "smallest free" is atomic. Same
semantics; chosen when an operator already runs a kvstore and wants prismd off the
API-server write path.

Either way the authority is the **single source of truth for the number**, replacing
the per-node `mintLocked()` decision — *not* the per-node allocator, which stays
exactly as-is and now acts as a **local cache + datapath writer** (§2.2).

### 2.2 Per-node prismd flow on a new workload — *ask, then Adopt locally*

The key insight: the coordinator and the local `Allocator` compose through the
allocator's **existing** `Adopt(canonical, id)` method. `Adopt` was built precisely to
bind a canonical set to an **externally chosen** number with refcount 1 (it is how
restart-reseed reclaims a survivor's id today). It is the natural seam for "the
coordinator chose this number; install it locally." No allocator change is required.

The change is localized to the **allocate branch** of `Controller.HandlePod`
(`pkg/sync/controller.go` lines ~245-276). Today that branch is:

1. try restart-reseed reclaim via `Adopt(canon, rid)`;
2. else `Allocate(canon)` (mint locally).

The coordinated branch interposes one step between (1) and (2):

```
canon := identity.WorkloadCanonical(pod.Labels, pod.Namespace, pod.Spec.ServiceAccountName)

// (existing) unchanged-canon fast path: this UID already mapped to this exact
// canon -> Lookup the id, refresh the timestamp, do NOT churn the refcount, and
// make no coordinator call (controller.go: `hadPrev && prev.canon == canon`).
if id, ok := c.alloc.Lookup(canon); ok && hadPrev && prev.canon == canon {
    return c.upsert(wkey, id, canon, pol)
}

// (existing) a NEW UID landing on a canon already mapped locally (a second pod
// of a shared label set) -> Allocate bumps the refcount and returns the same id;
// still no coordinator call.
if _, ok := c.alloc.Lookup(canon); ok {
    id, _, _ := c.alloc.Allocate(canon)   // refcount++, id unchanged
    return c.upsert(wkey, id, canon, pol)
}

// (existing) restart-reseed reclaim, unchanged.
// ... Adopt(canon, rid) on a surviving local number ...

// (NEW) first local observation of canon -> ask the cluster authority.
gid, err := c.coord.Resolve(ctx, canon)   // canonical -> cluster-global id
if err == nil {
    // Coordinator is authoritative. Install its number locally via the
    // allocator's existing Adopt seam (binds canon->gid, refcount 1).
    if !c.alloc.Adopt(canon, gid) {
        // canon raced onto the local map between Lookup and here; Adopt
        // declines and the existing entry already holds the right id.
        gid, _ = c.alloc.Lookup(canon)
    }
    return c.upsert(wkey, gid, canon, pol)
}
// err != nil -> coordinator unreachable: DEGRADE to local Allocate (§3).
```

`Resolve(canonical)` on the coordinator is "get-or-create": if a `PrismIdentity`
exists for that canonical (matched via the `labelHash` index then a `spec.canonical`
equality check to rule out the astronomically-rare FNV collision), return its
`spec.identity`; otherwise the leader mints the cluster-global smallest-free number,
creates the object, and returns it. **Minting happens in exactly one place in the
cluster**, so the number is globally unique and globally agreed.

Crucially this preserves all the *local* allocator invariants the datapath depends on:
the local `byLabels` still maps `canon → {id, refs}`, `Release` on pod delete still
refcounts locally, and `Lookup` (the upsert fast path, the read path) is untouched.
The coordinator only supplies the *number* the local allocator would otherwise have
minted; everything downstream of "what number is this canonical?" is identical.

### 2.3 Reconciliation / watch — keeping every node's cache warm and correct

The coordinator is the source of truth; each node's `Allocator` is a **read-through
cache**. Two mechanisms keep them consistent:

1. **Watch (push).** Each node `prismd` opens a `Watch` on `PrismIdentity` (CRD) or a
   prefix-watch on the kvstore. On any add/update it pre-warms (or corrects) its local
   binding via `Adopt(canon, id)` for canonicals it cares about, so the *first* pod of
   a workload to land on a node frequently finds the number already cached (no
   blocking `Resolve` on the hot-ish assign path). A watch event that changes the
   number for a `canon` the node already holds is reconciled by `Release(old canon)` +
   `Adopt(canon, newID)` — but note this only happens during the one-time convergence
   after a degraded split (§3), never in steady state.

2. **Reconcile (pull).** Extend the existing periodic sweep
   (`Controller.Reconcile`, `reconcileLoop`, every `defaultResync` = 10 min). Today it
   GCs sink keys with no live pod. The coordinated version *additionally* walks its
   local `byLabels` and, for each canonical, confirms the local id equals the
   coordinator's id; any drift (only possible post-degrade) is corrected via
   `Release`+`Adopt`, and any local-only id minted while degraded is **reconciled
   upward** (§3.2). This is the same "self-heals within one resync window" contract the
   doc already makes for missed deletes.

The watch makes the common case push-driven and cheap; the reconcile loop is the
backstop that guarantees eventual convergence even if a watch event is missed.

---

## 3. Degrade strategy — coordinator unreachable ⇒ local autonomy, reconcile on return

**Non-negotiable: a node never blocks identity propagation on the coordinator.** The
DaemonSet must keep stamping the bus during an API-server hiccup, a coordinator
rollout, or a network partition — that is the whole reason identity lives in a
per-node daemon writing a per-node pinned map in the first place.

### 3.1 Fall back to local `Allocate()`

If `coord.Resolve(ctx, canon)` errors (timeout, coordinator down, partition), the
allocate branch falls straight through to the **existing** path:

```go
id, created, err = c.alloc.Allocate(canon)   // unchanged code, local smallest-free
```

The node mints a **provisional, node-local** number from its own allocator — exactly
today's behavior. The datapath keeps working; the only cost is that this number may
not match other nodes *until reconciliation*. The node records that this `canon` was
locally minted (a small `pendingSync` set, analogous to `reseedByHash`) so the
reconcile pass knows to confirm/repair it.

A short bounded `Resolve` timeout (e.g. 250 ms) plus a circuit breaker means a dead
coordinator costs at most one timeout per *new* canonical, after which the node trips
the breaker and goes fully local until a probe succeeds — it does not pay the timeout
on every pod.

### 3.2 Reconcile when the coordinator returns — `Adopt` / `Reserve`

On reconnect (watch re-established or the breaker's probe succeeds), for each
locally-minted `canon` in `pendingSync` the node calls `coord.Resolve(canon)` to learn
the **authoritative** number and converges using the allocator's existing primitives:

- **Authoritative id == local id:** nothing to do (the smallest-free policy is
  deterministic, so an isolated node that minted in the same order often agrees by
  luck). Drop from `pendingSync`.

- **Authoritative id != local id:** the cluster wins. Drop the local binding with
  `Release(canon)` and then install the cluster number with `Adopt(canon,
  authoritativeID)`. Note the exact allocator contract: `Release` only deletes the
  `byLabels[canon]` entry when it drops the **last** reference (it returns `false` and
  keeps the binding while `refs > 0`), and `Adopt` returns `false` if `canon` is still
  mapped. So when the node holds N live pods of this canon, the renumber must drop all
  N local refs (Release until it returns `true`, or equivalently fully evict the canon)
  before `Adopt` can succeed — a partial Release leaves the old number in place. `Adopt`
  then clears any reservation and rebinds with refcount 1; the remaining live pods'
  references are re-established on the next resync as their events re-flow through
  HandlePod. This renumbers the workload on *this* node to the cluster-global value —
  the same kind of benign renumber the controller already tolerates (`robustness.go`:
  "a benign identity renumber that self-corrects on the next resync").

- **No authoritative entry yet (the node was the first to see this canon, cluster-wide):**
  the node **promotes** its provisional number — it calls
  `coord.Claim(canon, localID)`, a CAS create that succeeds iff no other node already
  claimed that canonical *or that number*. On success the local number becomes the
  cluster number (zero renumber). On conflict (two partitioned nodes minted the same
  number for different canonicals, or different numbers for the same canonical), the
  coordinator's existing entry wins and the node falls to the `Adopt` case above.

`Reserve(id)` is reused on the **coordinator/leader** side: when the leader boots (or
fails over) it must rebuild the smallest-free counter from the surviving
`PrismIdentity` objects exactly as a node rebuilds from the surviving pinned map
today. The leader `Reserve`s every `spec.identity` it reads from the CRD/kvstore
before it mints anything, so a freshly-minted number can never collide with a live
one — this is `pkg/sync/robustness.go::Reseed` lifted to cluster scope, using the
identical `Reserve`-then-`Adopt`/mint discipline.

### 3.3 Why local autonomy is safe

Within a single node the identity numbers are *always* self-consistent (one
allocator, one writer), so **node-local correctness never degrades** — net policy and
tracing *within* a node are correct even fully partitioned. Only *cross-node* equality
is temporarily relaxed during a partition, and it self-heals on reconnect. This is the
correct failure mode: degrade the global property, never the local one, and converge.

### 3.4 Refcount ownership note

The authoritative number's lifetime is governed by the *cluster's* aggregate refcount,
not any one node's. The simplest correct model: the coordinator's `spec.identity` is
**stable for the life of the canonical label set** and is only retired when *no node*
holds it (each node reports add/remove of a canonical; the coordinator GCs a
`PrismIdentity` when `observedNodes` empties and a grace period elapses). Per-node
`Release` continues to free the *local* number for *local* reuse pressure, but a number
is not returned to the cluster free-list until globally unreferenced. This keeps a
workload's id stable across pod churn and node migration — the coherence property §1
is about.

---

## 4. Why the hot path stays 3.6 ns

The measured **≈3.6 ns** per-decision figure (native consumer; `paper/FACTS.md`,
`consumer_overhead.c`) is the cost of a **consumer reading one entry from the pinned
`prism_identity` map** — `bpf_map_lookup_elem` on a `BPF_MAP_TYPE_HASH`. The
coordinator is **nowhere near that path**:

- **The read path is unchanged, by construction.** Consumers read the pinned map; the
  map is `BPF_F_RDONLY_PROG` and daemon-written (ABI §2.2, §6.1). The coordinator adds
  no field, no indirection, no lookup — it changes *which number* the daemon writes
  into offset 0, not *how* anyone reads it. A 256 written by a cluster authority and a
  256 minted locally are byte-identical to the consumer.

- **The coordinator touches only the rare *assign* path.** `coord.Resolve` is called
  **once per (node, canonical) on first local observation** — the same branch that
  calls `Allocate`/`Adopt` today. The steady-state controller paths pay nothing new:
  the unchanged-canon fast path (`Lookup` + timestamp refresh), the refcount bump for
  an already-mapped canonical, the upsert, and `Release` on delete are all
  allocator-local and never call the coordinator. The number of `Resolve` calls over a
  cluster's life is bounded by the number of **distinct workloads**, not pods, not
  packets, not scheduling decisions, not events.

- **First-touch latency is amortized off the datapath.** Even the first-observation
  `Resolve` is in **userspace `prismd`**, between an informer event and a sink
  `Upsert` — it is not on any consumer's per-packet / per-scheduling-decision path. A
  consumer reading the map during that brief window sees either no entry (treat as
  "no identity yet", ABI §2.1) or the prior value; it never blocks on the coordinator.
  And with the §2.3 watch pre-warming the local cache, most first-touches resolve from
  the local `Allocator` with **no** coordinator round-trip at all.

So the coordinator buys cluster-wide coherence on the **assign** path (rare, userspace,
amortized) while leaving the **read** path (hot, in-kernel, 3.6 ns) byte-for-byte
identical. That separation is exactly what the ABI's "daemon is the sole writer,
consumers only read" contract makes possible.

---

## 5. Benefits

- **Cluster-wide identity coherence (the goal).** The numeric `identity` becomes a
  global name: cross-node net policy matches correctly, traces join correctly, and a
  rescheduled pod keeps its number. The ABI's "compare `identity`, not `label_hash`"
  rule (§4.2) becomes safe across nodes.
- **Zero ABI change, zero datapath change.** Nothing in the 24-byte struct, the pin
  path, the map flags, or any consumer moves (§7).
- **Builds on existing seams.** Reuses `Adopt`/`Reserve`/`Release`/`Lookup`/`Allocate`
  verbatim; the only new controller code is the `Resolve`/degrade branch and a small
  `pendingSync` set modeled on `reseedByHash`.
- **Operability.** With the CRD backend the whole identity table is `kubectl
  get`-able and audited; the coordinator is a standard leader-elected Deployment.
- **Deterministic, dense numbering preserved.** Smallest-free still holds, now
  cluster-wide, so the Cilium-parity mental model and the dense 24-bit space survive.

## 5b. Tradeoffs

- **New control-plane component + write RBAC.** prismd today is read-only on Pods
  (`deploy/rbac.yaml`). Nodes now need a *narrow* write/get/watch on
  `prismidentities.prism.io` (or kvstore creds); the leader needs create/update.
  Mitigation: scope to the CRD only; keep Pods read-only; the node role gets
  `get/list/watch` + a single `Claim`-style update, not blanket write.
- **Coordinator availability is a new dependency** — explicitly mitigated by the
  degrade strategy (§3): its failure costs cross-node coherence *temporarily*, never
  node-local correctness or bus availability.
- **API-server write load (CRD backend).** Bounded by distinct workloads, not pods;
  watches + local caching keep it low. The kvstore backend exists for operators who
  want prismd off the API-server write path entirely.
- **Eventual (not instant) cross-node consistency after a partition.** Convergence is
  bounded by the reconcile window. Acceptable: identity is read-mostly and policy
  engines already tolerate brief propagation lag.
- **Renumber-on-converge.** Post-degrade convergence can renumber a workload on a node
  (§3.2). This is the same benign renumber the codebase already documents and
  self-heals; consumers are told (ABI §6.3) not to cache across a change without
  revalidating via `Lookup`, and `label_hash` change-detection lets them notice.

---

## 6. Migration path

The rollout is incremental and never breaks a running cluster, because the coordinated
allocator is a **strict superset** of today's behavior (coordinator-down ≡ today).

1. **Ship the allocator unchanged.** No edits to `pkg/identity/allocator.go`. The
   coordinator integrates purely through `Resolve` → `Adopt`/`Allocate` in the
   controller; `Adopt`/`Reserve`/`Release`/`Lookup` are used exactly as specified.
2. **Phase 0 — feature-flagged off (default).** Add a `-coordinator=<crd|kv|off>` flag
   to `prismd` (default `off`). Off ≡ current node-local behavior, bit-for-bit. CI and
   the bench harness keep running the local path; the 3.6 ns / 13 ns numbers are
   unaffected.
3. **Phase 1 — coordinator advisory (shadow).** Deploy the leader + CRD. Nodes call
   `Resolve` but only **log/meter** disagreement between the local mint and the
   coordinator's number (no `Adopt` yet). Validates the leader, RBAC, watch load, and
   convergence with zero datapath risk.
4. **Phase 2 — coordinator authoritative.** Flip nodes to `Adopt(canon, gid)` on
   `Resolve` success, degrade to `Allocate` on failure (§3). Roll node-by-node
   (`RollingUpdate`, `maxUnavailable: 1`, already in the DaemonSet). A mixed fleet is
   safe: un-flipped nodes are simply "permanently degraded" w.r.t. coordination and
   still correct locally; coherence improves monotonically as nodes flip.
5. **Phase 3 — default on.** Make `-coordinator=crd` the default once soaked.

At every phase the **frozen 24-byte ABI is untouched**: the struct, offsets, flags,
pin path, `BPF_F_RDONLY_PROG`, and the smallest-free / label-set-keyed semantics are
all exactly as `spec/README.md` freezes them. The coordinator changes *who decides the
number*, never *what the number is, how it is stored, or how it is read* — which is
precisely why spec §4.2 could promise this backing "with the ABI unchanged."

---

## Appendix A — exact allocator methods this design relies on

From `pkg/identity/allocator.go` (semantics quoted/paraphrased from the source):

| Method | Signature | Role in this design |
|---|---|---|
| `Allocate` | `(canonical string) (id NumericIdentity, created bool, err error)` | **Degrade path.** Local smallest-free mint when the coordinator is unreachable (§3.1). Unchanged code. |
| `Adopt` | `(canonical string, id NumericIdentity) bool` | **Coordinator install seam.** Binds canon→coordinator-chosen id (refcount 1), clears any reservation; returns false if canon already mapped (caller falls back to `Lookup`/`Allocate`). The composition point with the authority (§2.2). |
| `Reserve` | `(id NumericIdentity)` | **Leader reseed.** On the coordinator at boot/failover, Reserve every surviving `spec.identity` before minting, so a fresh mint never collides — the cluster-scope analogue of `robustness.go::Reseed` (§3.2). |
| `Release` | `(canonical string) bool` | **Refcount + reconcile.** Drops one local reference (pod delete); returns `true` and frees the id only when the **last** reference goes (else `false`, binding kept). It is the first half of the `Release`+`Adopt` renumber when converging post-degrade — which requires dropping *all* local refs before `Adopt` can rebind (§3.2). Unchanged in the steady state. |
| `Lookup` | `(canonical string) (NumericIdentity, bool)` | **Hot/assign fast path.** Already-mapped canon → id with no coordinator call (§2.2); the read-side path that keeps 3.6 ns untouched (§4). |

These five methods are sufficient; the coordinator adds **no** new allocator API.
