package report

import (
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/netforge/netforge/internal/chaos"
	"github.com/netforge/netforge/internal/experiment"
	"github.com/netforge/netforge/internal/investigator"
	"github.com/netforge/netforge/internal/metrics"
	"github.com/netforge/netforge/internal/network"
	"github.com/netforge/netforge/internal/probe"
	"github.com/netforge/netforge/internal/recovery"
	"github.com/netforge/netforge/internal/router"
)

type Report struct {
	Name         string `json:"name"`
	ExperimentID string `json:"experiment_id"`

	Seed *uint64 `json:"seed,omitempty"`

	Succeeded bool   `json:"succeeded"`
	Failed    string `json:"failed,omitempty"`
	Error     string `json:"error,omitempty"`

	CleanedUp bool `json:"cleaned_up"`

	Duration string `json:"duration"`

	Laboratory *Laboratory `json:"laboratory,omitempty"`

	Steps []Step `json:"steps"`

	Faults []Fault `json:"faults,omitempty"`

	Diagnosis Diagnosis `json:"diagnosis"`

	Recoveries []Recovery `json:"recoveries,omitempty"`

	Metrics metrics.Metrics `json:"metrics"`

	Baseline []probe.Measurement `json:"baseline"`
	Observed []probe.Measurement `json:"observed"`
	Verified []probe.Measurement `json:"verified"`
}

type Laboratory struct {
	ID string `json:"id"`

	Network string `json:"network"`

	Routing string `json:"routing"`

	KernelForwarding string `json:"ip_forward"`

	RouterNamespace string `json:"router_namespace"`

	RouterLinks []string `json:"router_links,omitempty"`

	Nodes []LaboratoryNode `json:"nodes,omitempty"`

	Services []LaboratoryService `json:"services,omitempty"`

	Router *router.Stats `json:"router,omitempty"`

	RouterError string `json:"router_error,omitempty"`
}

type LaboratoryNode struct {
	Role      string `json:"role"`
	Namespace string `json:"namespace"`
	Address   string `json:"address,omitempty"`
	Gateway   string `json:"gateway,omitempty"`
}

type LaboratoryService struct {
	Namespace string   `json:"namespace"`
	Endpoints []string `json:"endpoints,omitempty"`
}

type Step struct {
	State string `json:"state"`

	Detail string `json:"detail,omitempty"`

	Duration string `json:"duration,omitempty"`

	Error string `json:"error,omitempty"`

	Recovery string `json:"recovery,omitempty"`
}

type Fault struct {
	ID string `json:"fault_id"`

	Kind string `json:"type"`

	Target string `json:"target"`

	Direction string `json:"direction,omitempty"`

	Status string `json:"status"`
}

type Diagnosis struct {
	Classification string `json:"classification"`

	Layer string `json:"layer,omitempty"`

	Summary string `json:"summary,omitempty"`

	Rule string `json:"rule,omitempty"`

	Evidence int `json:"evidence_count"`

	ProbesRun int `json:"probes_run"`
}

type Recovery struct {
	Action string `json:"action"`
	Target string `json:"target"`

	From string `json:"from,omitempty"`

	Reason string `json:"reason,omitempty"`

	Passed bool `json:"passed"`

	RecoveredAt string `json:"recovered_at,omitempty"`

	Result string `json:"result,omitempty"`

	Error string `json:"error,omitempty"`
}

func FromExperiment(run experiment.Report, laboratory *network.Summary) Report {
	out := Report{
		Name:         run.Name,
		ExperimentID: run.ExperimentID,
		Seed:         run.Seed,
		Succeeded:    run.Succeeded(),
		Failed:       string(run.Failed),
		Error:        messageOf(run.Err),
		CleanedUp:    run.CleanedUp,
		Duration:     run.Duration.String(),
		Laboratory:   laboratoryOf(laboratory),
		Diagnosis:    diagnosisOf(run.Diagnosis),
		Metrics:      metrics.FromReport(run),
		Baseline:     run.Baseline,
		Observed:     run.Observed,
		Verified:     run.Verified,
	}
	for _, step := range run.Steps {
		out.Steps = append(out.Steps, Step{
			State:    string(step.State),
			Detail:   step.Detail,
			Duration: step.Duration.String(),
			Error:    messageOf(step.Err),
			Recovery: strings.Join(step.Recovery, ", "),
		})
	}
	for _, fault := range run.Faults {
		out.Faults = append(out.Faults, faultOf(fault))
	}
	for _, outcome := range run.Recoveries {
		out.Recoveries = append(out.Recoveries, recoveryOf(outcome))
	}
	return out
}

func EncodeJSON(w io.Writer, run experiment.Report, laboratory *network.Summary) error {
	encoded, err := json.MarshalIndent(FromExperiment(run, laboratory), "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(encoded, '\n'))
	return err
}

func EncodeText(w io.Writer, run experiment.Report, laboratory *network.Summary) error {
	report := FromExperiment(run, laboratory)
	if err := writeLine(w, "experiment: %s\n", report.Name); err != nil {
		return err
	}
	if err := writeLine(w, "experiment_id: %s\n", report.ExperimentID); err != nil {
		return err
	}
	if report.Seed != nil {
		if err := writeLine(w, "seed: %d\n", *report.Seed); err != nil {
			return err
		}
	}
	switch {
	case report.Succeeded:
		if err := writeLine(w, "result: succeeded\n"); err != nil {
			return err
		}
	case report.Failed != "":
		if err := writeLine(w, "result: failed in %s\n", report.Failed); err != nil {
			return err
		}
		if report.Error != "" {
			if err := writeLine(w, "error: %s\n", report.Error); err != nil {
				return err
			}
		}
	}
	if err := writeLine(w, "duration: %s\n", report.Duration); err != nil {
		return err
	}
	if err := writeLine(w, "cleaned up: %t\n", report.CleanedUp); err != nil {
		return err
	}

	if err := writeLine(w, "\nlifecycle:\n"); err != nil {
		return err
	}
	for _, step := range report.Steps {
		verdict := "ok"
		if step.Error != "" {
			verdict = "failed: " + step.Error
		}
		if err := writeLine(w, "  %-9s %s (%s) %s\n", step.State, step.Detail, step.Duration, verdict); err != nil {
			return err
		}
		if step.Recovery != "" {
			if err := writeLine(w, "             recovery: %s\n", step.Recovery); err != nil {
				return err
			}
		}
	}

	if err := writeLaboratory(w, report.Laboratory); err != nil {
		return err
	}

	if err := writeLine(w, "\nmetrics:\n"); err != nil {
		return err
	}
	m := report.Metrics
	lines := []string{
		"  packets sent:         " + itoa(m.PacketsSent),
		"  bytes:                " + itoa(m.Bytes),
		"  rtt:                  min " + m.RTT.Min + " average " + m.RTT.Average + " max " + m.RTT.Max,
		"  packet loss:          " + percent(m.PacketLoss) + "%",
		"  retransmissions:      " + itoa(m.Retransmissions),
		"  throughput:           " + itoaU64(m.Throughput) + " bps",
		"  connection failures:  " + itoa(m.ConnectionFailures),
		"  availability:         " + percent(m.Availability) + "%",
		"  recovery time:        " + m.RecoveryTime,
		"  diagnostic duration:  " + m.DiagnosticDuration,
	}
	for _, line := range lines {
		if err := writeLine(w, "%s\n", line); err != nil {
			return err
		}
	}
	return nil
}

func writeLaboratory(w io.Writer, lab *Laboratory) error {
	if lab == nil {
		return nil
	}
	if err := writeLine(w, "\nlaboratory:\n"); err != nil {
		return err
	}
	for _, line := range []string{
		"  id:         " + lab.ID,
		"  network:    " + lab.Network,
		"  routing:    " + lab.Routing,
		"  ip_forward: " + lab.KernelForwarding,
	} {
		if err := writeLine(w, "%s\n", line); err != nil {
			return err
		}
	}
	if len(lab.Nodes) > 0 {
		if err := writeLine(w, "  namespaces:\n"); err != nil {
			return err
		}
		for _, node := range lab.Nodes {
			line := "    " + pad(node.Namespace, 24) + pad(node.Role, 8) + node.Address
			if node.Gateway != "" {
				line += " via " + node.Gateway
			}
			if err := writeLine(w, "%s\n", line); err != nil {
				return err
			}
		}
	}
	if lab.RouterNamespace != "" {
		line := "    " + pad(lab.RouterNamespace, 24) + "router    forwarding here"
		if len(lab.RouterLinks) > 0 {
			line += ", " + strings.Join(lab.RouterLinks, ", ")
		}
		if err := writeLine(w, "%s\n", line); err != nil {
			return err
		}
	}
	if len(lab.Services) > 0 {
		if err := writeLine(w, "  services:\n"); err != nil {
			return err
		}
		for _, entry := range lab.Services {
			for _, endpoint := range entry.Endpoints {
				if err := writeLine(w, "    %s %s\n", pad(entry.Namespace, 24), endpoint); err != nil {
					return err
				}
			}
		}
	}
	if lab.Router == nil {
		if lab.RouterError != "" {
			return writeLine(w, "  router:     counters not read: %s\n", lab.RouterError)
		}
		return nil
	}

	stats := lab.Router
	return writeLine(w, "  router:\n"+
		"    received %d, forwarded %d, bytes %d\n"+
		"    ignored %d, local %d, malformed %d, no route %d\n"+
		"    ttl exceeded %d, oversize %d, unreachable %d\n"+
		"    arp requests %d, arp replies %d, unresolved %d\n"+
		"    queue %d of %d, routes %d, neighbors %d, send errors %d\n",
		stats.Received, stats.Forwarded, stats.Bytes,
		stats.Ignored, stats.Local, stats.Malformed, stats.NoRoute,
		stats.TTLExceeded, stats.Oversize, stats.Unreachable,
		stats.ARPRequests, stats.ARPReplies, stats.Unresolved,
		stats.QueueDepth, stats.QueueSize, stats.Routes, stats.Neighbors, stats.SendErrors)
}

func laboratoryOf(summary *network.Summary) *Laboratory {
	if summary == nil {
		return nil
	}
	lab := Laboratory{
		ID:               summary.ID,
		Network:          summary.Network,
		Routing:          summary.Routing.String(),
		KernelForwarding: summary.KernelForwarding,
		RouterNamespace:  summary.RouterNamespace,
		RouterLinks:      summary.RouterLinks,
		Router:           summary.Router,
		RouterError:      summary.RouterError,
	}
	for _, node := range summary.Nodes {
		lab.Nodes = append(lab.Nodes, LaboratoryNode{
			Role:      node.Role,
			Namespace: node.Namespace,
			Address:   node.Address,
			Gateway:   node.Gateway,
		})
	}
	for _, entry := range summary.Services {
		lab.Services = append(lab.Services, LaboratoryService{
			Namespace: entry.Namespace,
			Endpoints: entry.Endpoints,
		})
	}
	return &lab
}

func pad(value string, width int) string {
	for len(value) < width {
		value += " "
	}
	return value
}

func diagnosisOf(investigation investigator.Report) Diagnosis {
	return Diagnosis{
		Classification: string(investigation.Diagnosis),
		Layer:          investigation.Layer,
		Summary:        investigation.Summary,
		Rule:           investigation.Rule,
		Evidence:       len(investigation.Evidence),
		ProbesRun:      investigation.ProbesRun,
	}
}

func faultOf(fault chaos.Fault) Fault {
	return Fault{
		ID:        fault.ID,
		Kind:      string(fault.Kind),
		Target:    fault.Target.String(),
		Direction: string(fault.Direction),
		Status:    string(fault.Status),
	}
}

func recoveryOf(outcome recovery.Outcome) Recovery {
	record := Recovery{
		Action:      string(outcome.Authorization.Action()),
		Target:      outcome.Authorization.Target(),
		From:        outcome.Authorization.From(),
		Reason:      outcome.Authorization.Reason(),
		Passed:      outcome.Passed(),
		RecoveredAt: textTime(outcome.RecoveredAt),
		Result:      outcome.Verification.Reason,
		Error:       messageOf(outcome.Err),
	}
	return record
}

func messageOf(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func textTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.Format(time.RFC3339)
}

func writeLine(w io.Writer, format string, args ...any) error {
	_, err := io.WriteString(w, sprintf(format, args...))
	return err
}
