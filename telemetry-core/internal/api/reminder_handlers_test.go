package api

import "testing"

// TestMessageIntentCollapses confirms that two messages with the same
// first three words collapse to the same dedup key — the property the
// Package E soft dedup relies on. LLM rephrasings tend to keep the verb
// + object identical and vary only the tail (more detail / no detail).
func TestMessageIntentCollapses(t *testing.T) {
	a := messageIntent("brake earlier here")
	b := messageIntent("brake earlier here, watch the rear")
	if a != b {
		t.Errorf("expected same intent, got %q vs %q", a, b)
	}
}

// TestMessageIntentDiffers — different intents should NOT collapse.
func TestMessageIntentDiffers(t *testing.T) {
	a := messageIntent("brake earlier at T3")
	b := messageIntent("apex later at T3")
	if a == b {
		t.Errorf("expected different intents, both collapsed to %q", a)
	}
}

// TestMessageIntentNormalisesPunctuation strips trailing punctuation /
// case so the dedup key is stable across LLM rephrasings.
func TestMessageIntentNormalisesPunctuation(t *testing.T) {
	a := messageIntent("Brake Earlier!")
	b := messageIntent("brake earlier.")
	if a != b {
		t.Errorf("expected punctuation-insensitive match, got %q vs %q", a, b)
	}
}
