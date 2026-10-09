package engine

import (
	"encoding/json"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
)

// compactModelContinuation changes only the next request's projection. The
// durable assembly and user-visible output retain the original response bytes.
func (e *Engine) compactModelContinuation(
	messages []provider.Message,
	before tokenWindow,
	measure func([]provider.Message) (tokenWindow, error),
) ([]provider.Message, bool, error) {
	encoded, err := json.Marshal(messages)
	if err != nil {
		return nil, false, err
	}
	// Store before replacing any content. A failed spill leaves the original
	// projection intact. One byte is the smallest supported surface preview,
	// not a context budget; the search below uses the measured request limit.
	stored, ok := e.options.Tools.PruneRawSurface("provider_continuation", string(encoded), 1)
	if !ok || stored.Handle == "" {
		return messages, false, nil
	}
	project := func(previewBytes int) []provider.Message {
		preview, _ := e.options.Tools.PruneResultSurface("provider_continuation", stored, previewBytes)
		return []provider.Message{promptcontext.ContinuationPressureFeedback(e.turn, preview.Content)}
	}
	best := project(1)
	minimum, err := measure(best)
	if err != nil {
		return nil, false, err
	}
	if minimum.total >= before.total {
		return messages, false, nil
	}
	// Even the smallest projection may not fit mandatory context. Return the
	// reduced view so the ordinary gate can report the actual remaining limit.
	if minimum.total > minimum.hardLimit {
		return best, true, nil
	}
	for low, high := 2, len(encoded)-1; low <= high; {
		middle := low + (high-low)/2
		candidate := project(middle)
		window, err := measure(candidate)
		if err != nil {
			return nil, false, err
		}
		if window.total <= window.hardLimit && window.total < before.total {
			best = candidate
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	return best, true, nil
}
