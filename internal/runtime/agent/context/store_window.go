package agentcontext

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// WindowLedger is the durable token baseline for one compaction window.
// Provider-observed input is authoritative; estimates price only later deltas.
type WindowLedger struct {
	ID                       string `json:"id"`
	Number                   uint64 `json:"number"`
	PrefillTokens            uint64 `json:"prefill_tokens,omitempty"`
	PrefillObserved          bool   `json:"prefill_observed,omitempty"`
	FullActiveTokens         uint64 `json:"full_active_tokens,omitempty"`
	ObservedEstimateTokens   uint64 `json:"observed_estimate_tokens,omitempty"`
	ObservedContextDigest    string `json:"observed_context_digest,omitempty"`
	ToolDefinitionTokens     uint64 `json:"tool_definition_tokens,omitempty"`
	LastProviderInputTokens  uint64 `json:"last_provider_input_tokens,omitempty"`
	LastProviderCachedTokens uint64 `json:"last_provider_cached_tokens,omitempty"`
	Digest                   string `json:"digest"`
}

// WindowProjection is the complete accounting used for one provider request.
type WindowProjection struct {
	ID                   string
	Number               uint64
	Observed             bool
	FullActiveTokens     uint64
	PrefillTokens        uint64
	BodyTokens           uint64
	ToolDefinitionTokens uint64
	PendingTokens        uint64
	OutputReserve        uint64
	AutoCompactLimit     uint64
	HardLimit            uint64
	EstimatedTokens      uint64
}

type WindowPolicy struct {
	PrepareTokens, AutoTokens, EmergencyTokens uint64
	Scope                                      string
}

// WindowThresholds resolve operator ceilings. Zero values mean "no early
// compact tier": the compact and emergency limits equal the hard input
// capacity. Percents are not derived here.
func WindowThresholds(policy WindowPolicy, hardInputCapacity uint64) (uint64, uint64, uint64) {
	compact := hardInputCapacity
	if policy.AutoTokens != 0 {
		compact = min(policy.AutoTokens, hardInputCapacity)
	}
	prepare := compact
	if policy.PrepareTokens != 0 {
		prepare = min(policy.PrepareTokens, compact)
	}
	emergency := hardInputCapacity
	if policy.EmergencyTokens != 0 {
		emergency = min(policy.EmergencyTokens, hardInputCapacity)
	}
	return prepare, compact, emergency
}

type BudgetSnapshot struct {
	WindowID              string `json:"window_id,omitempty"`
	WindowNumber          uint64 `json:"window_number,omitempty"`
	Observed              bool   `json:"observed,omitempty"`
	ActiveTokens          uint64 `json:"active_tokens"`
	FullActiveTokens      uint64 `json:"full_active_tokens,omitempty"`
	PrefillTokens         uint64 `json:"prefill_tokens,omitempty"`
	BodyTokens            uint64 `json:"body_tokens,omitempty"`
	ToolDefinitionTokens  uint64 `json:"tool_definition_tokens,omitempty"`
	PendingTokens         uint64 `json:"pending_tokens,omitempty"`
	OutputReserve         uint64 `json:"output_reserve,omitempty"`
	HardInputTokens       uint64 `json:"hard_input_tokens,omitempty"`
	LimitSource           string `json:"limit_source,omitempty"`
	OutputSource          string `json:"output_source,omitempty"`
	AutoCompactTokens     uint64 `json:"auto_compact_tokens"`
	PrepareTokens         uint64 `json:"prepare_tokens,omitempty"`
	EmergencyTokens       uint64 `json:"emergency_tokens,omitempty"`
	RecentTailTurns       int    `json:"recent_tail_turns,omitempty"`
	KeepRecentToolResults int    `json:"keep_recent_tool_results,omitempty"`
	HistoryTokenCeiling   uint64 `json:"history_token_ceiling,omitempty"`
	Digest                string `json:"digest,omitempty"`
	NarrativeMode         string `json:"narrative_mode,omitempty"`
	EstimatedTokens       uint64 `json:"estimated_tokens,omitempty"`
	MaxContextTokens      uint64 `json:"max_context_tokens,omitempty"`
	Compactions           int    `json:"compactions"`
}

func NewWindowLedger(id string, number uint64) (WindowLedger, error) {
	if id == "" || number == 0 {
		return WindowLedger{}, errors.New("window id and number are required")
	}
	value := WindowLedger{ID: id, Number: number}
	value.seal()
	return value, nil
}

func CreateWindowLedger(number uint64) (WindowLedger, error) {
	id, err := protocol.NewWindowID()
	if err != nil {
		return WindowLedger{}, err
	}
	return NewWindowLedger(id, number)
}

func FallbackWindowLedger(current WindowLedger, seed string) WindowLedger {
	number := current.Number + 1
	sum := sha256.Sum256([]byte(fmt.Sprintf(
		"%s:%d:%s", current.ID, number, seed,
	)))
	value, _ := NewWindowLedger(
		"window_"+hex.EncodeToString(sum[:16]),
		number,
	)
	return value
}

func CloneWindowLedger(value WindowLedger) WindowLedger {
	return value
}

func (w WindowLedger) Valid() bool {
	return w.ID != "" && w.Number != 0 && w.Digest != "" &&
		w.Digest == w.digest()
}

// Prepare establishes an estimated prefill until the first provider usage
// arrives, then projects the latest observed full input plus only the estimate
// delta introduced after that observation.
func (w *WindowLedger) Prepare(
	context *protocol.SampleContextData,
	outputReserve uint64,
	autoCompactLimit uint64,
	hardLimit uint64,
) WindowProjection {
	if context == nil {
		return WindowProjection{}
	}
	if !w.Valid() {
		return WindowProjection{
			FullActiveTokens:     context.EstimatedTokens,
			PrefillTokens:        context.EstimatedTokens,
			ToolDefinitionTokens: context.ToolDefinitionTokens,
			PendingTokens:        context.EstimatedTokens,
			OutputReserve:        outputReserve, AutoCompactLimit: autoCompactLimit,
			HardLimit: hardLimit, EstimatedTokens: context.EstimatedTokens,
		}
	}
	if w.PrefillTokens == 0 {
		w.PrefillTokens = context.EstimatedTokens
	}
	full := context.EstimatedTokens
	pending := context.EstimatedTokens
	if w.ObservedEstimateTokens != 0 && w.LastProviderInputTokens != 0 {
		if context.EstimatedTokens >= w.ObservedEstimateTokens {
			delta := context.EstimatedTokens - w.ObservedEstimateTokens
			full = w.LastProviderInputTokens + delta
			pending = delta
		} else {
			delta := w.ObservedEstimateTokens - context.EstimatedTokens
			full = w.LastProviderInputTokens - min(w.LastProviderInputTokens, delta)
			pending = 0
		}
	}
	w.FullActiveTokens = full
	w.ToolDefinitionTokens = context.ToolDefinitionTokens
	w.seal()
	return WindowProjection{
		ID: w.ID, Number: w.Number, Observed: w.PrefillObserved,
		FullActiveTokens: full, PrefillTokens: w.PrefillTokens,
		BodyTokens:           full - min(full, w.PrefillTokens),
		ToolDefinitionTokens: context.ToolDefinitionTokens,
		PendingTokens:        pending, OutputReserve: outputReserve,
		AutoCompactLimit: autoCompactLimit, HardLimit: hardLimit,
		EstimatedTokens: context.EstimatedTokens,
	}
}

// Observe replaces the estimate-led baseline with provider usage for the
// exact MessageSnapshot identified by context.
func (w *WindowLedger) Observe(
	context protocol.SampleContextData,
	inputTokens uint64,
	cachedTokens uint64,
) {
	if !w.Valid() || inputTokens == 0 || context.EstimatedTokens == 0 {
		return
	}
	if !w.PrefillObserved {
		w.PrefillTokens = inputTokens
		w.PrefillObserved = true
	}
	w.FullActiveTokens = inputTokens
	w.ObservedEstimateTokens = context.EstimatedTokens
	w.ObservedContextDigest = context.ContextDigest
	w.ToolDefinitionTokens = context.ToolDefinitionTokens
	w.LastProviderInputTokens = inputTokens
	w.LastProviderCachedTokens = min(inputTokens, cachedTokens)
	w.seal()
}

// RebaseEstimates rescales the recorded estimate baseline after the session
// estimator's calibration ratio changes, so later Prepare deltas compare
// estimates from one measurement basis instead of reading a ratio change as
// content growth. A zero from-ratio marks a baseline recorded before the
// first calibration, when estimates were still raw.
func (w *WindowLedger) RebaseEstimates(from, to float64) {
	if !w.Valid() || w.ObservedEstimateTokens == 0 || w.LastProviderInputTokens == 0 {
		return
	}
	if from <= 0 {
		from = 1
	}
	if to <= 0 || to == from {
		return
	}
	scaled := math.Round(float64(w.ObservedEstimateTokens) * to / from)
	if scaled < 1 {
		scaled = 1
	}
	w.ObservedEstimateTokens = uint64(scaled)
	w.seal()
}

func (w WindowLedger) Advance(id string) (WindowLedger, error) {
	return NewWindowLedger(id, w.Number+1)
}

func ApplyWindowProjection(
	context *protocol.SampleContextData,
	value WindowProjection,
) {
	if context == nil {
		return
	}
	context.WindowID = value.ID
	context.WindowNumber = value.Number
	context.WindowObserved = value.Observed
	context.WindowProjectedTokens = value.FullActiveTokens
	context.WindowFullActiveTokens = value.FullActiveTokens
	context.WindowPrefillTokens = value.PrefillTokens
	context.WindowBodyTokens = value.BodyTokens
	context.WindowPendingTokens = value.PendingTokens
	context.WindowOutputReserve = value.OutputReserve
}

func (w WindowLedger) digest() string {
	w.Digest = ""
	encoded, _ := json.Marshal(w)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (w *WindowLedger) seal() {
	w.Digest = w.digest()
}
