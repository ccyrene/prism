#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# run-kind.sh — end-to-end local demo of the Prism workload-identity bus on kind.
#
# Pipeline:
#   1. Create (or reuse) a kind cluster.
#   2. docker build the prismd image.
#   3. kind load the image onto the cluster's node(s) (no registry needed).
#   4. kubectl apply -k deploy/  (namespace + RBAC + DaemonSet).
#   5. Wait for the DaemonSet rollout.
#   6. Create a couple of test Pods.
#   7. Tail prismd logs to show identity propagation.
#
# Idempotent: re-running reuses the cluster, rebuilds/reloads the image, and
# re-applies the manifests. Safe to run repeatedly.
#
# NOTE: kind nodes run an older/standard kernel image, so the kernel BPF sink
# is typically unavailable inside kind — prismd then logs the reason and falls
# back to its userspace sim sink. The identity-propagation logs still appear,
# which is what this demo verifies. Real pinned-map verification with bpftool
# requires a 6.12+ host (see deploy/README.md).
set -euo pipefail

# --- config (override via env) ------------------------------------------------
CLUSTER_NAME="${CLUSTER_NAME:-prism}"
IMAGE="${IMAGE:-prism/prismd:dev}"
NAMESPACE="${NAMESPACE:-prism-system}"
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-120s}"

# Resolve repo root from this script's location so the script runs from anywhere.
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." >/dev/null 2>&1 && pwd)"
DEPLOY_DIR="${REPO_ROOT}/deploy"
DOCKERFILE="${REPO_ROOT}/Dockerfile"

# --- helpers ------------------------------------------------------------------
log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

need() {
  # need <tool> <install-hint>
  command -v "$1" >/dev/null 2>&1 || die "'$1' not found in PATH. $2"
}

# --- preflight: required tools ------------------------------------------------
need docker  "Install Docker and ensure the daemon is reachable (docker info)."
need kind    "Install kind: https://kind.sigs.k8s.io/docs/user/quick-start/#installation"
need kubectl "Install kubectl: https://kubernetes.io/docs/tasks/tools/"

# Docker daemon must be reachable (on WSL it often isn't wired in).
if ! docker info >/dev/null 2>&1; then
  die "Docker daemon not reachable. Start Docker Desktop / dockerd (on WSL ensure integration is enabled), then re-run."
fi

# --- 0. ensure a Dockerfile exists --------------------------------------------
# The deploy manifests reference image '${IMAGE}', which we build here. The repo
# normally ships a Dockerfile (distroless static binary) — we use it as-is. Only
# if none exists do we generate a minimal, correct fallback (also static, so it
# matches the distroless ENTRYPOINT /prismd). The DaemonSet defines no exec
# readiness probe, so a shell in the image is not required.
if [[ ! -f "${DOCKERFILE}" ]]; then
  log "No Dockerfile found; generating fallback ${DOCKERFILE}"
  cat > "${DOCKERFILE}" <<'DOCKERFILE_EOF'
# Fallback Dockerfile: fully static prismd on distroless (no shell needed).
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-mod=mod go build -trimpath -ldflags='-s -w' \
        -o /prismd ./cmd/prismd

FROM gcr.io/distroless/static:latest
COPY --from=build /prismd /prismd
ENTRYPOINT ["/prismd"]
DOCKERFILE_EOF
else
  log "Using existing ${DOCKERFILE}"
fi

# --- 1. create or reuse the kind cluster --------------------------------------
if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  log "kind cluster '${CLUSTER_NAME}' already exists; reusing it"
else
  log "Creating kind cluster '${CLUSTER_NAME}'"
  kind create cluster --name "${CLUSTER_NAME}" --wait 60s
fi
# Point kubectl at this cluster for the rest of the script.
kubectl config use-context "kind-${CLUSTER_NAME}" >/dev/null

# --- 2. build the image -------------------------------------------------------
log "Building image ${IMAGE}"
docker build -t "${IMAGE}" -f "${DOCKERFILE}" "${REPO_ROOT}"

# --- 3. load the image onto the cluster nodes ---------------------------------
log "Loading ${IMAGE} into kind cluster '${CLUSTER_NAME}'"
kind load docker-image "${IMAGE}" --name "${CLUSTER_NAME}"

# --- 4. apply the deployment --------------------------------------------------
log "Applying manifests from ${DEPLOY_DIR}"
kubectl apply -k "${DEPLOY_DIR}"

# --- 5. wait for rollout ------------------------------------------------------
log "Waiting for prismd DaemonSet rollout (timeout ${ROLLOUT_TIMEOUT})"
if ! kubectl -n "${NAMESPACE}" rollout status daemonset/prismd --timeout="${ROLLOUT_TIMEOUT}"; then
  warn "Rollout did not complete in time. Recent prismd events/logs:"
  kubectl -n "${NAMESPACE}" describe daemonset/prismd | tail -n 30 || true
  kubectl -n "${NAMESPACE}" logs -l app.kubernetes.io/component=daemon --tail=50 || true
  die "prismd DaemonSet failed to become ready."
fi

# --- 6. create a couple of test pods ------------------------------------------
log "Creating test workloads (default namespace)"
# 'apply' makes this idempotent across re-runs.
kubectl apply -f - <<'PODS_EOF'
apiVersion: v1
kind: Pod
metadata:
  name: prism-test-a
  labels: { app: prism-test, workload: alpha }
spec:
  containers:
    - name: pause
      image: registry.k8s.io/pause:3.9
---
apiVersion: v1
kind: Pod
metadata:
  name: prism-test-b
  labels: { app: prism-test, workload: beta }
spec:
  containers:
    - name: pause
      image: registry.k8s.io/pause:3.9
PODS_EOF

log "Waiting for test pods to be Ready"
kubectl wait --for=condition=Ready pod/prism-test-a pod/prism-test-b --timeout=60s || \
  warn "Test pods not Ready yet; prismd should still have observed their add events."

# Give the prismd informer a moment to observe the new pod events.
log "Letting prismd observe the new pods (5s)"
end=$(( $(date +%s) + 5 ))
while [ "$(date +%s)" -lt "${end}" ]; do :; done

# --- 7. show identity propagation in the logs ---------------------------------
log "prismd logs (identity propagation):"
echo "------------------------------------------------------------------"
kubectl -n "${NAMESPACE}" logs -l app.kubernetes.io/component=daemon --prefix --tail=80 || true
echo "------------------------------------------------------------------"

cat <<EOF

Done.

Inspect further:
  kubectl -n ${NAMESPACE} get pods -o wide
  kubectl -n ${NAMESPACE} logs -l app.kubernetes.io/component=daemon -f

On a real 6.12+ host the pinned BPF map is verifiable on a node with:
  bpftool map show name prism_identity
  bpftool map dump  pinned /sys/fs/bpf/prism/identity

Tear everything down:
  kind delete cluster --name ${CLUSTER_NAME}
EOF
