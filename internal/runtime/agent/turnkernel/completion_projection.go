package turnkernel

import (
	"encoding/json"
	"maps"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func NewCompletionCandidate(
	call provider.ToolCall,
	result tool.Result,
	batchMutated bool,
	batchSize int,
	verificationCalls []string,
) CompletionCandidate {
	candidate := CompletionCandidate{
		CompletionCall:    call.ID,
		BatchMutated:      batchMutated,
		BatchSize:         batchSize,
		ToolError:         result.IsError,
		VerificationCalls: append([]string(nil), verificationCalls...),
	}
	var declaration *tool.CompletionDeclaration
	if result.Outcome != nil && result.Outcome.Facts != nil {
		declaration = result.Outcome.Facts.Completion
	}
	if declaration == nil {
		return candidate
	}
	candidate.DeclarationValid = true
	candidate.Status = declaration.Status
	candidate.Summary = declaration.Summary
	candidate.NoChangeReason = declaration.NoChangeReason
	candidate.NoChangeEvidence = append([]string(nil), declaration.NoChangeEvidence...)
	candidate.OutputMode = declaration.OutputMode
	candidate.PendingActions = append(
		[]string(nil),
		declaration.PendingActions...,
	)
	return candidate
}

func BindCompletionDecision(
	result *tool.Result,
	decision CompletionDecision,
) {
	if result == nil {
		return
	}
	result.Metadata = maps.Clone(result.Metadata)
	if result.Metadata == nil {
		result.Metadata = make(map[string]any)
	}
	result.Metadata["completion_declaration_accepted"] = decision.Accepted
	result.Metadata["completion_declaration_rejection"] = decision.Reason
	errorDetail := ""
	if result.IsError {
		errorDetail = strings.TrimSpace(result.Content)
		if errorDetail != "" {
			result.Metadata["completion_declaration_error"] = errorDetail
		}
	}
	if decision.Accepted {
		facts := tool.EnsureOutcomeFacts(result)
		if facts.Completion != nil {
			facts.Completion.ChangedPaths = append(
				[]string(nil),
				decision.ChangedPaths...,
			)
			facts.Completion.VerificationCallIDs = append(
				[]string(nil),
				decision.VerificationCalls...,
			)
			facts.Completion.MutationRevision = decision.Mutation
			facts.Completion.CallID = decision.CompletionCall
		}
	}
	result.Content = completionDecisionContent(
		decision.Accepted,
		decision.Reason,
		decision.RequiredAction,
		errorDetail,
	)
}

func completionDecisionContent(
	accepted bool,
	reason string,
	requiredAction string,
	errorDetail string,
) string {
	status := "rejected"
	if accepted {
		status = "accepted"
	}
	payload := map[string]any{
		"status":          status,
		"accepted":        accepted,
		"reason":          reason,
		"required_action": requiredAction,
	}
	if errorDetail != "" {
		payload["error_detail"] = errorDetail
	}
	content, err := json.Marshal(payload)
	if err != nil {
		return `{"status":"rejected","accepted":false,"reason":"encode_decision_failed"}`
	}
	return string(content)
}
