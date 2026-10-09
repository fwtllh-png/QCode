package assembly

import (
	"errors"
	"strings"
	"testing"
)

func TestDisabledProfileNeverFires(t *testing.T) {
	detector := NewTextRepetitionDetector(RepetitionProfile{})
	for i := 0; i < 100; i++ {
		if err := detector.Observe("repeat repeat repeat repeat"); err != nil {
			t.Fatalf("disabled profile fired: %v", err)
		}
	}
}

func TestShortOutputDoesNotTrigger(t *testing.T) {
	detector := NewTextRepetitionDetector(DefaultRepetitionProfile())
	// 200 characters of repeated text — below the 3000 char minimum.
	repeated := strings.Repeat("hello world ", 17)
	if err := detector.Observe(repeated); err != nil {
		t.Fatalf("short output triggered: %v", err)
	}
}

func TestRepeatedSuffixTriggers(t *testing.T) {
	detector := NewTextRepetitionDetector(DefaultRepetitionProfile())
	// Build 3100+ chars: 1000 chars of unique prefix, then repeated spans.
	prefix := strings.Repeat("unique preamble content ", 40) // ~1000 chars
	span := "Please try the same approach again. "           // ~37 chars
	repeated := strings.Repeat(span, 80)                     // ~2960 chars
	full := prefix + repeated
	err := detector.Observe(full)
	if !errors.Is(err, ErrTextRepetition) {
		t.Fatalf("expected ErrTextRepetition, got: %v", err)
	}
	if !strings.Contains(err.Error(), "repeated") {
		t.Fatalf("error message lacks repetition detail: %v", err)
	}
}

func TestNonRepeatingLongOutputDoesNotTrigger(t *testing.T) {
	detector := NewTextRepetitionDetector(DefaultRepetitionProfile())
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("This is line number ")
		sb.WriteString(string(rune('A' + i%26)))
		sb.WriteString(" with unique content that does not repeat.\n")
	}
	if err := detector.Observe(sb.String()); err != nil {
		t.Fatalf("non-repeating output triggered: %v", err)
	}
}

func TestIncrementalDeltasTrigger(t *testing.T) {
	detector := NewTextRepetitionDetector(DefaultRepetitionProfile())
	// Build a unique prefix: enough characters, no repeated suffix.
	var prefix strings.Builder
	for i := 0; i < 200; i++ {
		prefix.WriteString("This is unique line number ")
		prefix.WriteString(string(rune('A' + i%26)))
		prefix.WriteString(string(rune('a' + (i*7)%26)))
		prefix.WriteString(" with distinct content. ")
	}
	if err := detector.Observe(prefix.String()); err != nil {
		t.Fatalf("prefix triggered: %v", err)
	}
	span := "loop here loop here "
	for i := 0; i < 50; i++ {
		if err := detector.Observe(span); err != nil {
			if !errors.Is(err, ErrTextRepetition) {
				t.Fatalf("unexpected error: %v", err)
			}
			return // triggered as expected
		}
	}
	t.Fatal("incremental deltas never triggered repetition detection")
}

func TestTrivialSpanIgnored(t *testing.T) {
	// Single repeated letter (e.g., "aaaa...") should not trigger because
	// isMeaningfulSpan requires two distinct alphanumeric characters.
	profile := DefaultRepetitionProfile()
	profile.MinTotalChars = 100 // lower threshold for testing
	profile.CheckEveryChars = 50
	detector := NewTextRepetitionDetector(profile)
	// 200 'a' characters = 50 spans of "aaaa" repeated 4 times, but the
	// span "aaaa" has only one distinct alphanumeric character.
	repeated := strings.Repeat("a", 200)
	if err := detector.Observe(repeated); err != nil {
		t.Fatalf("single-letter repetition triggered: %v", err)
	}
}

func TestWindowIsBounded(t *testing.T) {
	profile := DefaultRepetitionProfile()
	profile.WindowChars = 100
	profile.MinTotalChars = 100
	profile.CheckEveryChars = 50
	detector := NewTextRepetitionDetector(profile)
	// Feed 10000 chars of non-repeating text; window should stay at 100.
	for i := 0; i < 200; i++ {
		if err := detector.Observe(strings.Repeat("x", 50)); err != nil {
			t.Fatalf("non-repeating content triggered: %v", err)
		}
	}
	if len(detector.window) > 100 {
		t.Fatalf("window grew to %d, expected <= 100", len(detector.window))
	}
}

func TestRepetitionMonitorWrapsProject(t *testing.T) {
	var forwarded int
	inner := func(value StreamProjection) error {
		forwarded++
		return nil
	}
	monitor := NewRepetitionMonitor(DefaultRepetitionProfile(), inner)

	// Normal text passes through.
	if err := monitor.Project(StreamProjection{Text: "hello"}); err != nil {
		t.Fatalf("normal text errored: %v", err)
	}
	if forwarded != 1 {
		t.Fatalf("forwarded = %d, want 1", forwarded)
	}

	// Non-text event passes through without detection.
	if err := monitor.Project(StreamProjection{}); err != nil {
		t.Fatalf("empty text errored: %v", err)
	}
	if forwarded != 2 {
		t.Fatalf("forwarded = %d, want 2", forwarded)
	}
}

func TestRepetitionMonitorAbortsOnDetection(t *testing.T) {
	var forwarded int
	inner := func(value StreamProjection) error {
		forwarded++
		return nil
	}
	monitor := NewRepetitionMonitor(DefaultRepetitionProfile(), inner)

	var prefix strings.Builder
	for i := 0; i < 200; i++ {
		prefix.WriteString("Nonrepeating preamble line ")
		prefix.WriteString(string(rune('A' + i%26)))
		prefix.WriteString(string(rune('B' + (i*3)%26)))
		prefix.WriteString(" fills the window. ")
	}
	if err := monitor.Project(StreamProjection{Text: prefix.String()}); err != nil {
		t.Fatalf("prefix errored: %v", err)
	}
	span := "stuck in a loop stuck in a loop "
	for i := 0; i < 100; i++ {
		err := monitor.Project(StreamProjection{Text: span})
		if err != nil {
			if !errors.Is(err, ErrTextRepetition) {
				t.Fatalf("unexpected error: %v", err)
			}
			if forwarded < 1 {
				t.Fatal("monitor aborted before forwarding any events")
			}
			return
		}
	}
	t.Fatal("monitor never detected repetition")
}
