package assembly

import (
	"errors"
	"fmt"
)

// ErrTextRepetition is returned when the streaming output detector observes
// the model repeating the same text span consecutively. The engine decides
// how to respond (retry, feedback, or turn-level convergence handling).
var ErrTextRepetition = errors.New(
	"model output repeated the same text span consecutively",
)

// RepetitionProfile bounds the sliding-window repetition detector. Zero
// values disable the detector. The defaults match a conservative policy:
// only flag when the model has produced at least 3000 characters of output
// and the trailing 10000-character window contains one span repeated
// back-to-back at least 4 times.
type RepetitionProfile struct {
	// MinTotalChars starts detection only after this many characters of
	// output text, avoiding false positives on short legitimate replies.
	MinTotalChars int
	// WindowChars bounds the trailing text examined for repetition.
	WindowChars int
	// CheckEveryChars amortizes detection cost by running the suffix check
	// at this interval instead of on every delta.
	CheckEveryChars int
	// MinSpanChars is the shortest repeated span worth flagging. Spans
	// shorter than this are dominated by punctuation or single words.
	MinSpanChars int
	// RepeatCount is the minimum number of consecutive occurrences.
	RepeatCount int
	// MaxSpanChars bounds the span length so the suffix check stays linear.
	MaxSpanChars int
}

// DefaultRepetitionProfile returns the conservative defaults described on
// RepetitionProfile. These are safety limits with public provenance: they
// flag only large-output repetition loops, not ordinary prose.
func DefaultRepetitionProfile() RepetitionProfile {
	return RepetitionProfile{
		MinTotalChars:   3000,
		WindowChars:     10000,
		CheckEveryChars: 500,
		MinSpanChars:    4,
		RepeatCount:     4,
		MaxSpanChars:    768,
	}
}

// Disabled returns true when the profile has no effective thresholds.
func (p RepetitionProfile) Disabled() bool {
	return p.MinTotalChars <= 0 || p.RepeatCount < 2 || p.MinSpanChars < 1
}

// TextRepetitionDetector observes streaming text deltas and flags when the
// trailing output is a short span repeated back-to-back. It is deterministic,
// allocation-bounded, and safe for concurrent use from one producer.
type TextRepetitionDetector struct {
	profile      RepetitionProfile
	window       []rune
	windowStart  int
	totalChars   int
	pendingChars int
}

// NewTextRepetitionDetector creates a detector bound to the given profile.
func NewTextRepetitionDetector(profile RepetitionProfile) *TextRepetitionDetector {
	if profile.MaxSpanChars <= 0 {
		profile.MaxSpanChars = 768
	}
	return &TextRepetitionDetector{profile: profile}
}

// Observe feeds a text delta into the detector. It returns a non-nil error
// when the repetition threshold is hit. Callers should stop feeding deltas
// and propagate the error once it fires.
func (d *TextRepetitionDetector) Observe(text string) error {
	if d.profile.Disabled() || text == "" {
		return nil
	}
	runes := []rune(text)
	d.totalChars += len(runes)
	d.pendingChars += len(runes)
	d.window = append(d.window, runes...)

	// Trim the sliding window to its bounded size.
	if limit := d.profile.WindowChars; limit > 0 && len(d.window) > limit {
		d.window = d.window[len(d.window)-limit:]
	}

	if d.totalChars < d.profile.MinTotalChars ||
		d.pendingChars < d.profile.CheckEveryChars {
		return nil
	}
	d.pendingChars = 0

	if span, ok := detectRepeatedSuffix(d.window, d.profile); ok {
		return fmt.Errorf("%w: span %q repeated %d times in the last %d characters",
			ErrTextRepetition, truncateString(span, 80), d.profile.RepeatCount, len(d.window))
	}
	return nil
}

// detectRepeatedSuffix checks whether the trailing portion of text consists
// of one span repeated back-to-back at least `profile.RepeatCount` times.
// It scans span lengths from the minimum to the maximum allowed, returning
// the first (shortest) repeated span found.
func detectRepeatedSuffix(window []rune, profile RepetitionProfile) (string, bool) {
	n := len(window)
	count := profile.RepeatCount
	minSpan := profile.MinSpanChars
	maxSpan := n / count
	if profile.MaxSpanChars > 0 && maxSpan > profile.MaxSpanChars {
		maxSpan = profile.MaxSpanChars
	}
	if maxSpan < minSpan {
		return "", false
	}
	for spanLen := minSpan; spanLen <= maxSpan; spanLen++ {
		start := n - spanLen*count
		if start < 0 {
			break
		}
		span := window[start : start+spanLen]
		if !isMeaningfulSpan(span) {
			continue
		}
		repeated := true
		for i := 1; i < count; i++ {
			offset := start + i*spanLen
			if string(window[offset:offset+spanLen]) != string(span) {
				repeated = false
				break
			}
		}
		if repeated {
			return string(span), true
		}
	}
	return "", false
}

// isMeaningfulSpan requires at least two distinct alphanumeric characters so
// that trivial spans (single repeated letters, whitespace, or punctuation)
// do not trigger the detector.
func isMeaningfulSpan(span []rune) bool {
	var first rune
	for _, r := range span {
		if !isAlnum(r) {
			continue
		}
		lower := toLower(r)
		if first == 0 {
			first = lower
		} else if first != lower {
			return true
		}
	}
	return false
}

func isAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func toLower(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return r
}

func truncateString(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

// RepetitionMonitor wraps a Project callback with repetition detection.
// It drops through to the wrapped function when no repetition is detected,
// and returns ErrTextRepetition (aborting the stream) when the detector fires.
// The zero value is not usable; construct with NewRepetitionMonitor.
type RepetitionMonitor struct {
	detector *TextRepetitionDetector
	project  func(StreamProjection) error
}

// NewRepetitionMonitor wraps a Project callback. Pass a nil inner function
// if the caller only wants detection without forwarding. Pass a zero profile
// to use defaults.
func NewRepetitionMonitor(
	profile RepetitionProfile,
	project func(StreamProjection) error,
) *RepetitionMonitor {
	return &RepetitionMonitor{
		detector: NewTextRepetitionDetector(profile),
		project:  project,
	}
}

// Project implements the ConsumeConfig.Project signature, observing text
// deltas and forwarding non-text events unchanged.
func (m *RepetitionMonitor) Project(value StreamProjection) error {
	if value.Text != "" {
		if err := m.detector.Observe(value.Text); err != nil {
			return err
		}
	}
	if m.project == nil {
		return nil
	}
	return m.project(value)
}
