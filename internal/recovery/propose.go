package recovery

import (
	"strings"

	"github.com/netforge/netforge/internal/investigator"
	"github.com/netforge/netforge/internal/probe"
)

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

func suggest(report investigator.Report) (Action, string, bool) {
	observation := report.Observation
	namespace := observation.Namespace
	if namespace == "" {
		return "", "", false
	}
	switch report.Layer {
	case "network":

		return ActionRemoveFault, namespace, true
	case "routing":

		if observation.Target == "" {
			return "", "", false
		}
		return ActionRestoreRoute, observation.Target, true
	case "transport":

		if observation.Failure == probe.FailureRefused {
			return ActionRestartService, namespace, true
		}
		return ActionRemoveFault, namespace, true
	case "dns", "tls", "http":

		return ActionRestartService, namespace, true
	default:
		return "", "", false
	}
}

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

func (r Recommendation) Requested(from string) Request {
	return Request{
		Action:  r.Action,
		Target:  r.Target,
		From:    from,
		Reason:  r.Reason,
		Consent: false,
	}
}
