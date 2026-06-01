# Build the standalone userspace microbenchmark (NOT a BPF program).
#
# This runs on any host, including the 5.15 WSL2 box used for development where
# sched_ext (needs 6.12+) and privileged BPF are unavailable. It measures the
# two per-scheduling-decision identity-resolution hot paths in isolation:
#   PRISM    = single u64->struct hash-map lookup (emulating the pinned BPF map)
#   BASELINE = scx_layered-style cgroup-path re-classification
#
# Exact build command:
#   clang -O2 -o /tmp/prism_ubench microbench.c
# Run (iteration count optional, default 1e6):
#   /tmp/prism_ubench 1000000

CC      ?= clang
CFLAGS  ?= -O2 -Wall -Wextra
OUT     ?= /tmp/prism_ubench

.PHONY: all run clean

all: $(OUT)

$(OUT): microbench.c
	$(CC) $(CFLAGS) -o $(OUT) microbench.c

run: $(OUT)
	$(OUT) 1000000

clean:
	rm -f $(OUT)
