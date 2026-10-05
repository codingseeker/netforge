package integration

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/netforge/netforge/internal/chaos"
)

type benchmarkRow struct {
	Condition condition
	Results   []measurement
}

func writeBenchmarks(rows []benchmarkRow) error {
	var out bytes.Buffer
	out.WriteString("# Transport benchmark\n\n")
	out.WriteString("The comparison below was produced by ")
	out.WriteString("`TestTransportComparisonRecordsEveryScenario` in ")
	out.WriteString("`tests/integration/transport_test.go`, which writes this file ")
	out.WriteString("from the run it just measured. Re-running the test rewrites it.\n\n")

	out.WriteString("## Conditions\n\n")
	out.WriteString("Every transport is offered the same work: ")
	fmt.Fprintf(&out, "%d payloads of %d bytes, echoed back by the same service in the\n",
		benchmarkMessages, benchmarkSize)
	out.WriteString("server namespace and read by a client in the client namespace.\n")
	fmt.Fprintf(&out, "One transfer is bounded at %s, and a read on an unreliable transport\n",
		benchmarkTransfer)
	fmt.Fprintf(&out, "gives up after %s, because a datagram that is never retransmitted\n", benchmarkRead)
	out.WriteString("is not going to arrive.\n\n")
	fmt.Fprintf(&out, "Faults are applied by the kernel to the client end of the link with\n")
	fmt.Fprintf(&out, "`netem`, not by a proxy in userspace. A proxy that dropped bytes from a TCP\n")
	out.WriteString("stream would corrupt that stream rather than model packet loss, so TCP\n")
	out.WriteString("would never retransmit what the proxy swallowed and the measurement would be\n")
	out.WriteString("of the proxy.\n\n")

	out.WriteString("## Measured on\n\n")
	for _, line := range benchmarkEnvironment() {
		out.WriteString("- " + line + "\n")
	}
	out.WriteString("\n")

	out.WriteString("## Configuration\n\n")
	fmt.Fprintf(&out, "- laboratory: one client and one server on %s, MTU %d, one\n",
		benchmarkNetwork, benchmarkMTU)
	fmt.Fprintf(&out, "  point-to-point link per node\n")
	fmt.Fprintf(&out, "- endpoints: stream %d, udp %d, rudp %d, in the server namespace\n",
		benchmarkStreamPort, benchmarkUDPPort, benchmarkRUDPPort)
	fmt.Fprintf(&out, "- work: %d payloads of %d bytes per transport, %d bytes offered\n",
		benchmarkMessages, benchmarkSize, benchmarkMessages*benchmarkSize)
	fmt.Fprintf(&out, "- bounds: a transfer ends after %s, an unreliable read gives up after %s\n",
		benchmarkTransfer, benchmarkRead)
	fmt.Fprintf(&out, "- reliable transport: window %d, payload %d, retransmit timeout %s, at\n",
		benchmarkRUDPLimits.MaxWindow, benchmarkRUDPLimits.MaxPayload,
		benchmarkRUDPLimits.RetransmitTimeout)
	fmt.Fprintf(&out, "  most %d attempts per segment, idle timeout %s\n",
		benchmarkRUDPLimits.MaxRetransmits, benchmarkRUDPLimits.IdleTimeout)
	out.WriteString("\n")

	out.WriteString("## Results\n\n")
	out.WriteString("Throughput is what was offered to the transport; goodput is what arrived\n")
	out.WriteString("intact. Loss is the share of the offered payload that never came back.\n\n")

	header := "| Condition | Transport | Throughput | Goodput | Loss | Mean RTT | Retransmits | Complete |"
	out.WriteString(header + "\n")
	out.WriteString("| --- | --- | ---: | ---: | ---: | ---: | ---: | :---: |\n")
	for _, row := range rows {
		for _, got := range row.Results {
			fmt.Fprintf(&out, "| %s | %s | %.0f B/s | %.0f B/s | %.1f%% | %.2f ms | %d | %t |\n",
				row.Condition.name, got.Transport, got.throughput(), got.goodput(),
				got.lossPercent(), got.MeanRTT, got.Retransm, got.Complete)
		}
	}
	out.WriteString("\n")

	out.WriteString("## What each scenario is for\n\n")
	for _, row := range rows {
		fault := "no fault is applied"
		if row.Condition.spec != nil {
			fault = describeFault(*row.Condition.spec)
		}
		fmt.Fprintf(&out, "- **%s**: %s. %s.\n", row.Condition.name, fault, row.Condition.reason)
	}
	out.WriteString("\n")

	out.WriteString("## Reading the numbers\n\n")
	out.WriteString("- Plain UDP is offered the same payloads and loses whatever the path drops. ")
	out.WriteString("That is the point of including it: it shows what the path does on its own, ")
	out.WriteString("with no recovery at all.\n")
	out.WriteString("- TCP recovers everything, at the cost of head-of-line blocking: a lost ")
	out.WriteString("segment holds up every later payload until it is retransmitted.\n")
	out.WriteString("- The reliable UDP transport recovers payloads independently, so a loss ")
	out.WriteString("costs the segment that was lost rather than the whole window behind it, ")
	out.WriteString("which is what the retransmit column and the completion column show.\n")
	out.WriteString("- Retransmissions are counted by the transport itself and are zero for the ")
	out.WriteString("other two rows, which have no such counter to report.\n")

	out.WriteString("\n## What these numbers do not show\n\n")
	out.WriteString("They are a small-scale comparison, and the absolute figures are not ")
	out.WriteString("capacities of anything:\n\n")
	out.WriteString("- The transfer is 256 KiB over virtual links in a namespace with no ")
	out.WriteString("bandwidth ceiling, so a clean-path row is mostly measuring how fast a ")
	out.WriteString("namespace loopback can carry 256 payloads. The shapes across rows are ")
	out.WriteString("the finding; the byte rates are not a claim about a real network.\n")
	out.WriteString("- The fault is applied to the client end of the link, so it is the ")
	out.WriteString("client's sends that are impaired and the client's window that has to ")
	out.WriteString("recover.\n")
	out.WriteString("- The reliable transport infers loss only from a retransmission ")
	out.WriteString("timeout, because a datagram gives nothing else away. TCP also ")
	out.WriteString("retransmits on duplicate acknowledgements, which is why its loss rows ")
	out.WriteString("cost it far less. A transport that inferred loss from reordering or ")
	out.WriteString("duplication would narrow that gap, and this comparison does not claim ")
	out.WriteString("otherwise.\n")
	out.WriteString("- One run is one sample of a random fault. The retransmit counts in ")
	out.WriteString("particular move with the draw: rerunning the test rewrites this file ")
	out.WriteString("with a different sample, and a row that reports no retransmission under ")
	out.WriteString("loss is a run the path happened not to impair.\n")

	path := filepath.Join(repoRootPath, "docs", "BENCHMARKS.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, out.Bytes(), 0o644)
}

func benchmarkEnvironment() []string {
	out := []string{
		fmt.Sprintf("%s, %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH),
	}
	if release := readProcValue("/proc/sys/kernel/osrelease"); release != "" {
		out = append(out, "kernel "+release)
	} else {
		out = append(out, "kernel unknown: /proc/sys/kernel/osrelease was not readable")
	}
	cpus := fmt.Sprintf("%d CPUs", runtime.NumCPU())
	if model := cpuModel(); model != "" {
		out = append(out, cpus+", "+model)
	} else {
		out = append(out, cpus+", model unknown: /proc/cpuinfo was not readable")
	}
	out = append(out, benchmarkRevision())
	return out
}

func benchmarkRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "revision unknown: this binary carries no build information"
	}
	revision, dirty := "", false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if revision == "" {
		return "revision unknown: built outside a version control checkout"
	}
	if dirty {
		return "NetForge at " + revision + " with local modifications"
	}
	return "NetForge at " + revision
}

func cpuModel() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if name, value, found := strings.Cut(line, ":"); found &&
			strings.TrimSpace(name) == "model name" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func readProcValue(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func describeFault(spec chaos.Spec) string {
	parts := make([]string, 0, 3)
	if spec.Parameters.Latency > 0 {
		parts = append(parts, fmt.Sprintf("%s of added latency", spec.Parameters.Latency))
	}
	if spec.Parameters.Jitter > 0 {
		parts = append(parts, fmt.Sprintf("%s of jitter", spec.Parameters.Jitter))
	}
	if spec.Parameters.Loss > 0 {
		parts = append(parts, fmt.Sprintf("%s of packets dropped", spec.Parameters.Loss))
	}
	if len(parts) == 0 {
		return string(spec.Kind) + " with its default parameters"
	}
	return fmt.Sprintf("%s (%s)", spec.Kind, strings.Join(parts, ", "))
}
