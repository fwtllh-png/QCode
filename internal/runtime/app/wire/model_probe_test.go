package wire

import (
	"reflect"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
)

func TestWithDefaultReasoningEffortsUsesConventionalFallback(t *testing.T) {
	got := WithDefaultReasoningEfforts(
		"unknown-reasoning-model",
		model.Capabilities{Reasoning: true, Streaming: true, ToolCalls: true},
	)
	if !reflect.DeepEqual(
		got.ReasoningEfforts,
		[]string{"low", "medium", "high", "xhigh", "max"},
	) ||
		got.DefaultReasoningEffort != "medium" {
		t.Fatalf("capabilities = %+v", got)
	}
}

func TestWithDefaultReasoningEffortsPreservesExplicitMetadata(t *testing.T) {
	input := model.Capabilities{
		Reasoning: true, ReasoningEfforts: []string{"minimal", "max"},
		DefaultReasoningEffort: "max",
	}
	got := WithDefaultReasoningEfforts("unknown-reasoning-model", input)
	if !reflect.DeepEqual(got, input) {
		t.Fatalf("capabilities = %+v, want %+v", got, input)
	}
}
