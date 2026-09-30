package extension

import (
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	skillruntime "github.com/fwtllh-png/QCode/internal/adapter/skill"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
)

func TestReceiptRecordsSkillsReadInvocation(t *testing.T) {
	recorder := newTurnReceiptRecorder("review")
	recorder.observe(agentengine.Event{
		State: agentengine.RunningTools,
		ToolCall: &provider.ToolCall{
			ID: "call-1", Name: "skills_read", Arguments: `{"handle":"skh"}`,
		},
		Result: &tool.Result{
			Content: "instructions",
			Metadata: map[string]any{
				"resolved_skills": []skillruntime.ResolvedSkill{{
					Name: "review", Version: "1.0.0",
					Source: skillruntime.SourceWorkspace,
					Digest: strings.Repeat("c", 64), Locked: true,
				}},
			},
		},
	})
	receipt := recorder.build(turnReceiptObservations{})
	if len(receipt.Skills) != 1 || receipt.Skills[0].Name != "review" {
		t.Fatalf("skills = %+v", receipt.Skills)
	}
}
