// SPDX-License-Identifier: Apache-2.0

package classify

import (
	"testing"

	"github.com/ccyrene/prism/pkg/identity"
)

func TestClassifyDefaultRules(t *testing.T) {
	m := NewMatcher(DefaultRules())

	cases := []struct {
		name      string
		path      string
		wantClass string
		wantMatch bool
	}{
		{
			name:      "burstable pod",
			path:      "/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234_5678.slice/cri-containerd-abcdef.scope",
			wantClass: "pod-burstable",
			wantMatch: true,
		},
		{
			name:      "besteffort pod",
			path:      "/sys/fs/cgroup/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podaaaa.slice/cri-containerd-deadbeef.scope",
			wantClass: "pod-besteffort",
			wantMatch: true,
		},
		{
			name:      "ingress wins over generic pod (order/first-match)",
			path:      "/sys/fs/cgroup/kubepods.slice/kubepods-pod99.slice/cri-containerd-ingress-nginx-controller.scope",
			wantClass: "ingress-nginx",
			wantMatch: true,
		},
		{
			name:      "system slice",
			path:      "/sys/fs/cgroup/system.slice/containerd.service",
			wantClass: "system",
			wantMatch: true,
		},
		{
			name:      "unmanaged path",
			path:      "/sys/fs/cgroup/something/totally/unrelated",
			wantClass: "unmanaged",
			wantMatch: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, id, matched := m.Classify(tc.path)
			if matched != tc.wantMatch {
				t.Fatalf("matched=%v, want %v (class=%q id=%v)", matched, tc.wantMatch, class, id)
			}
			if class != tc.wantClass {
				t.Fatalf("class=%q, want %q", class, tc.wantClass)
			}
			if !matched && id != identity.IDUnmanaged {
				t.Fatalf("unmatched id=%v, want IDUnmanaged(%v)", id, identity.IDUnmanaged)
			}
		})
	}
}

// TestClassifyFirstMatchWins pins the ordered, first-match-wins semantics that
// mirror scx_layered's top-to-bottom layer walk.
func TestClassifyFirstMatchWins(t *testing.T) {
	rules := []Rule{
		{Class: "specific", ID: identity.MinDynamicID, Preds: []Predicate{
			{Kind: PredContains, Value: "kubepods"},
			{Kind: PredContains, Value: "special"},
		}},
		{Class: "general", ID: identity.MinDynamicID + 1, Preds: []Predicate{
			{Kind: PredContains, Value: "kubepods"},
		}},
	}
	m := NewMatcher(rules)

	class, id, matched := m.Classify("/kubepods.slice/special-pod.scope")
	if !matched || class != "specific" || id != identity.MinDynamicID {
		t.Fatalf("first-match-wins broken: class=%q id=%v matched=%v", class, id, matched)
	}

	class, _, matched = m.Classify("/kubepods.slice/ordinary-pod.scope")
	if !matched || class != "general" {
		t.Fatalf("fallthrough to general broken: class=%q matched=%v", class, matched)
	}
}

// TestEmptyRuleNeverMatches guards against a rule with no predicates matching
// everything (which would make classification meaningless).
func TestEmptyRuleNeverMatches(t *testing.T) {
	m := NewMatcher([]Rule{{Class: "bad", ID: identity.MinDynamicID, Preds: nil}})
	if class, _, matched := m.Classify("/anything"); matched {
		t.Fatalf("empty rule matched (class=%q); it must not", class)
	}
}
