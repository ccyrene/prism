// SPDX-License-Identifier: Apache-2.0

// Command prism-net-exporter is a userspace Prometheus exporter for the Prism
// NET consumer (bpf/consumers/net_policy_prism.bpf.c). That cgroup_skb/egress
// program attributes every outbound packet to a workload identity read off the
// shared prism_identity bus and accumulates per-identity counters in its OWN
// writable map:
//
//	map  name : prism_net_stats        (BPF_MAP_TYPE_HASH)
//	key       : __u32  numeric identity (PRISM_ID_UNKNOWN == 0 for unmanaged)
//	value     : struct net_stat { __u64 packets; __u64 bytes; }  // 16 bytes, LE
//
// prismd's own /metrics surface (pkg/metrics) covers the CONTROL plane
// (identities minted, pods processed, propagation latency). It does NOT see this
// in-kernel counter map — that is the observability gap this exporter closes. It
// reads prism_net_stats and re-exposes it as Prometheus counters:
//
//	prism_net_packets_total{identity="256"}  <counter>
//	prism_net_bytes_total{identity="256"}    <counter>
//
// HOW IT READS THE MAP (two paths, tried in order):
//
//  1. cilium/ebpf (already a direct dep — see pkg/sink/bpf_sink.go): open the map
//     by its bpffs pin path and iterate it in-process. Fast, no fork per scrape.
//     The net consumer's maps are pinned wherever its loader put them. The
//     three-leg demo loads it with `bpftool prog loadall <obj> /sys/fs/bpf/net`,
//     which pins this map at /sys/fs/bpf/net/prism_net_stats; that is the default
//     here, overridable with -pin.
//
//  2. bpftool fallback: if the pin can't be opened (different loader / no fixed
//     pin), shell out to `bpftool map dump name prism_net_stats -j` and parse the
//     JSON. The map is resolved by NAME, so this works regardless of pin layout —
//     exactly how scripts/three-leg-demo.sh reads it. Requires bpftool on PATH
//     and CAP_BPF/root.
//
// BUILD:
//
//	go build -o prism-net-exporter ./cmd/prism-net-exporter
//
// RUN (needs CAP_BPF/root to read the kernel map; the program itself is loaded
// and attached separately, e.g. by scripts/three-leg-demo.sh):
//
//	sudo ./prism-net-exporter                       # :9465, pin /sys/fs/bpf/net/prism_net_stats
//	sudo ./prism-net-exporter -listen :9465 \
//	     -pin /sys/fs/bpf/net/prism_net_stats        # explicit
//	sudo ./prism-net-exporter -bpftool              # force the bpftool fallback
//
// Then point Prometheus at http://<node>:9465/metrics .
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Map ABI — the exact C definition in bpf/consumers/net_policy_prism.bpf.c.
// Keep byte-identical with `struct net_stat` (little-endian, 16 bytes).
const mapName = "prism_net_stats"

// prismIdentityMask is the Cilium-style 24-bit identity space (bpf:
// PRISM_IDENTITY_MASK == 0x00FFFFFF in bpf/prism_maps.bpf.h). The net consumer
// stores keys already masked (it writes prism_id(wid), which masks), but we
// re-apply the mask on read so the exposed label is canonical regardless of how
// the map was populated — never trust the producer for a value we put on a
// Prometheus label.
const prismIdentityMask uint32 = 0x00FFFFFF

// netStat is the Go twin of `struct net_stat { __u64 packets; __u64 bytes; }`.
// cilium/ebpf marshals the 16-byte little-endian map value straight into this
// (two contiguous u64 fields, no padding), exactly as pkg/abi mirrors the
// prism_identity value for the sink.
type netStat struct {
	Packets uint64
	Bytes   uint64
}

// statReader abstracts the two ways we can read prism_net_stats so the
// collector and the rest of main don't care which path is live.
type statReader interface {
	// read returns one entry per live identity in the map.
	read() ([]entry, error)
	describe() string
}

type entry struct {
	identity uint32
	stat     netStat
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("prism-net-exporter: ")

	var (
		listen     = flag.String("listen", ":9465", "address for /metrics, /healthz (Prometheus scrape target)")
		pin        = flag.String("pin", "/sys/fs/bpf/net/"+mapName, "bpffs pin path of the prism_net_stats map (cilium/ebpf path)")
		forceTool  = flag.Bool("bpftool", false, "skip the pinned-map path and always use `bpftool map dump name "+mapName+" -j`")
		bpftoolBin = flag.String("bpftool-bin", "bpftool", "bpftool binary to use for the fallback dump")
	)
	flag.Parse()

	reader, err := pickReader(*pin, *forceTool, *bpftoolBin)
	if err != nil {
		// Don't die: a missing map at startup is recoverable (the net consumer
		// may be loaded after us). Serve metrics anyway; the collector reports
		// the failure as prism_net_scrape_errors_total and /healthz stays 200 so
		// the process isn't churned. But if we couldn't even decide on a path,
		// that's a hard config error.
		log.Fatalf("init: %v", err)
	}
	log.Printf("reading %s via %s", mapName, reader.describe())

	reg := prometheus.NewRegistry()
	reg.MustRegister(newCollector(reader))

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	log.Printf("serving /metrics /healthz on %s", *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatalf("http server: %v", err)
	}
}

// pickReader chooses the pinned-map reader when the pin is openable, otherwise
// (or when forced) the bpftool reader. With -bpftool we never touch the pin.
func pickReader(pin string, force bool, bpftoolBin string) (statReader, error) {
	if !force {
		if r, err := newPinnedReader(pin); err == nil {
			return r, nil
		} else {
			log.Printf("pinned map %q not usable (%v); falling back to bpftool", pin, err)
		}
	}
	return newBpftoolReader(bpftoolBin)
}

// ---- path 1: cilium/ebpf over the bpffs pin -------------------------------

type pinnedReader struct {
	m   *ebpf.Map
	pin string
}

func newPinnedReader(pin string) (*pinnedReader, error) {
	m, err := ebpf.LoadPinnedMap(pin, nil)
	if err != nil {
		return nil, err
	}
	return &pinnedReader{m: m, pin: pin}, nil
}

func (r *pinnedReader) describe() string { return "pinned map " + r.pin + " (cilium/ebpf)" }

func (r *pinnedReader) read() ([]entry, error) {
	var (
		out  []entry
		k    uint32
		v    netStat
		iter = r.m.Iterate()
	)
	for iter.Next(&k, &v) {
		out = append(out, entry{identity: k, stat: v})
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s: %w", r.pin, err)
	}
	return out, nil
}

// ---- path 2: `bpftool map dump name prism_net_stats -j` --------------------

type bpftoolReader struct {
	bin string
}

func newBpftoolReader(bin string) (*bpftoolReader, error) {
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("bpftool fallback unavailable: %w", err)
	}
	return &bpftoolReader{bin: bin}, nil
}

func (r *bpftoolReader) describe() string {
	return r.bin + " map dump name " + mapName + " -j"
}

func (r *bpftoolReader) read() ([]entry, error) {
	cmd := exec.Command(r.bin, "map", "dump", "name", mapName, "-j")
	stdout, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("%s: %w: %s", r.describe(), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("%s: %w", r.describe(), err)
	}
	return parseBpftoolDump(stdout)
}

// bpftool -j dump entry. With BTF present each entry carries a "formatted"
// object with named fields; without BTF only the raw little-endian byte arrays
// "key"/"value" are present. We handle both, and the BTF case where key/value
// come back as plain JSON numbers/objects.
type dumpEntry struct {
	Key       json.RawMessage `json:"key"`
	Value     json.RawMessage `json:"value"`
	Formatted *struct {
		Key   json.RawMessage `json:"key"`
		Value struct {
			Packets json.RawMessage `json:"packets"`
			Bytes   json.RawMessage `json:"bytes"`
		} `json:"value"`
	} `json:"formatted"`
}

func parseBpftoolDump(raw []byte) ([]entry, error) {
	var dump []dumpEntry
	if err := json.Unmarshal(raw, &dump); err != nil {
		return nil, fmt.Errorf("parse bpftool json: %w", err)
	}
	out := make([]entry, 0, len(dump))
	for _, e := range dump {
		// Prefer BTF-formatted fields when bpftool provided them.
		if e.Formatted != nil {
			id, err := jsonUint(e.Formatted.Key)
			if err != nil {
				return nil, fmt.Errorf("formatted key: %w", err)
			}
			pkts, err := jsonUint(e.Formatted.Value.Packets)
			if err != nil {
				return nil, fmt.Errorf("formatted packets: %w", err)
			}
			byts, err := jsonUint(e.Formatted.Value.Bytes)
			if err != nil {
				return nil, fmt.Errorf("formatted bytes: %w", err)
			}
			out = append(out, entry{
				identity: uint32(id),
				stat:     netStat{Packets: pkts, Bytes: byts},
			})
			continue
		}
		// Raw form: key/value are little-endian byte arrays (u32 / 2x u64).
		id, err := leUintFromRaw(e.Key, 4)
		if err != nil {
			return nil, fmt.Errorf("raw key: %w", err)
		}
		vbytes, err := leBytes(e.Value, 16)
		if err != nil {
			return nil, fmt.Errorf("raw value: %w", err)
		}
		out = append(out, entry{
			identity: uint32(id),
			stat: netStat{
				Packets: leUint(vbytes[0:8]),
				Bytes:   leUint(vbytes[8:16]),
			},
		})
	}
	return out, nil
}

// jsonUint reads a uint64 from a JSON number or a quoted number ("0x.." or
// decimal). bpftool emits formatted scalars as numbers, but be liberal.
func jsonUint(raw json.RawMessage) (uint64, error) {
	if len(raw) == 0 {
		return 0, errors.New("empty")
	}
	s := strings.TrimSpace(string(raw))
	s = strings.Trim(s, `"`)
	return parseUint(s)
}

// leBytes decodes a JSON array of byte values (numbers or "0x.." strings) into a
// little-endian byte slice, padded/checked to want length.
func leBytes(raw json.RawMessage, want int) ([]byte, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("not a byte array: %w", err)
	}
	if len(items) < want {
		return nil, fmt.Errorf("got %d bytes, want >=%d", len(items), want)
	}
	b := make([]byte, want)
	for i := 0; i < want; i++ {
		v, err := jsonUint(items[i])
		if err != nil {
			return nil, err
		}
		b[i] = byte(v)
	}
	return b, nil
}

// leUintFromRaw decodes the first `n` bytes of a JSON byte array as a
// little-endian unsigned integer.
func leUintFromRaw(raw json.RawMessage, n int) (uint64, error) {
	b, err := leBytes(raw, n)
	if err != nil {
		return 0, err
	}
	return leUint(b), nil
}

func leUint(b []byte) uint64 {
	var v uint64
	for i := len(b) - 1; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v
}

func parseUint(s string) (uint64, error) {
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		return strconv.ParseUint(s[2:], 16, 64)
	}
	return strconv.ParseUint(s, 10, 64)
}

// ---- prometheus collector --------------------------------------------------

// collector pulls the map on every scrape (a Prometheus Collector), so the
// numbers are always current and there is no background goroutine to leak. The
// underlying BPF counters are monotonic, so we expose them as counters.
type collector struct {
	reader statReader

	packets      *prometheus.Desc
	bytes        *prometheus.Desc
	scrapeErrors prometheus.Counter
	up           *prometheus.Desc
}

func newCollector(r statReader) *collector {
	return &collector{
		reader: r,
		packets: prometheus.NewDesc(
			"prism_net_packets_total",
			"Egress packets attributed to a Prism workload identity by the net consumer (prism_net_stats map).",
			[]string{"identity"}, nil),
		bytes: prometheus.NewDesc(
			"prism_net_bytes_total",
			"Egress bytes attributed to a Prism workload identity by the net consumer (prism_net_stats map).",
			[]string{"identity"}, nil),
		scrapeErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "prism_net_scrape_errors_total",
			Help: "Failures reading the prism_net_stats map during a scrape.",
		}),
		up: prometheus.NewDesc(
			"prism_net_up",
			"1 if the last scrape read prism_net_stats successfully, else 0.",
			nil, nil),
	}
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.packets
	ch <- c.bytes
	ch <- c.up
	c.scrapeErrors.Describe(ch)
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	entries, err := c.reader.read()
	if err != nil {
		c.scrapeErrors.Inc()
		log.Printf("scrape: %v", err)
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
		ch <- c.scrapeErrors
		return
	}
	for _, e := range entries {
		id := strconv.FormatUint(uint64(e.identity&prismIdentityMask), 10)
		ch <- prometheus.MustNewConstMetric(c.packets, prometheus.CounterValue, float64(e.stat.Packets), id)
		ch <- prometheus.MustNewConstMetric(c.bytes, prometheus.CounterValue, float64(e.stat.Bytes), id)
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1)
	ch <- c.scrapeErrors
}
