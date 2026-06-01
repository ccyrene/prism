// SPDX-License-Identifier: Apache-2.0

package key

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func mkpod(uid string, qos corev1.PodQOSClass) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID(uid)},
		Status:     corev1.PodStatus{QOSClass: qos},
	}
}

func TestUIDKeyerStable(t *testing.T) {
	p := mkpod("abc-123", corev1.PodQOSBurstable)
	a, ok, err := (UIDKeyer{}).Key(p)
	if !ok || err != nil {
		t.Fatal("uid keyer must always succeed")
	}
	b, _, _ := (UIDKeyer{}).Key(p)
	if a != b {
		t.Fatal("uid key not stable")
	}
	if a != UIDKey("abc-123") {
		t.Fatal("uid keyer disagrees with UIDKey")
	}
}

func TestSystemdPath(t *testing.T) {
	k := CgroupKeyer{Root: "/sys/fs/cgroup", Driver: DriverSystemd}
	got := k.PodCgroupPath(mkpod("pod-abc-def", corev1.PodQOSBurstable))
	want := "/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podpod_abc_def.slice"
	if got != want {
		t.Fatalf("systemd burstable path:\n got %s\nwant %s", got, want)
	}
	g := k.PodCgroupPath(mkpod("g-uid", corev1.PodQOSGuaranteed))
	if g != "/sys/fs/cgroup/kubepods.slice/kubepods-podg_uid.slice" {
		t.Fatalf("systemd guaranteed path: %s", g)
	}
}

func TestCgroupfsPath(t *testing.T) {
	k := CgroupKeyer{Root: "/sys/fs/cgroup", Driver: DriverCgroupfs}
	got := k.PodCgroupPath(mkpod("abc-def", corev1.PodQOSBestEffort))
	if got != "/sys/fs/cgroup/kubepods/besteffort/podabc-def" {
		t.Fatalf("cgroupfs besteffort path: %s", got)
	}
}

// TestCgroupKeyerInode builds a fake kubepods tree and proves the key equals the
// real directory inode — i.e. exactly what bpf_get_current_ancestor_cgroup_id
// returns on a live kernel.
func TestCgroupKeyerInode(t *testing.T) {
	root := t.TempDir()
	pod := mkpod("11111111-2222-3333-4444-555555555555", corev1.PodQOSBurstable)
	k := CgroupKeyer{Root: root, Driver: DriverSystemd}

	dir := k.PodCgroupPath(pod)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		t.Fatal(err)
	}

	got, ok, err := k.Key(pod)
	if err != nil || !ok {
		t.Fatalf("key: ok=%v err=%v", ok, err)
	}
	if uint64(got) != st.Ino {
		t.Fatalf("key %d != dir inode %d", got, st.Ino)
	}

	// A pod whose cgroup doesn't exist yet -> ok=false, no error.
	_, ok, err = k.Key(mkpod("absent", corev1.PodQOSBurstable))
	if ok || err != nil {
		t.Fatalf("absent pod: ok=%v err=%v (want false,nil)", ok, err)
	}
	_ = filepath.Base(dir)
}
