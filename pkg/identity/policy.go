// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"strconv"
	"strings"

	"github.com/prism-bus/prism/pkg/abi"
)

// Policy label keys. An operator sets scheduling policy CENTRALLY on the pod;
// prismd reads it here and publishes it on the bus (the scheduling-policy
// sub-fields of the identity value, see abi.EncodeSchedPolicy), so any scx
// scheduler consumes the same descriptor natively. These keys live under the
// prism.io/ prefix, which labels.go excludes from the identity canonical form —
// so policy never changes identity (replicas stay one identity; retuning a
// pod's class does not renumber it).
const (
	// LatencyClassLabel selects the latency class: "critical" | "normal" |
	// "batch" (case-insensitive; "latency-critical" is accepted for critical).
	LatencyClassLabel = "prism.io/latency-class"
	// WeightLabel is an optional 1..127 relative weight refining the class.
	WeightLabel = "prism.io/weight"
)

// SchedPolicy is the per-workload scheduling policy derived from a pod's labels.
// The zero value (SchedClassUnset, weight 0) means "no policy — let the
// scheduler use its own heuristic", so policy is strictly opt-in.
type SchedPolicy struct {
	Class  abi.SchedClass
	Weight uint8 // 0 = unset, else 1..127
}

// DeriveSchedPolicy reads the prism.io/ policy labels off a pod's label set and
// returns the scheduling policy to publish on the bus. Missing, empty, or
// invalid values yield the zero policy, so an unlabeled pod is unaffected and a
// consumer sees PRISM_SCHED_CLASS_UNSET (its own heuristic governs).
func DeriveSchedPolicy(labels map[string]string) SchedPolicy {
	var p SchedPolicy
	switch strings.ToLower(strings.TrimSpace(labels[LatencyClassLabel])) {
	case "critical", "latency-critical":
		p.Class = abi.SchedClassCritical
	case "normal":
		p.Class = abi.SchedClassNormal
	case "batch":
		p.Class = abi.SchedClassBatch
	}
	if w, err := strconv.Atoi(strings.TrimSpace(labels[WeightLabel])); err == nil && w > 0 {
		if w > abi.SchedWeightMax {
			w = abi.SchedWeightMax
		}
		p.Weight = uint8(w)
	}
	return p
}

// Encode returns the Flags sub-field bits for this policy (facet bits zero), so
// the controller can OR it into the facet flags it already stamps.
func (p SchedPolicy) Encode() uint32 {
	return abi.EncodeSchedPolicy(p.Class, uint32(p.Weight))
}

// IsZero reports whether the policy carries no class and no weight (the opt-out
// default). Handy for tests and logging.
func (p SchedPolicy) IsZero() bool {
	return p.Class == abi.SchedClassUnset && p.Weight == 0
}
