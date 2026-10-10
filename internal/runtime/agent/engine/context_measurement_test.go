package engine

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestTerminalContextBudgetUsesLastRequestIncludingFailedAttempt(t *testing.T) {
	var terminal *ContextBudgetSnapshot
	emitter := newTurnEmitter(1, func(event Event) error {
		if event.State == Failed {
			terminal = event.ContextBudget
		}
		return nil
	})
	observed := protocol.SampleContextData{ContextDigest: "first", EstimatedTokens: 900,
		MeasuredInputTokens: 1000, WindowFullActiveTokens: 1000, WindowContextTokens: 2048,
		WindowOutputReserve: 128, WindowHardInputTokens: 1920}
	if err := emitter.send(Streaming, Event{SampleContext: &observed}); err != nil {
		t.Fatal(err)
	}
	policy := ContextBudgetSnapshot{ActiveTokens: 99999, EstimatedTokens: 99999,
		MaxContextTokens: 99999, AutoCompactTokens: 1920, Compactions: 3}
	emitter.setContextBudget(policy)
	if emitter.contextBudget.ActiveTokens != 1000 || emitter.contextBudget.MeasurementSource != "provider_usage" {
		t.Fatal("actual usage was lost")
	}
	next := protocol.SampleContextData{ContextDigest: "last", EstimatedTokens: 800,
		WindowFullActiveTokens: 850, WindowContextTokens: 2048, WindowOutputReserve: 256,
		WindowHardInputTokens: 1792, CompactionHeadroomTokens: 400, CompactionTargetTokens: 1392}
	if err := emitter.send(Streaming, Event{InputContext: &next}); err != nil {
		t.Fatal(err)
	}
	next.WindowFullActiveTokens = 99999 // sender mutations must not change the frozen request
	emitter.setContextBudget(policy)
	if err := emitter.send(Failed, Event{}); err != nil {
		t.Fatal(err)
	}
	if terminal == nil || terminal.MeasurementSource != "request_estimate" || terminal.Observed ||
		terminal.ActiveTokens != 850 || terminal.EstimatedTokens != 800 || terminal.ContextDigest != "last" ||
		terminal.MaxContextTokens != 2048 || terminal.OutputReserve != 256 || terminal.HardInputTokens != 1792 ||
		terminal.CompactionHeadroomTokens != 400 || terminal.Compactions != 3 {
		t.Fatalf("terminal mixed request and maintenance: %+v", terminal)
	}
}

func TestContextBudgetWithoutRequestDoesNotClaimUsage(t *testing.T) {
	budget := contextBudgetFromSample(ContextBudgetSnapshot{ActiveTokens: 9000,
		EstimatedTokens: 9000, MaxContextTokens: 4096, Compactions: 1}, nil)
	if budget.MeasurementSource != "" || budget.ActiveTokens != 0 || budget.MaxContextTokens != 0 || budget.Compactions != 1 {
		t.Fatalf("unsampled history presented as usage: %+v", budget)
	}
}
