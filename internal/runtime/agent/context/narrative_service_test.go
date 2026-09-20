package agentcontext

import (
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
)

func TestNarrativeOutputBudgetUsesAdvertisedModelLimit(t *testing.T) {
	tokens, outputBytes, err := NarrativeOutputBudget(
		NarrativeLimits{},
		model.Limits{ContextTokens: 8192, MaxOutputTokens: 2048},
		256,
	)
	// Byte budgets convert at dense-script density (three bytes per token).
	if err != nil || tokens != 2048 || outputBytes != 6144 {
		t.Fatalf("budget = %d tokens / %d bytes err=%v", tokens, outputBytes, err)
	}
}

func TestNarrativeOutputBudgetHonorsOperatorCeiling(t *testing.T) {
	tokens, outputBytes, err := NarrativeOutputBudget(
		NarrativeLimits{MaxOutputBytes: 512 * 4},
		model.Limits{ContextTokens: 8192, MaxOutputTokens: 2048},
		256,
	)
	// 2048 bytes are ceil(2048/3) = 683 dense-script tokens.
	if err != nil || tokens != 683 || outputBytes != 2048 {
		t.Fatalf("budget = %d tokens / %d bytes err=%v", tokens, outputBytes, err)
	}
}

func TestNarrativeOutputBudgetUsesRemainingContextWindow(t *testing.T) {
	tokens, outputBytes, err := NarrativeOutputBudget(
		NarrativeLimits{},
		model.Limits{ContextTokens: 1024, MaxOutputTokens: 4096},
		800,
	)
	want := uint64(1024 - 800 - narrativeFramingReserve)
	if err != nil || tokens != want || outputBytes != int(want*3) {
		t.Fatalf("budget = %d tokens / %d bytes err=%v want %d", tokens, outputBytes, err, want)
	}
}

func TestNarrativeOutputBudgetRejectsUnknownModelLimit(t *testing.T) {
	_, _, err := NarrativeOutputBudget(
		NarrativeLimits{},
		model.Limits{ContextTokens: 8192},
		256,
	)
	if err == nil || !strings.Contains(err.Error(), "does not advertise max output tokens") {
		t.Fatalf("err = %v", err)
	}
}

func TestNarrativeOutputBudgetRejectsInputThatFillsTheWindow(t *testing.T) {
	_, _, err := NarrativeOutputBudget(
		NarrativeLimits{},
		model.Limits{ContextTokens: 256, MaxOutputTokens: 128},
		200,
	)
	if err == nil || !strings.Contains(err.Error(), "exceeds the summary route context window") {
		t.Fatalf("err = %v", err)
	}
}
