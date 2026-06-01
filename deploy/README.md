# Deploying prismd (Prism Workload Identity Bus)

`prismd` is a DaemonSet: one Pod per node. Each Pod watches the Pods scheduled
to **its** node, derives a stable numeric workload identity, and writes that
identity into the shared, pinned BPF map `prism_identity` at
`/sys/fs/bpf/prism/identity`. Every BPF consumer (sched_ext scheduler, tc/XDP
net policy, observer) reads that one map — the shared map **is** the bus.

```
deploy/
  namespace.yaml      # namespace prism-system (+ PSA privileged labels)
  rbac.yaml           # ServiceAccount + ClusterRole (get/list/watch pods) + binding
  daemonset.yaml      # the prismd DaemonSet
  kustomization.yaml  # ties the three together; pins the image tag
```

## Prerequisites

- A Kubernetes cluster and `kubectl`.
- The `prism/prismd:dev` image **built and available on every node** (it is
  never pulled from a registry — the manifests use `imagePullPolicy:
  IfNotPresent`).
- For the **real kernel path** on each node:
  - Linux **6.12+** (sched_ext lands in 6.12; e.g. Ubuntu 25.04 ships 6.14,
    Fedora 41+). On older kernels prismd transparently falls back to a
    userspace sim sink — it still runs and propagates identity in its logs.
  - **bpffs mounted at `/sys/fs/bpf`** on the host (`mount -t bpf bpf /sys/fs/bpf`).
  - The Pod must be able to create/pin BPF maps and read the cgroup tree —
    granted by the `privileged` securityContext (default) or the least-privilege
    capability set documented in `daemonset.yaml`.
  - cgroup **v2** at `/sys/fs/cgroup` (mounted read-only into the Pod).

## 1. Build the image

The repo ships a `Dockerfile` (multi-stage: `golang:1.24` builder →
`gcr.io/distroless/static` final, a fully static `CGO_ENABLED=0` binary). Build
it from the repo root:

```bash
docker build -t prism/prismd:dev -f Dockerfile .
```

`scripts/run-kind.sh` runs this build for you (and generates an equivalent
fallback Dockerfile only if the repo has none). The image is distroless (no
shell), which is why the DaemonSet defines **no** exec readiness probe — see the
note in `daemonset.yaml`.

## 2. Load the image onto the node(s)

The image must exist on each node's container runtime, since it is not pulled.

- **kind:**     `kind load docker-image prism/prismd:dev --name <cluster>`
- **minikube:** `minikube image load prism/prismd:dev`
- **real cluster:** push to a registry your nodes can reach and update the image
  in `kustomization.yaml` (`images:` → `newName`/`newTag`) or via
  `kustomize edit set image prism/prismd=<registry>/prismd:<tag>`.

## 3. Apply the manifests

```bash
kubectl apply -k deploy/
```

This creates the `prism-system` namespace, the `prismd` ServiceAccount +
ClusterRole + binding (read-only on Pods, cluster-wide), and the DaemonSet.

## 4. Verify the rollout

```bash
kubectl -n prism-system rollout status daemonset/prismd
kubectl -n prism-system get pods -o wide
kubectl -n prism-system logs -l app.kubernetes.io/component=daemon
```

You should see lines like:

```
prismd: identity sink ready: kind=bpf      # or kind=sim on older kernels
prismd: bus keyer: cgroup (root=/sys/fs/cgroup)
prismd: node-scoped: <node-name>
prismd: starting pod informer (sim=false)
```

Create a workload and watch the identity propagate:

```bash
kubectl run demo --image=registry.k8s.io/pause:3.9
kubectl -n prism-system logs -l app.kubernetes.io/component=daemon -f
```

## 5. Verify the pinned BPF map on a node (real path only)

The map only exists on a 6.12+ host with bpffs + CAP_BPF. On such a **node**
(SSH in, or `kubectl debug node/<name>` / a privileged debug Pod), run
[`bpftool`](https://github.com/libbpf/bpftool):

```bash
# Is the map present, and what shape?
bpftool map show name prism_identity
# key: u64 (cgroup id / workload key), value: struct prism_identity (24 bytes)

# Dump live entries (one per observed workload on this node):
bpftool map dump pinned /sys/fs/bpf/prism/identity

# The pin path on disk:
ls -l /sys/fs/bpf/prism/
```

On an older kernel the map is absent (prismd used the sim sink); the daemon log
states which sink it chose.

## Configuration knobs (DaemonSet args)

`daemonset.yaml` runs prismd with `["-keyer=cgroup", "-node=$(NODE_NAME)"]` and
`NODE_NAME` from the downward API (`spec.nodeName`). Other prismd flags
(`-bpf`, `-cgroup-root`, `-cgroup-driver`, `-kubeconfig`, `-sim`) keep their
defaults; add them to `args:` if you need to override them. If your kubelet uses
the cgroupfs driver instead of systemd, add `-cgroup-driver=cgroupfs`.

## Monitoring

prismd serves a Prometheus exposition endpoint on container port **9464**
(named `metrics`) at **`/metrics`**, alongside **`/healthz`** (process is up) and
**`/readyz`** (informer cache synced — see below). The DaemonSet Pod template
also carries `prometheus.io/scrape` annotations.

### How to scrape

There are two independent ways to get prismd's metrics into Prometheus; pick the
one that matches your setup. `deploy/monitoring.yaml` is **optional** and is
**not** part of `kustomization.yaml` — apply it explicitly if you want it.

- **Annotation-based (plain Prometheus).** Already works with **no extra
  objects**. The Pod template sets `prometheus.io/scrape: "true"`,
  `prometheus.io/port: "9464"`, `prometheus.io/path: "/metrics"`, so a
  Prometheus using the common Pod service-discovery + relabel rules scrapes
  every node's prismd Pod automatically.

- **Prometheus Operator (PodMonitor).** If you run the
  [Prometheus Operator](https://github.com/prometheus-operator/prometheus-operator),
  apply `deploy/monitoring.yaml`:

  ```bash
  kubectl apply -f deploy/monitoring.yaml
  ```

  It creates a headless Service `prismd-metrics` (a stable, named endpoint for
  the per-node Pods) and a **PodMonitor** (`monitoring.coreos.com/v1`) that
  scrapes the prismd Pods directly on port `metrics`, path `/metrics`, every
  `30s`, selecting `app.kubernetes.io/name=prism` in `prism-system`. A
  PodMonitor (not a ServiceMonitor) is the right fit because prismd is a
  DaemonSet: each node's Pod is scraped on its own port without funnelling
  through a single Service VIP.

  > The `PodMonitor` kind is a CRD that exists **only if the Prometheus
  > Operator is installed**. Without the Operator, `kubectl apply` fails with
  > `no matches for kind "PodMonitor"`; in that case use the annotation-based
  > path above, or apply only the `Service` from that file.

### The `prism_*` metrics

prismd exports seven Prism-specific series (plus the usual Go/process
collectors):

| Metric | Type | Meaning |
| --- | --- | --- |
| `prism_pods_processed_total` | counter (label `event`=`add`/`update`/`delete`) | Pod events propagated to the identity bus, by event type. |
| `prism_identities_allocated_total` | counter | Numeric identities minted (each new security-relevant label set). |
| `prism_alloc_failures_total` | counter | Identity allocation failures (e.g. the 24-bit identity space is exhausted). |
| `prism_sink_errors_total` | counter (label `op`=`upsert`/`delete`) | Sink (BPF map / sim) write failures, by operation. |
| `prism_handler_panics_total` | counter | Panics recovered in the pod-event handler (the daemon survived). |
| `prism_live_identities` | gauge | Distinct identities currently live in the allocator. |
| `prism_propagation_seconds` | histogram | Per-pod control-plane propagation time, handler entry to sink write (buckets ~100ns .. ~26ms). |

`prism_identities_allocated_total` minus deletions tracks roughly with
`prism_live_identities`; replicas of the same workload share one identity (the
allocator dedups identical label sets), so these counts reflect distinct
workloads, not Pod counts.

### Readiness

`/readyz` gates readiness on the **client-go informer cache being synced**: it
reports ready only after the Pod informer has primed its cache
(`WaitForCacheSync`), i.e. once prismd is actually able to propagate identities.
The DaemonSet wires `/readyz` as the `readinessProbe` and `/healthz` (process
liveness) as the `livenessProbe` — both over the `metrics` port, so no shell is
needed in the distroless image.

## Local end-to-end demo (kind)

`scripts/run-kind.sh` automates everything above on a kind cluster: create
cluster → build image → `kind load` → `kubectl apply -k deploy/` → wait rollout
→ create test Pods → tail prismd logs. See the script header for env overrides
(`CLUSTER_NAME`, `IMAGE`, …).

```bash
./scripts/run-kind.sh
# teardown:
kind delete cluster --name prism
```

> kind/minikube node images usually predate 6.12, so prismd uses the sim sink
> there; the identity-propagation logs still demonstrate the bus. Use a 6.12+
> host/cluster to exercise the real pinned-map path and `bpftool` verification.

## Uninstall

```bash
kubectl delete -k deploy/
```
