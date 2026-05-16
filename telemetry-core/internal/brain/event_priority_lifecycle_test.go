package brain

import "testing"

// TestDefaultEventPriority_RaceLifecycle asserts the four new race-lifecycle
// event types land at the correct default priority. The bus's TalkLevel
// floor and dispatcher coalesce window both key off priority, so a wrong
// default here would silently swallow the race-start celebration or queue
// the chequered-flag call behind a low-stakes coaching cue.
func TestDefaultEventPriority_RaceLifecycle(t *testing.T) {
	cases := map[EventType]int{
		EventLightsOut:    5, // dramatic launch, must voice
		EventPodium:       5, // win/podium, MUST voice + override "not sycophantic"
		EventRaceFinished: 5, // any non-podium finish — still must voice
		EventFinalLap:     4, // urgent strategic shift, voiced
	}
	for typ, want := range cases {
		if got := DefaultEventPriority(typ); got != want {
			t.Errorf("DefaultEventPriority(%s) = %d, want %d", typ, got, want)
		}
	}
}
