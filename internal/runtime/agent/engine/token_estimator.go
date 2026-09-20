package engine

import (
	"math"
	"sync"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

// The baseline heuristic (dense scripts per rune, other text at four
// characters per token) still overcounts sparse prose by roughly 2x and
// undercounts tokenizer-heavy scripts by up to roughly 2x. A reported ratio
// outside [minObservedEstimateRatio, maxObservedEstimateRatio] therefore
// indicates a provider accounting anomaly rather than content density, and is
// not learned. Boundary tests lock the exact behavior at both ends.
const (
	minObservedEstimateRatio = 0.25
	maxObservedEstimateRatio = 8
)

// calibratedTokenEstimator scales the base heuristic estimate by the ratio
// between the runtime estimate and the provider-reported input tokens of the
// same request, learned from the first provider usage observation onward.
// Calibrated estimates keep early-session window math, economic admission,
// and throughput admission aligned with real tokenizer pressure instead of
// discovering it through repeated prune and fold cycles.
type calibratedTokenEstimator struct {
	inner TokenEstimator

	mu    sync.Mutex
	ratio float64
}

func (c *calibratedTokenEstimator) Estimate(
	messages []provider.Message,
) (uint64, error) {
	base, err := c.inner.Estimate(messages)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	ratio := c.ratio
	c.mu.Unlock()
	if ratio <= 0 {
		return base, nil
	}
	return uint64(max(uint64(1), uint64(math.Ceil(float64(base)*ratio)))), nil
}

// EstimateImage forwards to the wrapped estimator when it implements image
// estimation so image attribution keeps its dedicated accounting.
func (c *calibratedTokenEstimator) EstimateImage(
	attachment provider.Attachment,
) (uint64, error) {
	if inner, ok := c.inner.(agentcontext.ImageEstimator); ok {
		return inner.EstimateImage(attachment)
	}
	return agentcontext.EstimateImageTokens(attachment), nil
}

// Observe records the provider-reported input tokens for a request whose
// runtime estimate is estimated. The latest in-range ratio wins: tokenizers
// are stable within a session, and immediate adoption is what corrects the
// next sample rather than a smoothed average.
//
// estimated must be the calibrated estimate Estimate returned for that same
// request. The raw heuristic base is recovered by dividing out the ratio that
// produced it, so the learned ratio always relates provider usage to the raw
// heuristic. Learning against the calibrated estimate instead would never
// converge: each adopted ratio changes the denominator of the next
// observation, alternating between the observed ratio and 1.
func (c *calibratedTokenEstimator) Observe(estimated, actual uint64) {
	if c == nil || estimated == 0 || actual == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	base := estimated
	if c.ratio > 0 {
		base = uint64(math.Round(float64(estimated) / c.ratio))
		if base == 0 {
			return
		}
	}
	ratio := float64(actual) / float64(base)
	if ratio < minObservedEstimateRatio || ratio > maxObservedEstimateRatio {
		return
	}
	c.ratio = ratio
}

// CalibrationRatio returns the multiplier currently applied to raw
// estimates; zero means no provider usage has been learned yet.
func (c *calibratedTokenEstimator) CalibrationRatio() float64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ratio
}

// estimateMessageTokens reports the engine's best current estimate for the
// messages: the calibrated estimator once provider usage has been observed,
// the dense-script-aware heuristic before that. Capacity decisions compared
// against calibrated window measurements must use the same estimator, or the
// two disagree about what a token is until the first observation arrives.
func (e *Engine) estimateMessageTokens(messages []provider.Message) uint64 {
	if estimate, err := e.options.TokenEstimator.Estimate(messages); err == nil &&
		estimate != 0 {
		return estimate
	}
	return agentcontext.EstimateMessageTokens(messages)
}

// EstimateMessageTokens exposes the calibrated estimate to hosts that budget
// against the same window, such as parent-context forking.
func (e *Engine) EstimateMessageTokens(messages []provider.Message) uint64 {
	return e.estimateMessageTokens(messages)
}
