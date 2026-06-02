// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"testing"

	"github.com/ccyrene/prism/pkg/abi"
)

func TestDeriveSchedPolicy_Classes(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want abi.SchedClass
	}{
		{"unlabeled", map[string]string{"app": "web"}, abi.SchedClassUnset},
		{"critical", map[string]string{LatencyClassLabel: "critical"}, abi.SchedClassCritical},
		{"latency-critical alias", map[string]string{LatencyClassLabel: "latency-critical"}, abi.SchedClassCritical},
		{"mixed case + spaces", map[string]string{LatencyClassLabel: "  Critical "}, abi.SchedClassCritical},
		{"normal", map[string]string{LatencyClassLabel: "normal"}, abi.SchedClassNormal},
		{"batch", map[string]string{LatencyClassLabel: "batch"}, abi.SchedClassBatch},
		{"garbage -> unset", map[string]string{LatencyClassLabel: "turbo"}, abi.SchedClassUnset},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveSchedPolicy(tc.in).Class; got != tc.want {
				t.Errorf("class = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDeriveSchedPolicy_Weight(t *testing.T) {
	cases := []struct {
		in   string
		want uint8
	}{
		{"", 0}, {"0", 0}, {"-5", 0}, {"abc", 0},
		{"1", 1}, {"64", 64}, {"127", 127}, {"999", abi.SchedWeightMax},
	}
	for _, tc := range cases {
		labels := map[string]string{WeightLabel: tc.in}
		if got := DeriveSchedPolicy(labels).Weight; got != tc.want {
			t.Errorf("weight %q -> %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestDeriveSchedPolicy_Encode(t *testing.T) {
	p := DeriveSchedPolicy(map[string]string{
		LatencyClassLabel: "critical",
		WeightLabel:       "100",
	})
	v := abi.PrismIdentity{Flags: p.Encode()}
	if v.SchedClass() != abi.SchedClassCritical || v.SchedWeight() != 100 {
		t.Errorf("encoded policy = class %v weight %d, want critical/100", v.SchedClass(), v.SchedWeight())
	}
	if DeriveSchedPolicy(nil).IsZero() != true {
		t.Errorf("nil labels should derive the zero policy")
	}
}

// TestPolicyLabelsDoNotChangeIdentity is the decoupling invariant: a prism.io/
// policy label must be excluded from the identity canonical form, so two
// workloads identical except for their policy label share ONE identity (and a
// replica can be retuned without renumbering).
func TestPolicyLabelsDoNotChangeIdentity(t *testing.T) {
	base := map[string]string{"app": "web", "tier": "frontend"}
	withCritical := map[string]string{"app": "web", "tier": "frontend", LatencyClassLabel: "critical"}
	withBatch := map[string]string{"app": "web", "tier": "frontend", LatencyClassLabel: "batch", WeightLabel: "120"}

	c0 := Canonical(base)
	if c1 := Canonical(withCritical); c1 != c0 {
		t.Errorf("policy label changed canonical: %q != %q", c1, c0)
	}
	if c2 := Canonical(withBatch); c2 != c0 {
		t.Errorf("policy label+weight changed canonical: %q != %q", c2, c0)
	}

	// Same through the spoof-resistant WorkloadCanonical too.
	w0 := WorkloadCanonical(base, "ns", "sa")
	if w1 := WorkloadCanonical(withCritical, "ns", "sa"); w1 != w0 {
		t.Errorf("policy label changed WorkloadCanonical: %q != %q", w1, w0)
	}
}
