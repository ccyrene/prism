# Toolchain image for building + loading the Prism sched_ext scheduler against
# the HOST's 6.17 kernel. Runs privileged sharing the host kernel; this image
# only supplies the userspace build toolchain that the host lacks (clang/libbpf/
# go) plus the latency workloads. sched_ext itself is the host kernel's.
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# Core BPF/build toolchain + latency workloads + python plotting.
# bpftool: install the version-matched linux-tools so the /usr/sbin/bpftool
# wrapper (keyed on uname -r == host 6.17.0-29) resolves; we also call the real
# binary by absolute path. rt-tests provides hackbench; stress-ng for contention.
RUN apt-get update && apt-get install -y --no-install-recommends \
        clang llvm lld \
        libbpf-dev libelf-dev zlib1g-dev pkg-config \
        make gcc git curl ca-certificates \
        python3 python3-matplotlib python3-numpy \
        stress-ng rt-tests \
        linux-tools-common linux-tools-6.17.0-29-generic \
        bc jq \
    && rm -rf /var/lib/apt/lists/*

# Go 1.24 (matches go.mod 'go 1.24.0'); apt's golang-go is 1.22.
RUN curl -fsSL https://go.dev/dl/go1.24.3.linux-amd64.tar.gz -o /tmp/go.tgz \
    && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
ENV PATH=/usr/local/go/bin:/usr/local/bin:$PATH

# schbench — Facebook's scheduler wakeup-latency benchmark (the gold-standard
# tail-latency signal run-sched-eval.sh prefers). Built from source.
RUN git clone --depth 1 https://github.com/masoncl/schbench /tmp/schbench \
    && make -C /tmp/schbench \
    && cp /tmp/schbench/schbench /usr/local/bin/schbench \
    && rm -rf /tmp/schbench

WORKDIR /work
CMD ["sleep", "infinity"]
