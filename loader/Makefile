# loader/Makefile — build the scx_prism userspace loader.
#
# This builds ONLY on a Linux 6.12+ host that has:
#   * libbpf-dev   >= 1.0   (provides <bpf/libbpf.h>, <bpf/bpf.h>, -lbpf)
#   * clang or gcc
#   * libelf + zlib (-lelf -lz, libbpf's link deps)
#   * a compiled bpf/scx_prism.bpf.o (produced by scripts/provision-schedext-vm.sh)
#
# It will NOT build on the 5.15 WSL2 dev box: libbpf-dev is absent there and the
# sched_ext types only exist in a 6.12 BTF. That is expected and documented in
# README.md (with the exact `clang -fsyntax-only` error you get here).
#
# Two build modes (see scx_prism_loader.c header for the difference):
#   make            -> generic object API (default; needs only the .bpf.o)
#   make skel       -> skeleton API (regenerates scx_prism.skel.h with bpftool,
#                      then compiles with -DUSE_SKEL)
#
# Useful overrides:
#   make CC=gcc                 # use gcc instead of clang
#   make BPF_OBJ=../bpf/scx_prism.bpf.o
#   make BPFTOOL=/usr/lib/linux-tools-6.14.0/bpftool

CC       ?= clang
CFLAGS   ?= -O2 -g -Wall -Wextra
# libbpf is usually under /usr/include/bpf with -lbpf; pkg-config picks up any
# non-default prefix (e.g. a locally built libbpf). Fall back to bare -lbpf.
PKG_CFG  := $(shell pkg-config --exists libbpf 2>/dev/null && echo yes)
ifeq ($(PKG_CFG),yes)
  LIBBPF_CFLAGS := $(shell pkg-config --cflags libbpf)
  LIBBPF_LIBS   := $(shell pkg-config --libs libbpf)
else
  LIBBPF_CFLAGS :=
  LIBBPF_LIBS   := -lbpf
endif
# libbpf needs libelf + zlib at link time.
LDLIBS   ?= $(LIBBPF_LIBS) -lelf -lz

BIN      := scx_prism_loader
SRC      := scx_prism_loader.c
BPF_OBJ  ?= ../bpf/scx_prism.bpf.o
SKEL     := scx_prism.skel.h
BPFTOOL  ?= bpftool

.PHONY: all generic skel clean syntax syntax-expected

# Default: the generic (no-skeleton) build. Only needs the compiled .bpf.o at run
# time, so the build does not depend on bpftool.
all: generic

generic: $(SRC)
	$(CC) $(CFLAGS) $(LIBBPF_CFLAGS) $(SRC) $(LDLIBS) -o $(BIN)
	@echo "built $(BIN) (generic object API). Run: sudo ./$(BIN) $(BPF_OBJ)"

# Skeleton build: regenerate the typed skeleton from the compiled BPF object,
# then compile with -DUSE_SKEL so the loader embeds and uses it.
$(SKEL): $(BPF_OBJ)
	$(BPFTOOL) gen skeleton $(BPF_OBJ) name scx_prism > $(SKEL)
	@echo "generated $(SKEL) from $(BPF_OBJ)"

skel: $(SKEL) $(SRC)
	$(CC) $(CFLAGS) $(LIBBPF_CFLAGS) -DUSE_SKEL -I. $(SRC) $(LDLIBS) -o $(BIN)
	@echo "built $(BIN) (skeleton API). Run: sudo ./$(BIN)"

# Best-effort syntax check. On a host WITHOUT libbpf-dev (e.g. the 5.15 dev box)
# this FAILS at `#include <bpf/libbpf.h>` — that is the expected dependency, not
# a code bug. `make syntax-expected` documents/asserts that exact failure mode.
syntax:
	$(CC) -fsyntax-only $(LIBBPF_CFLAGS) $(SRC)

syntax-expected:
	@echo "Expect: fatal error: 'bpf/libbpf.h' file not found  (libbpf-dev missing)"
	-@$(CC) -fsyntax-only $(SRC) 2>&1 | head -3 || true

clean:
	rm -f $(BIN) $(SKEL)
