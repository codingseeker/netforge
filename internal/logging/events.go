package logging

const (
	EventDispatch       = "dispatch"
	EventCommandFailed  = "command_failed"
	EventStageSetup     = "stage_setup"
	EventStageBaseline  = "stage_baseline"
	EventStageFault     = "stage_fault"
	EventStageObserve   = "stage_observe"
	EventStageDiagnose  = "stage_diagnose"
	EventStageRecover   = "stage_recover"
	EventStageVerify    = "stage_verify"
	EventStageReport    = "stage_report"
	EventStageCleanup   = "stage_cleanup"
	EventStageUnknown   = "stage_unknown"
	EventNamespace      = "namespace_created"
	EventFaultApplied   = "fault_applied"
	EventFaultRemoved   = "fault_removed"
	EventDiagnosis      = "diagnosis_made"
	EventAction         = "action_authorized"
	EventVerifiedPass   = "verification_passed"
	EventVerifiedFailed = "verification_failed"

	EventConfigurationValidated = "configuration validated"
	EventStateReconciled        = "state reconciled"
	EventRunSucceeded           = "experiment run succeeded"
	EventRunFailed              = "experiment run failed"
)

func Events() []string {
	return []string{
		EventDispatch,
		EventCommandFailed,
		EventStageSetup,
		EventStageBaseline,
		EventStageFault,
		EventStageObserve,
		EventStageDiagnose,
		EventStageRecover,
		EventStageVerify,
		EventStageReport,
		EventStageCleanup,
		EventStageUnknown,
		EventNamespace,
		EventFaultApplied,
		EventFaultRemoved,
		EventDiagnosis,
		EventAction,
		EventVerifiedPass,
		EventVerifiedFailed,
		EventConfigurationValidated,
		EventStateReconciled,
		EventRunSucceeded,
		EventRunFailed,
	}
}

func StageEvents() []string {
	return []string{
		EventStageSetup,
		EventStageBaseline,
		EventStageFault,
		EventStageObserve,
		EventStageDiagnose,
		EventStageRecover,
		EventStageVerify,
		EventStageReport,
		EventStageCleanup,
	}
}

func IsEvent(name string) bool {
	for _, known := range Events() {
		if known == name {
			return true
		}
	}
	return false
}
