// SPDX-License-Identifier: Apache-2.0

// Package classify implements the "state-of-practice" baseline that Prism is
// measured against: an ad-hoc, per-decision cgroup-path classifier in the style
// of sched_ext's scx_layered.
//
// Why this exists
// ---------------
// scx_layered (https://github.com/sched-ext/scx, tools/sched_ext/scx_layered)
// has no notion of a precomputed workload identity. Instead each "layer" carries
// an ordered list of *match rules* (its `matches` field, a disjunction of
// conjunctions of LayerMatch predicates such as CgroupPrefix, CommPrefix,
// NiceAbove, ...). On the hot path the BPF side matches a task by walking the
// configured layers in order and testing those predicates — and the cgroup-based
// predicates fundamentally require inspecting the task's cgroup *path string*
// segment by segment. The classification is therefore RE-DERIVED from the path
// on essentially every scheduling decision, rather than being an O(1) map read.
//
// This package is a faithful userspace re-implementation of that approach: rules
// are an ordered list, each rule is a conjunction of substring/prefix predicates
// over the '/'-separated cgroup-path segments, and Classify walks the path and
// tests the rules with first-match-wins semantics (exactly scx_layered's
// "first layer whose match succeeds" policy). It does the real per-call work —
// no caching, no memoization, no artificial delay — so a benchmark can honestly
// measure the gap against Prism's single map lookup.
package classify

import (
	"strings"

	"github.com/ccyrene/prism/pkg/identity"
)

// PredKind selects how a Predicate's Value is tested against a path segment.
type PredKind uint8

const (
	// PredContains matches if a segment contains Value as a substring.
	PredContains PredKind = iota
	// PredHasPrefix matches if a segment starts with Value.
	PredHasPrefix
	// PredHasSuffix matches if a segment ends with Value (e.g. ".scope", ".slice").
	PredHasSuffix
)

// Predicate is a single test applied to the cgroup-path segments. It mirrors a
// single scx_layered LayerMatch entry (e.g. CgroupPrefix/CgroupSuffix/substring).
type Predicate struct {
	Kind  PredKind
	Value string
}

// match reports whether this predicate is satisfied by ANY segment of the path.
// scx_layered's cgroup matches are likewise evaluated against the cgroup path,
// not a single fixed component, so we scan all segments.
func (p Predicate) match(segments []string) bool {
	switch p.Kind {
	case PredHasPrefix:
		for _, s := range segments {
			if strings.HasPrefix(s, p.Value) {
				return true
			}
		}
	case PredHasSuffix:
		for _, s := range segments {
			if strings.HasSuffix(s, p.Value) {
				return true
			}
		}
	default: // PredContains
		for _, s := range segments {
			if strings.Contains(s, p.Value) {
				return true
			}
		}
	}
	return false
}

// Rule is one "layer": a class label, the identity to assign, and an ordered
// conjunction of predicates that ALL must hold (AND) for the rule to fire. This
// is the analogue of a single scx_layered layer's `matches` conjunction.
type Rule struct {
	Class string
	ID    identity.NumericIdentity
	// Preds is ANDed together: every predicate must match for the rule to fire.
	Preds []Predicate
}

// Matcher holds an ordered rule list and classifies cgroup paths with
// first-match-wins semantics — the baseline "slow path".
type Matcher struct {
	rules []Rule
}

// NewMatcher builds a Matcher from an ordered slice of rules. Order is
// significant: the first rule whose predicates all match wins, exactly like
// scx_layered walking its layers top-to-bottom.
func NewMatcher(rules []Rule) *Matcher {
	// Copy so the caller cannot mutate our rule order after construction.
	cp := make([]Rule, len(rules))
	copy(cp, rules)
	return &Matcher{rules: cp}
}

// Classify derives the class/identity of a workload from its cgroup path.
//
// This is the per-decision hot-path work the baseline must repeat every time:
//  1. split the path into segments on '/',
//  2. for each rule (in order) test every predicate (AND) against the segments,
//  3. first rule that fully matches wins,
//  4. otherwise the workload is unmanaged.
//
// There is deliberately no cache: scx_layered re-runs its match logic per
// scheduling decision, and that re-derivation cost is exactly what Prism's
// precomputed identity map replaces with a single O(1) lookup.
func (m *Matcher) Classify(cgroupPath string) (class string, id identity.NumericIdentity, matched bool) {
	// Tokenize the path. strings.Split on '/' yields empty leading/trailing
	// tokens for absolute paths; the empty-segment predicates simply never
	// match, so we keep them rather than pay an allocation to filter.
	segments := strings.Split(cgroupPath, "/")

	for i := range m.rules {
		r := &m.rules[i]
		all := len(r.Preds) > 0 // an empty rule must not match everything
		for _, p := range r.Preds {
			if !p.match(segments) {
				all = false
				break
			}
		}
		if all {
			return r.Class, r.ID, true
		}
	}
	return "unmanaged", identity.IDUnmanaged, false
}

// DefaultRules returns a representative scx_layered-style rule set covering the
// common Kubernetes cgroup-v2 layout (kubepods.slice / guaranteed / burstable /
// besteffort, system & user slices, plus a few "label-ish" workload tokens that
// an operator would configure to bucket specific apps). Identities here are
// illustrative dynamic IDs; in scx_layered the analogue would be a per-layer
// index/weight rather than a Cilium identity, but the matching cost is the same.
//
// Order matters: more specific rules come first so first-match-wins picks them.
func DefaultRules() []Rule {
	base := identity.MinDynamicID
	return []Rule{
		// --- specific app buckets an operator would hand-configure ---
		{Class: "ingress-nginx", ID: base + 0, Preds: []Predicate{
			{Kind: PredHasPrefix, Value: "kubepods"},
			{Kind: PredContains, Value: "ingress-nginx"},
		}},
		{Class: "kube-system-dns", ID: base + 1, Preds: []Predicate{
			{Kind: PredContains, Value: "kube-system"},
			{Kind: PredContains, Value: "coredns"},
		}},
		{Class: "monitoring", ID: base + 2, Preds: []Predicate{
			{Kind: PredHasPrefix, Value: "kubepods"},
			{Kind: PredContains, Value: "prometheus"},
		}},
		{Class: "batch-job", ID: base + 3, Preds: []Predicate{
			{Kind: PredHasPrefix, Value: "kubepods"},
			{Kind: PredContains, Value: "job-"},
			{Kind: PredHasSuffix, Value: ".scope"},
		}},
		// --- QoS tiers of the kubepods hierarchy ---
		{Class: "pod-guaranteed", ID: base + 4, Preds: []Predicate{
			{Kind: PredHasPrefix, Value: "kubepods.slice"},
			{Kind: PredHasPrefix, Value: "kubepods-pod"},
			{Kind: PredHasSuffix, Value: ".scope"},
		}},
		{Class: "pod-burstable", ID: base + 5, Preds: []Predicate{
			{Kind: PredContains, Value: "kubepods-burstable"},
			{Kind: PredHasSuffix, Value: ".scope"},
		}},
		{Class: "pod-besteffort", ID: base + 6, Preds: []Predicate{
			{Kind: PredContains, Value: "kubepods-besteffort"},
			{Kind: PredHasSuffix, Value: ".scope"},
		}},
		// --- any remaining pod under kubepods (least specific pod rule) ---
		{Class: "pod-generic", ID: base + 7, Preds: []Predicate{
			{Kind: PredHasPrefix, Value: "kubepods"},
			{Kind: PredHasSuffix, Value: ".scope"},
		}},
		// --- node-level slices ---
		{Class: "system", ID: identity.IDHost, Preds: []Predicate{
			{Kind: PredHasPrefix, Value: "system.slice"},
		}},
		{Class: "user", ID: base + 8, Preds: []Predicate{
			{Kind: PredHasPrefix, Value: "user.slice"},
		}},
		{Class: "init", ID: identity.IDInit, Preds: []Predicate{
			{Kind: PredHasPrefix, Value: "init.scope"},
		}},
	}
}
