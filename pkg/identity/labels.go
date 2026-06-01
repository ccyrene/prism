// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"sort"
	"strings"
)

// securityRelevantPrefixes selects which labels contribute to identity. Like
// Cilium, only a stable, security-relevant subset matters; volatile labels
// (pod-template-hash, controller-revision-hash, timestamps) are excluded so the
// identity is stable across rollouts of the same logical workload.
var ignoredKeys = map[string]struct{}{
	"pod-template-hash":                         {},
	"controller-revision-hash":                  {},
	"statefulset.kubernetes.io/pod-name":        {},
	"apps.kubernetes.io/pod-index":              {},
	"batch.kubernetes.io/job-completion-index":  {},
	"controller-uid":                            {},
}

// ignoredPrefixes drops whole families of operational/volatile annotations.
var ignoredPrefixes = []string{
	"kubectl.kubernetes.io/",
	// Prism scheduling-policy hints (prism.io/latency-class, prism.io/weight,
	// see policy.go) are read SEPARATELY into the bus's scheduling-policy
	// sub-fields; they must NOT change the workload identity. Excluding the
	// prefix decouples "who you are" (identity) from "how you're treated"
	// (policy), so two replicas that differ only in a policy label still share
	// one identity, and an operator can retune policy without renumbering it.
	// (Distinct from the internal "io.prism/" authoritative keys in
	// WorkloadCanonical — that prefix is reversed and never a user label.)
	"prism.io/",
}

// isSecurityRelevant reports whether a label key participates in identity.
func isSecurityRelevant(key string) bool {
	if _, bad := ignoredKeys[key]; bad {
		return false
	}
	for _, p := range ignoredPrefixes {
		if strings.HasPrefix(key, p) {
			return false
		}
	}
	return true
}

// Canonical produces the canonical string form of a label set: the
// security-relevant labels as sorted "k=v" pairs joined by ';'. Two label maps
// that differ only in volatile labels (or ordering) yield the same canonical
// form, hence the same identity.
func Canonical(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	// Collect only the keys (not "k=v" pairs): sorting keys avoids allocating a
	// concatenated string per label, and lets us emit the canonical form in a
	// single Builder pass with one pre-sized backing buffer.
	keys := make([]string, 0, len(labels))
	total := 0
	for k, v := range labels {
		if !isSecurityRelevant(k) {
			continue
		}
		keys = append(keys, k)
		total += len(k) + len(v) + 2 // "k=v" + separator
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)

	var b strings.Builder
	b.Grow(total)
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	return b.String()
}

// Authoritative-key prefixes for the spoof-resistant canonical form. These keys
// carry the API/RBAC-controlled provenance fields (namespace, service account)
// that a workload CANNOT freely set on itself — they are assigned by the
// Kubernetes API server / admission, unlike pod labels which a workload's own
// controller can choose. We use an "io.prism/" prefix so they live in a
// reserved namespace that can never collide with a real user label key.
const (
	nsKeyPrefix = "io.prism/namespace="
	saKeyPrefix = "io.prism/serviceaccount="
)

// WorkloadCanonical produces the SPOOF-RESISTANT canonical form of a workload's
// identity. Identity derived from pod labels ALONE is spoofable: a workload
// that controls its own labels (e.g. via a malicious controller or a mutated
// pod spec) could mint or impersonate another workload's label set and thereby
// inherit its identity, scheduling treatment and network policy. To harden
// this, WorkloadCanonical folds in two fields the workload cannot freely set on
// itself — they are governed by the API server and RBAC:
//
//   - namespace: assigned at create time; cross-namespace pod creation is
//     RBAC-gated, so a workload cannot relocate itself into another namespace.
//   - serviceAccount: bound by RBAC; a pod cannot assume an arbitrary SA
//     (and use of a given SA is itself an authorization decision).
//
// The result is "io.prism/namespace=<ns>;io.prism/serviceaccount=<sa>;" followed
// by the existing label-based Canonical(labels). Because the authoritative
// prefix is always present (even for empty ns/sa), two workloads with identical
// labels but different namespace or service account get DIFFERENT canonical
// forms — hence different identities — while replicas of the same Deployment
// (identical ns + SA + labels) still collapse to ONE identity, preserving dedup.
//
// Canonical(labels) is intentionally left unchanged; WorkloadCanonical builds on
// top of it so the volatile-label filtering and ordering rules stay shared.
func WorkloadCanonical(labels map[string]string, namespace, serviceAccount string) string {
	labelCanon := Canonical(labels)

	// Pre-size: two prefixes + their values + the two ';' separators + labels.
	var b strings.Builder
	b.Grow(len(nsKeyPrefix) + len(namespace) + len(saKeyPrefix) +
		len(serviceAccount) + 2 + len(labelCanon))
	b.WriteString(nsKeyPrefix)
	b.WriteString(namespace)
	b.WriteByte(';')
	b.WriteString(saKeyPrefix)
	b.WriteString(serviceAccount)
	b.WriteByte(';')
	b.WriteString(labelCanon)
	return b.String()
}

// LabelHash is the FNV-1a/64 hash of the canonical label set. It is stored in
// the map value purely for cheap change-detection by consumers; it is NOT the
// identity (identities come from the allocator, not from hashing).
func LabelHash(canonical string) uint64 {
	const (
		offset64 = 1469598103934665603
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(canonical); i++ {
		h ^= uint64(canonical[i])
		h *= prime64
	}
	return h
}
