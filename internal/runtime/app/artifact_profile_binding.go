package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type planExecutionProfile struct {
	Mode            string   `json:"mode"`
	Provider        string   `json:"provider"`
	Model           string   `json:"model"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	EnabledToolIDs  []string `json:"enabled_tool_ids,omitempty"`
	ApprovalPosture string   `json:"approval_posture"`
	ExecutionTarget string   `json:"execution_target"`
	MaxSteps        int      `json:"max_steps"`
}

// PlanExecutionProfileDigest binds a Plan to settings that affect execution.
// Planning controls are excluded so changing approval cannot strand the Plan.
func PlanExecutionProfileDigest(profile protocol.SessionProfile) (string, error) {
	if err := profile.Validate(); err != nil {
		return "", err
	}
	tools := append([]string(nil), profile.EnabledToolIDs...)
	slices.Sort(tools)
	payload, err := json.Marshal(planExecutionProfile{
		Mode: profile.Mode, Provider: profile.Provider, Model: profile.Model,
		ReasoningEffort: profile.ReasoningEffort, EnabledToolIDs: tools,
		ApprovalPosture: profile.ApprovalPosture,
		ExecutionTarget: profile.ExecutionTarget, MaxSteps: profile.MaxSteps,
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}
