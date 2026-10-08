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
	// Bytes never stand in for token admission.
	if err != nil || tokens != 2048 || outputBytes != 0 {
		t.Fatalf("budget = %d tokens / %d bytes err=%v", tokens, outputBytes, err)
	}
}

func TestNarrativeOutputBudgetHonorsOperatorCeiling(t *testing.T) {
	tokens, outputBytes, err := NarrativeOutputBudget(
		NarrativeLimits{MaxOutputBytes: 512 * 4},
		model.Limits{ContextTokens: 8192, MaxOutputTokens: 2048},
		256,
	)
	// An explicit byte limit is a transport check, independent of tokens.
	if err != nil || tokens != 2048 || outputBytes != 2048 {
		t.Fatalf("budget = %d tokens / %d bytes err=%v", tokens, outputBytes, err)
	}
}

func TestNarrativeOutputBudgetUsesRemainingContextWindow(t *testing.T) {
	tokens, outputBytes, err := NarrativeOutputBudget(
		NarrativeLimits{},
		model.Limits{ContextTokens: 1024, MaxOutputTokens: 4096},
		800,
	)
	want := uint64(1024 - 800)
	if err != nil || tokens != want || outputBytes != 0 {
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
		256,
	)
	if err == nil || !strings.Contains(err.Error(), "exceeds the summary route context window") {
		t.Fatalf("err = %v", err)
	}
}
