// SPDX-License-Identifier: Apache-2.0

// Package key derives the Prism bus key for a Pod — the u64 the BPF side uses to
// look identity up. There are two strategies:
//
//   - UIDKeyer: a stable FNV-1a hash of the Pod UID. Used in simulation, in
//     benchmarks, and on a node where the cgroup tree isn't readable. It never
//     fails but it does NOT match what a BPF program sees in-kernel.
//
//   - CgroupKeyer: the inode number of the Pod's cgroup-v2 directory. On a real
//     kernel this equals bpf_get_current_ancestor_cgroup_id() at the pod level,
//     i.e. exactly the key a BPF consumer computes from a running task. This is
//     what makes the userspace daemon and the in-kernel datapath agree.
//
// (A task's own bpf_get_current_cgroup_id() returns its *container* cgroup, a
// descendant of the pod cgroup; a BPF consumer that keys on the pod identity
// must therefore use the pod-ancestor cgroup id. libprism.bpf.h documents the
// helper variant for this. We key on the pod-level cgroup so one identity covers
// all containers of a Pod.)
package key

import (
	"fmt"
	"strings"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ccyrene/prism/pkg/abi"
)

// Keyer maps a Pod to its bus key.
type Keyer interface {
	// Key returns the workload key and whether it could be derived. err is set
	// only for unexpected failures (a missing cgroup dir returns ok=false, nil).
	Key(pod *corev1.Pod) (abi.WorkloadKey, bool, error)
	Name() string
}

// UIDKey is the canonical UID-hash key (FNV-1a/64, no allocation hot path).
func UIDKey(uid types.UID) abi.WorkloadKey {
	const (
		offset64 = 1469598103934665603
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	s := string(uid)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}

// UIDKeyer keys on the Pod UID. Always succeeds.
type UIDKeyer struct{}

func (UIDKeyer) Name() string { return "uid" }
func (UIDKeyer) Key(pod *corev1.Pod) (abi.WorkloadKey, bool, error) {
	return UIDKey(pod.UID), true, nil
}

// Driver selects the kubelet cgroup driver layout.
type Driver int

const (
	DriverSystemd  Driver = iota // *.slice, dashes->underscores (default on modern distros)
	DriverCgroupfs               // plain dirs, dashes preserved
)

// ParseDriver maps "systemd"/"cgroupfs" to a Driver (systemd default).
func ParseDriver(s string) Driver {
	if strings.EqualFold(s, "cgroupfs") {
		return DriverCgroupfs
	}
	return DriverSystemd
}

// CgroupKeyer resolves the Pod's cgroup-v2 directory inode == in-kernel cgroup id.
type CgroupKeyer struct {
	Root   string // cgroup v2 mount, e.g. /sys/fs/cgroup
	Driver Driver
}

func (k CgroupKeyer) Name() string {
	if k.Driver == DriverCgroupfs {
		return "cgroup(cgroupfs)"
	}
	return "cgroup(systemd)"
}

// Key stats the pod cgroup directory and returns its inode as the bus key.
func (k CgroupKeyer) Key(pod *corev1.Pod) (abi.WorkloadKey, bool, error) {
	path := k.PodCgroupPath(pod)
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		if err == syscall.ENOENT {
			return 0, false, nil // cgroup not present (e.g. pod not yet started on this node)
		}
		return 0, false, fmt.Errorf("stat %s: %w", path, err)
	}
	return abi.WorkloadKey(st.Ino), true, nil
}

// qosSegment returns the QoS sub-slice token ("" for Guaranteed, which lives
// directly under kubepods).
func qosSegment(pod *corev1.Pod) string {
	switch pod.Status.QOSClass {
	case corev1.PodQOSGuaranteed:
		return ""
	case corev1.PodQOSBestEffort:
		return "besteffort"
	case corev1.PodQOSBurstable:
		return "burstable"
	default:
		// Status not populated (e.g. test fixtures): assume burstable, the
		// common case; callers wanting exactness should set Status.QOSClass.
		return "burstable"
	}
}

// PodCgroupPath builds the absolute pod-level cgroup directory for the active
// driver. It mirrors kubelet's layout exactly so the inode matches in-kernel.
func (k CgroupKeyer) PodCgroupPath(pod *corev1.Pod) string {
	root := k.Root
	if root == "" {
		root = "/sys/fs/cgroup"
	}
	qos := qosSegment(pod)
	uid := string(pod.UID)

	if k.Driver == DriverCgroupfs {
		// /kubepods/<qos>/pod<uid>   (guaranteed: /kubepods/pod<uid>)
		if qos == "" {
			return fmt.Sprintf("%s/kubepods/pod%s", root, uid)
		}
		return fmt.Sprintf("%s/kubepods/%s/pod%s", root, qos, uid)
	}

	// systemd driver: dashes in UID become underscores; segments are *.slice.
	u := strings.ReplaceAll(uid, "-", "_")
	if qos == "" {
		// /kubepods.slice/kubepods-pod<uid>.slice
		return fmt.Sprintf("%s/kubepods.slice/kubepods-pod%s.slice", root, u)
	}
	// /kubepods.slice/kubepods-<qos>.slice/kubepods-<qos>-pod<uid>.slice
	return fmt.Sprintf("%s/kubepods.slice/kubepods-%s.slice/kubepods-%s-pod%s.slice",
		root, qos, qos, u)
}
