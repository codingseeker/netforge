package logging

import "testing"

func TestTheEventVocabularyIsAClosedSet(t *testing.T) {
	seen := make(map[string]bool, len(Events()))
	for _, name := range Events() {
		if name == "" {
			t.Error("the vocabulary holds an empty event name, which no record can carry")
			continue
		}
		if seen[name] {
			t.Errorf("the event %q is listed twice, so a test cannot tell the two apart", name)
		}
		seen[name] = true
		if !IsEvent(name) {
			t.Errorf("IsEvent(%q) = false for a name the vocabulary lists", name)
		}
	}
	for _, name := range []string{"", "stage", "command dispatch", "EventDispatch", "dispatched"} {
		if IsEvent(name) {
			t.Errorf("IsEvent(%q) = true for a name outside the vocabulary", name)
		}
	}
}

func TestEveryStageHasItsOwnEventName(t *testing.T) {
	stages := StageEvents()
	if len(stages) == 0 {
		t.Fatal("the vocabulary names no stage")
	}
	seen := make(map[string]bool, len(stages))
	for _, name := range stages {
		if seen[name] {
			t.Errorf("two stages share the event %q", name)
		}
		seen[name] = true
		if !IsEvent(name) {
			t.Errorf("the stage event %q is not in the vocabulary", name)
		}
	}
	if seen[EventStageUnknown] {
		t.Error("the fallback for a stage that does not exist is one of the stages")
	}
	for _, name := range []string{EventDispatch, EventCommandFailed, EventNamespace,
		EventFaultApplied, EventFaultRemoved, EventDiagnosis, EventAction,
		EventVerifiedPass, EventVerifiedFailed, EventRunSucceeded, EventRunFailed} {
		if seen[name] {
			t.Errorf("the event %q is both a stage and something else", name)
		}
	}
}
