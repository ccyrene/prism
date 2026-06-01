// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/prism-bus/prism/pkg/abi"
	"github.com/prism-bus/prism/pkg/sink"
)

func newSimSink() (abi.Sink, error) { return sink.NewCompactSink(), nil }

func writeJSON(path string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		panic(err)
	}
}

func writeResolutionCSV(path string, prism, base []float64) {
	var sb strings.Builder
	sb.WriteString("trial,prism_ns_per_op,baseline_ns_per_op\n")
	n := len(prism)
	if len(base) < n {
		n = len(base)
	}
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "%d,%.4f,%.4f\n", i, prism[i], base[i])
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		panic(err)
	}
}

func writeScaleCSV(path string, rows []scaleRow) {
	var sb strings.Builder
	sb.WriteString("live_identities,alloc_ns_per_op,lookup_ns_per_op,baseline_ns_per_op\n")
	for _, r := range rows {
		fmt.Fprintf(&sb, "%d,%.4f,%.4f,%.4f\n", r.Count, r.AllocNsOp, r.LookupNsOp, r.BaselineNs)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		panic(err)
	}
}

func readFirstLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
}

func cpuModel() string {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "model name") {
			if i := strings.Index(line, ":"); i >= 0 {
				return strings.TrimSpace(line[i+1:])
			}
		}
	}
	return "unknown"
}
