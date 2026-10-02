package app

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestValidateTerminalReceipt(t *testing.T) {
	validChanged := &protocol.ExecutionReceiptData{
		Intent:  protocol.TurnIntentWorkspaceChange,
		Outcome: protocol.TurnOutcomeChanged,
		Changes: []protocol.ReceiptChange{{
			Path: "calc.go", Kind: "modified",
		}},
		WorkspaceOutcome: &protocol.ReceiptWorkspaceOutcome{Status: "changed"},
	}
	tests := []struct {
		name      string
		receipt   *protocol.ExecutionReceiptData
		completed bool
		wantError bool
	}{
		{
			name: "failed_without_outcome",
			receipt: &protocol.ExecutionReceiptData{
				Intent: protocol.TurnIntentWorkspaceChange,
			},
		},
		{
			name: "failed_with_success_outcome",
			receipt: &protocol.ExecutionReceiptData{
				Intent:  protocol.TurnIntentWorkspaceChange,
				Outcome: protocol.TurnOutcomeChanged,
			},
			wantError: true,
		},
		{
			name: "completed_answer",
			receipt: &protocol.ExecutionReceiptData{
				Intent:  protocol.TurnIntentAnswer,
				Outcome: protocol.TurnOutcomeAnswered,
			},
			completed: true,
		},
		{
			name:      "completed_workspace_change",
			receipt:   validChanged,
			completed: true,
		},
		{
			name: "completed_workspace_change_without_changes",
			receipt: &protocol.ExecutionReceiptData{
				Intent:  protocol.TurnIntentWorkspaceChange,
				Outcome: protocol.TurnOutcomeChanged,
				WorkspaceOutcome: &protocol.ReceiptWorkspaceOutcome{
					Status: "unchanged",
				},
			},
			completed: true,
			wantError: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateTerminalReceipt(testCase.receipt, testCase.completed)
			if (err != nil) != testCase.wantError {
				t.Fatalf("validateTerminalReceipt() error = %v", err)
			}
		})
	}
}
