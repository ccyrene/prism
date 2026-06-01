// SPDX-License-Identifier: Apache-2.0

// Package identity implements the Prism workload-identity model: a Cilium-style
// numeric identity derived from a canonical set of security-relevant labels,
// allocated from a 24-bit space by a deterministic min-available-integer
// allocator. The label-set -> identity mapping is the substrate of the bus.
package identity

import "fmt"

// NumericIdentity is a 24-bit identity stored in a u32 (matching the ABI).
type NumericIdentity uint32

// Identity space bounds (mirror bpf/prism_maps.bpf.h).
const (
	IdentityMask  NumericIdentity = 0x00FFFFFF
	MinDynamicID  NumericIdentity = 256
	MaxID         NumericIdentity = 0x00FFFFFF // 16,777,215
)

// Reserved identities — well-known IDs shared across subsystems (Cilium range).
const (
	IDUnknown       NumericIdentity = 0
	IDHost          NumericIdentity = 1
	IDWorld         NumericIdentity = 2
	IDUnmanaged     NumericIdentity = 3
	IDHealth        NumericIdentity = 4
	IDInit          NumericIdentity = 5
	IDRemoteNode    NumericIdentity = 6
	IDKubeAPIServer NumericIdentity = 7
	IDIngress       NumericIdentity = 8
)

var reservedNames = map[NumericIdentity]string{
	IDUnknown: "unknown", IDHost: "host", IDWorld: "world", IDUnmanaged: "unmanaged",
	IDHealth: "health", IDInit: "init", IDRemoteNode: "remote-node",
	IDKubeAPIServer: "kube-apiserver", IDIngress: "ingress",
}

// IsReserved reports whether id is in the reserved (< MinDynamicID) range.
func (id NumericIdentity) IsReserved() bool { return id < MinDynamicID }

// IsValid reports whether id fits the 24-bit space.
func (id NumericIdentity) IsValid() bool { return id <= MaxID }

func (id NumericIdentity) String() string {
	if n, ok := reservedNames[id]; ok {
		return fmt.Sprintf("%d(%s)", uint32(id), n)
	}
	return fmt.Sprintf("%d", uint32(id))
}
