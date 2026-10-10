package app

import (
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func ProjectToolExecutionReceipt(
	source *tool.ExecutionReceipt,
) *protocol.ToolExecutionReceipt {
	if source == nil {
		return nil
	}
	projected := &protocol.ToolExecutionReceipt{
		GuardianReviewID: source.GuardianReviewID,
		Tool: protocol.ToolExecutionRef{
			Name: source.Tool.Name, Source: source.Tool.Source,
			CatalogID: source.Tool.CatalogID, Generation: source.Tool.Generation,
			Revision: source.Tool.Revision,
		},
		Source: string(source.Source), Disposition: string(source.Disposition),
		ApprovalWait: source.ApprovalWait, DispatchWait: source.DispatchWait,
		ClaimWait: source.ClaimWait, TerminalStatus: string(source.TerminalStatus),
		TerminalOwner: string(source.TerminalOwner), Teardown: source.Teardown,
		TeardownMS: source.TeardownMS, TeardownTimedOut: source.TeardownTimedOut,
		Attempts: make([]protocol.ToolAttemptReceipt, len(source.Attempts)),
	}
	projected.PolicyDenial = projectToolPolicyDecision(source.PolicyDenial)
	for index, attempt := range source.Attempts {
		projected.Attempts[index] = projectToolAttemptReceipt(attempt)
	}
	return projected
}

func projectToolPolicyDecision(
	source *tool.PolicyDecisionReceipt,
) *protocol.ToolPolicyDecision {
	if source == nil {
		return nil
	}
	return &protocol.ToolPolicyDecision{
		Action: source.Action, Layer: source.Layer, Code: source.Code,
	}
}

func projectToolAttemptReceipt(
	source tool.AttemptReceipt,
) protocol.ToolAttemptReceipt {
	projected := protocol.ToolAttemptReceipt{
		FullAccess: source.FullAccess,
		Sequence:   source.Sequence, Sandbox: source.Sandbox,
		Status: string(source.Status), TerminalOwner: string(source.TerminalOwner),
		Reason:                  source.Reason,
		OperationSchemaVersion:  source.OperationSchemaVersion,
		OperationDigest:         source.OperationDigest,
		LeaseID:                 source.LeaseID,
		LeaseState:              source.LeaseState,
		LeaseAttempt:            source.LeaseAttempt,
		WorkspaceID:             source.WorkspaceID,
		WorkspaceGeneration:     source.WorkspaceGeneration,
		SubjectKind:             source.SubjectKind,
		SubjectID:               source.SubjectID,
		SubjectDigest:           source.SubjectDigest,
		SubjectGeneration:       source.SubjectGeneration,
		PolicyRevision:          source.PolicyRevision,
		Policy:                  projectToolPolicyDecision(source.Policy),
		SandboxPolicyID:         source.SandboxPolicyID,
		EffectKind:              source.EffectKind,
		EffectRisk:              source.EffectRisk,
		EffectReversibility:     source.EffectReversibility,
		WorkspaceTransaction:    source.WorkspaceTransaction,
		PermissionSchemaVersion: source.PermissionSchemaVersion,
		PermissionRevision:      source.PermissionRevision,
		PermissionDigest:        source.PermissionDigest,
		PermissionCapability:    string(source.PermissionCapability),
		PermissionAccess:        string(source.PermissionAccess),
		Enforcement:             source.Enforcement, Backend: source.Backend,
		EffectiveControls:  source.EffectiveControls.StringMap(),
		WorkspaceRoot:      source.WorkspaceRoot,
		ReadRoots:          append([]string(nil), source.ReadRoots...),
		WritePaths:         append([]string(nil), source.WritePaths...),
		DeniedWriteRoots:   append([]string(nil), source.DeniedWriteRoots...),
		WorkspaceBaseWrite: source.WorkspaceBaseWrite,
		NetworkMode:        source.NetworkMode,
		NetworkTargets:     append([]string(nil), source.NetworkTargets...),
		ManagedProxyPort:   source.ManagedProxyPort,
		LoopbackAllowed:    source.LoopbackAllowed,
		ProcessAllowed:     source.ProcessAllowed,
		StartedAt:          source.StartedAt, CompletedAt: source.CompletedAt,
		DurationMS: source.DurationMS, Teardown: source.Teardown,
		TeardownMS: source.TeardownMS, TeardownTimedOut: source.TeardownTimedOut,
		Provenance: make(
			[]protocol.ToolPermissionProvenance,
			len(source.Provenance),
		),
	}
	for index, provenance := range source.Provenance {
		projected.Provenance[index] = protocol.ToolPermissionProvenance{
			Kind: provenance.Kind, Value: provenance.Value,
			Digest: provenance.Digest, Revision: provenance.Revision,
		}
	}
	if source.Denial != nil {
		projected.Denial = &protocol.ToolSandboxDenial{
			Backend:   source.Denial.Backend,
			Operation: string(source.Denial.Operation),
			Resource:  source.Denial.Resource, ReasonCode: source.Denial.ReasonCode,
			Protocol: source.Denial.Protocol, Port: source.Denial.Port,
		}
	}
	if source.Amendment != nil {
		projected.Amendment = &protocol.ToolPermissionAmendmentReceipt{
			BasePermissionDigest: source.Amendment.BasePermissionDigest,
			Kind:                 source.Amendment.Kind, Resource: source.Amendment.Resource,
			Protocol: source.Amendment.Protocol, Port: source.Amendment.Port,
			Capability:              string(source.Amendment.Capability),
			Decision:                source.Amendment.Decision,
			AmendedPermissionDigest: source.Amendment.AmendedPermissionDigest,
		}
	}
	return projected
}
