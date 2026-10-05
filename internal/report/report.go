// Package report renders an experiment run as a document a reader or a machine
// can keep.
//
// The report it produces is data, never prose: it is what the run measured,
// what the run judged, and where the run stopped. Errors are written as
// strings so a report round-trips through JSON and can be read by anything that
// speaks JSON.
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

// Report is the report of one run. Every field is derived from the run's own
// record, so the parts of a report can be cross-checked against each other.
type Report struct {
	// Name and ExperimentID identify the run.
	Name         string `json:"name"`
	ExperimentID string `json:"experiment_id"`
	// Seed is the seed the run declared, when it declared one.
	Seed *uint64 `json:"seed,omitempty"`
	// Succeeded reports whether the run finished. Failed names the state it
	// stopped in when it did not, and Error is why.
	Succeeded bool   `json:"succeeded"`
	Failed    string `json:"failed,omitempty"`
	Error     string `json:"error,omitempty"`
	// CleanedUp reports whether the laboratory was taken apart.
	CleanedUp bool `json:"cleaned_up"`
	// Duration is the whole run.
	Duration string `json:"duration"`
	// Laboratory is what the run built: the namespaces, their addresses, the
	// endpoints that were listening, who forwarded, and the software router's
	// counters. It is recorded while the laboratory existed and printed after it
	// was taken apart, because that is the only order in which both halves are
	// available.
	Laboratory *Laboratory `json:"laboratory,omitempty"`
	// Steps is the lifecycle as it ran, in order, so the report says which
	// states were walked and how long each one took.
	Steps []Step `json:"steps"`
	// Faults are the records the fault engine made of the injected faults.
	Faults []Fault `json:"faults,omitempty"`
	// Diagnosis is what the evidence supported. It is always present, even when
	// no evidence supported anything.
	Diagnosis Diagnosis `json:"diagnosis"`
	// Recoveries are the outcomes of the authorized actions, if any were.
	Recoveries []Recovery `json:"recoveries,omitempty"`
	// Metrics are the numbers the run measured.
	Metrics metrics.Metrics `json:"metrics"`
	// Baseline, Observed and Verified are the measurements of the three points
	// in the lifecycle that measure something.
	Baseline []probe.Measurement `json:"baseline"`
	Observed []probe.Measurement `json:"observed"`
	Verified []probe.Measurement `json:"verified"`
}

// Laboratory is the laboratory a run built, as it was before it was taken apart.
//
// It exists so a report says what it acted on and not only what it measured. A
// run that reports loss without naming the namespaces, the addresses, the routing
// mode and the endpoints that answered is a report a reader cannot check against
// anything.
type Laboratory struct {
	// ID is the laboratory's identifier.
	ID string `json:"id"`
	// Network is the network it was built on.
	Network string `json:"network"`
	// Routing names who forwarded in the router namespace.
	Routing string `json:"routing"`
	// KernelForwarding is what the router namespace reported for
	// net.ipv4.ip_forward. A software-routed run reads "0", which is the proof
	// that the kernel forwarded nothing.
	KernelForwarding string `json:"ip_forward"`
	// RouterNamespace is the namespace that forwards.
	RouterNamespace string `json:"router_namespace"`
	// RouterLinks are the router's addresses, one per link.
	RouterLinks []string `json:"router_links,omitempty"`
	// Nodes are the namespaces that carried traffic, with their addresses.
	Nodes []LaboratoryNode `json:"nodes,omitempty"`
	// Services are the endpoints each namespace reported bound.
	Services []LaboratoryService `json:"services,omitempty"`
	// Router is the software router's counters, when the run reported them.
	Router *router.Stats `json:"router,omitempty"`
	// RouterError is why they could not be reported, when they could not be.
	RouterError string `json:"router_error,omitempty"`
}

// LaboratoryNode is one namespace of a laboratory and the address it held.
type LaboratoryNode struct {
	Role      string `json:"role"`
	Namespace string `json:"namespace"`
	Address   string `json:"address,omitempty"`
	Gateway   string `json:"gateway,omitempty"`
}

// LaboratoryService is one serving namespace and what it reported listening.
type LaboratoryService struct {
	Namespace string   `json:"namespace"`
	Endpoints []string `json:"endpoints,omitempty"`
}

// Step is one state of the lifecycle as it ran.
type Step struct {
	// State is the lifecycle state this was.
	State string `json:"state"`
	// Detail is what the step was going to do.
	Detail string `json:"detail,omitempty"`
	// Duration is how long the step took.
	Duration string `json:"duration,omitempty"`
	// Error is why the step failed, if it did.
	Error string `json:"error,omitempty"`
	// Recovery names the actions the step performed.
	Recovery string `json:"recovery,omitempty"`
}

// Fault is the record of one injected fault, kept short enough to read and
// exact enough to trace back to the engine's own record.
type Fault struct {
	// ID is the engine's record identifier.
	ID string `json:"fault_id"`
	// Kind is the fault that was injected.
	Kind string `json:"type"`
	// Target is the endpoint that felt the fault.
	Target string `json:"target"`
	// Direction names the side of the link that was impaired.
	Direction string `json:"direction,omitempty"`
	// Status is the fault's recorded status.
	Status string `json:"status"`
}

// Diagnosis is what the evidence supported, and the reasoning that reached it.
type Diagnosis struct {
	// Classification is how far the evidence carried the conclusion.
	Classification string `json:"classification"`
	// Layer is the layer the failure was attributed to.
	Layer string `json:"layer,omitempty"`
	// Summary is the conclusion in a sentence.
	Summary string `json:"summary,omitempty"`
	// Rule is the rule that produced the classification.
	Rule string `json:"rule,omitempty"`
	// Evidence is how many measurements the report cites.
	Evidence int `json:"evidence_count"`
	// ProbesRun is how many probes the investigation took.
	ProbesRun int `json:"probes_run"`
}

// Recovery is the outcome of one authorized recovery action.
type Recovery struct {
	// Action and Target name the action that was authorized.
	Action string `json:"action"`
	Target string `json:"target"`
	// From is the namespace the verification probe ran in.
	From string `json:"from,omitempty"`
	// Reason is the operator's justification, written before the run.
	Reason string `json:"reason,omitempty"`
	// Passed reports whether the action ran and the measurement agreed.
	Passed bool `json:"passed"`
	// RecoveredAt is when the action finished.
	RecoveredAt string `json:"recovered_at,omitempty"`
	// Result is the verification verdict in a sentence.
	Result string `json:"result,omitempty"`
	// Error is why the action failed, if it did.
	Error string `json:"error,omitempty"`
}

// FromExperiment turns an experiment run into a report.
//
// The laboratory is passed in rather than read out of the run, because the
// environment that built it holds it as a value: the namespaces are gone by the
// time the run is reported, and a report that asked for them again would be
// asking about a laboratory that no longer exists.
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

// EncodeJSON writes the report as JSON.
func EncodeJSON(w io.Writer, run experiment.Report, laboratory *network.Summary) error {
	encoded, err := json.MarshalIndent(FromExperiment(run, laboratory), "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(encoded, '\n'))
	return err
}

// EncodeText writes the report as text a person can read.
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

// writeLaboratory prints what the run built, before the metrics that were
// measured on it.
//
// It is printed even when the run failed, and in particular when it failed in
// setup, because a run that built nothing is the run whose report has to say so
// rather than leave the section out.
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
	// The counters are the only way a reader can tell from outside the process
	// whether a forwarded byte came from NetForge or from the kernel, so they are
	// printed whenever the software router was in use, and their absence is said
	// rather than passed over.
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

// laboratoryOf renders the laboratory the environment recorded.
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
