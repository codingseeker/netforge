package recovery

import (
	"strings"

	"github.com/netforge/netforge/internal/investigator"
	"github.com/netforge/netforge/internal/probe"
)

// Propose reads a diagnosis and returns what it suggests should be done about
// it.
//
// This is the whole reason the package refuses to execute a diagnosis
// directly. Propose is a pure function over a report: it takes evidence and
// returns a sentence. It cannot reach an Executor, cannot construct an
// Authorization, and carries no Consent of its own, so the strongest thing any
// amount of measured suspicion can produce is a suggestion an operator has to
// agree with in writing.
//
// A report that does not separate the explanations returns no recommendation at
// all rather than a weak one.
func Propose(report investigator.Report) Recommendation {
	if report.Diagnosis == investigator.Unknown {
		return Recommendation{}
	}
	action, target, ok := suggest(report)
	if !ok {
		return Recommendation{}
	}
	return Recommendation{
		Action:   action,
		Target:   target,
		Reason:   report.Summary,
		Evidence: report.Rule,
	}
}

// suggest maps a diagnosis onto an allowed action, using the layers the
// investigator actually reports and the failure it measured.
//
// The mapping is deliberately narrow. A layer NetForge cannot repair produces no
// suggestion at all rather than an invented one, and the backend actions stay
// out of it: a backend belongs to a load balancer, and no evidence in a
// laboratory report speaks for one.
func suggest(report investigator.Report) (Action, string, bool) {
	observation := report.Observation
	namespace := observation.Namespace
	if namespace == "" {
		return "", "", false
	}
	switch report.Layer {
	case "network":
		// An echo that is lost is an impairment on the path, and taking an
		// injected fault back out is the only repair that fits.
		return ActionRemoveFault, namespace, true
	case "routing":
		// A route is restored for the destination that could not be reached,
		// not for the namespace the failure was seen from.
		if observation.Target == "" {
			return "", "", false
		}
		return ActionRestoreRoute, observation.Target, true
	case "transport":
		// A refusal is the far end answering, so the process is the problem. A
		// timeout or a reset is the path, so the impairment is.
		if observation.Failure == probe.FailureRefused {
			return ActionRestartService, namespace, true
		}
		return ActionRemoveFault, namespace, true
	case "dns", "tls", "http":
		// These layers are served by a laboratory service process, so the
		// process is what a restart would address.
		return ActionRestartService, namespace, true
	default:
		return "", "", false
	}
}

// Describe renders a recommendation for a report, so a reader sees the
// suggestion and the reason for it together.
func (r Recommendation) Describe() string {
	if r.Action == "" {
		return "no recovery is suggested: the evidence does not name a repairable failure"
	}
	parts := []string{r.Action.String() + " on " + r.Target}
	if r.Reason != "" {
		parts = append(parts, r.Reason)
	}
	if r.Evidence != "" {
		parts = append(parts, "rule "+r.Evidence)
	}
	return strings.Join(parts, "; ")
}

// Requested turns a recommendation into a request that still has to be
// authorized.
//
// Consent is left false on purpose. A caller that wants the repair sets it, and
// setting it is the explicit act the phase requires. Had this returned a
// consented request, a diagnosis would carry its own permission and the rule
// would be unenforceable in practice.
func (r Recommendation) Requested(from string) Request {
	return Request{
		Action:  r.Action,
		Target:  r.Target,
		From:    from,
		Reason:  r.Reason,
		Consent: false,
	}
}
