package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestContextObservationSurvivesNoUsageAndCountsRecoveryBytes(t *testing.T) {
	r := newTurnReceiptRecorder("inspect")
	sample := &protocol.SampleContextData{ContextProjectionDigest: "projection", WindowHardInputTokens: 1000}
	projection := &protocol.ReceiptContextProjection{Digest: "projection", SourceHistoryDigest: "history", InputTokens: 120, OutputReserve: 80}
	event := agentengine.Event{State: agentengine.CallingModel, InputContext: sample, ContextProjection: projection,
		ModelExecution: &agentengine.ModelExecution{Kind: "provider_attempt", SampleID: "sample", Attempt: 1, Status: protocol.ProviderAttemptStarted}}
	r.observe(event)
	attempt := providerAttemptData(event)
	if attempt.ContextProjection.Digest != "projection" || attempt.Context.WindowHardInputTokens != 1000 {
		t.Fatal("attempt lost selection")
	}
	sample.WindowHardInputTokens = 2000
	for _, name := range []string{"turn_history", "result_get", "file_read"} {
		r.observe(agentengine.Event{State: agentengine.RunningTools, ToolCall: &provider.ToolCall{Name: name}, Result: &tool.Result{Content: "恢复"}})
	}
	r.observe(agentengine.Event{State: agentengine.RunningTools, ToolCall: &provider.ToolCall{Name: "turn_history"}, Result: &tool.Result{Content: "error", IsError: true}})
	receipt := r.build(turnReceiptObservations{})
	if receipt.ContextProjection.Digest != "projection" || receipt.ContextSample.WindowHardInputTokens != 1000 ||
		receipt.ContextRecovery.Calls != 3 || receipt.ContextRecovery.Failed != 1 || receipt.ContextRecovery.Bytes != 12 {
		t.Fatalf("receipt=%+v recovery=%+v", receipt, receipt.ContextRecovery)
	}
	raw, _ := json.Marshal(receipt.ContextProjection)
	if strings.Contains(string(raw), "恢复") {
		t.Fatal("projection contains tool text")
	}
}

func TestContextReceiptKeepsTerminalBudgetAndSampleOnSameRequest(t *testing.T) {
	r := newTurnReceiptRecorder("inspect")
	r.observe(agentengine.Event{State: agentengine.Streaming,
		SampleContext: &protocol.SampleContextData{ContextDigest: "earlier", MeasuredInputTokens: 900}})
	sample := protocol.SampleContextData{ContextDigest: "last", EstimatedTokens: 120,
		WindowFullActiveTokens: 130, MeasuredInputTokens: 130, WindowContextTokens: 2048,
		WindowOutputReserve: 512, WindowHardInputTokens: 1536,
		CompactionHeadroomTokens: 100, CompactionTargetTokens: 1436}
	r.observe(agentengine.Event{State: agentengine.Streaming, SampleContext: &sample})
	r.observe(agentengine.Event{State: agentengine.Completed, ContextBudget: &agentengine.ContextBudgetSnapshot{
		MeasurementSource: "provider_usage", ContextDigest: "last", ActiveTokens: 130,
		FullActiveTokens: 130, EstimatedTokens: 120, MaxContextTokens: 2048,
		OutputReserve: 512, HardInputTokens: 1536, CompactionHeadroomTokens: 100, CompactionTargetTokens: 1436,
	}})
	receipt := r.build(turnReceiptObservations{budget: r.budget})
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var replay protocol.ExecutionReceiptData
	if err := json.Unmarshal(encoded, &replay); err != nil {
		t.Fatal(err)
	}
	b := replay.ContextBudget
	if b == nil || replay.ContextSample == nil || b.ContextDigest != replay.ContextSample.ContextDigest ||
		b.ActiveTokens != replay.ContextSample.MeasuredInputTokens || b.MaxContextTokens != replay.ContextSample.WindowContextTokens ||
		b.OutputReserve != 512 || b.HardInputTokens != 1536 || b.CompactionHeadroomTokens != 100 ||
		b.CompactionTargetTokens != 1436 || b.MeasurementSource != "provider_usage" {
		t.Fatalf("receipt changed request accounting: budget=%+v sample=%+v", b, replay.ContextSample)
	}
}
