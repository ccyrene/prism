# Prism Workload Identity Bus — prismd daemon image.
#
# Multi-stage build:
#   1. builder  — golang:1.24, compiles a fully static (CGO_ENABLED=0) prismd.
#   2. final    — gcr.io/distroless/static, ships only the stripped binary.
#
# The resulting image runs the userspace (simulation) sink out of the box on any
# node. The REAL kernel BPF sink (a pinned BPF map under /sys/fs/bpf/prism/) is
# an opt-in fast path that the DaemonSet enables — it is NOT baked into the image
# because it depends on host privileges and mounts, not on image contents:
#
#   * privileged: true  (or capabilities CAP_BPF + CAP_SYS_ADMIN) — to create and
#     pin the prism_identity map.
#   * hostPath mount of /sys/fs/bpf (bpffs) at /sys/fs/bpf, so the pinned map at
#     /sys/fs/bpf/prism/identity survives and is shared with kernel consumers.
#   * hostPath mount of /sys/fs/cgroup (read-only is fine) for the default
#     "cgroup" keyer, which stat()s pod cgroup inodes for real-kernel parity.
#   * NODE_NAME via the downward API (fieldRef: spec.nodeName) for node scoping.
#   * RBAC to list/watch Pods.
#
# Without those, prismd logs the reason and falls back to the simulation sink,
# so the container still comes up healthy — see cmd/prismd/main.go.
#
# Build:
#   docker build -t prism/prismd:dev .
# Pin the Go toolchain / base digests in CI for reproducible images.

# ---- Stage 1: builder ------------------------------------------------------
FROM golang:1.24 AS builder

# Static, reproducible-ish build. CGO off so the binary has no libc dependency
# and runs on distroless/static (and scratch). Trimpath drops local paths.
ENV CGO_ENABLED=0 \
    GOFLAGS=-mod=mod \
    GO111MODULE=on

WORKDIR /src

# Prime the module cache first so dependency downloads are cached across source
# edits (this layer only busts when go.mod / go.sum change).
COPY go.mod go.sum ./
RUN go mod download

# Now the rest of the source.
COPY . .

# Build the daemon. -s -w strips the symbol table and DWARF; -trimpath removes
# absolute build paths. Target linux/amd64 by default; override via build args.
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /prismd ./cmd/prismd \
 && /prismd -help 2>&1 | head -1 || true

# ---- Stage 2: final --------------------------------------------------------
# distroless/static: no shell, no package manager, minimal attack surface. The
# binary is fully static so this is all it needs. ":nonroot" would run as uid
# 65532; we stay root because the BPF fast path needs caps — the DaemonSet drops
# privileges as appropriate per environment.
FROM gcr.io/distroless/static:latest

LABEL org.opencontainers.image.title="prismd" \
      org.opencontainers.image.description="Prism Workload Identity Bus sync daemon" \
      org.opencontainers.image.source="https://github.com/prism-bus/prism"

COPY --from=builder /prismd /prismd

# Default to the simulation sink path so the bare image is runnable anywhere for
# a smoke test; the DaemonSet overrides args/env for the real cluster path.
ENTRYPOINT ["/prismd"]
