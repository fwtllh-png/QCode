package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	appextension "github.com/fwtllh-png/QCode/internal/runtime/app/extension"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestToolExecutionReceiptProjectsIntoDurableToolResult(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	digest := strings.Repeat("a", 64)
	source := &tool.ExecutionReceipt{
		Tool: tool.ToolRef{
			Name: "exec_command", Source: "builtin:exec_command",
			CatalogID: "catalog-1", Generation: 2, Revision: 3, Authority: 4,
		},
		Source:      tool.InvocationSourceModel,
		Disposition: tool.DispositionWaitForTeardown,
		Attempts: []tool.AttemptReceipt{{
			Sequence: 1, Sandbox: "strong", Status: tool.OutcomeRejected,
			TerminalOwner:          tool.TerminalOwnerGuard,
			OperationSchemaVersion: 1, OperationDigest: digest,
			LeaseID: "lease-1", LeaseState: "settled", LeaseAttempt: 1,
			WorkspaceID: digest, WorkspaceGeneration: 2,
			SubjectKind: "builtin", SubjectID: "builtin:exec_command",
			SubjectDigest: digest, SubjectGeneration: 3,
			PolicyRevision: 7, SandboxPolicyID: "sandbox-policy",
			Policy: &tool.PolicyDecisionReceipt{
				Action: "ask", Layer: "binding", Code: "host_process_approval_required",
			},
			EffectKind: "process.read_only", EffectRisk: "low",
			EffectReversibility:     "reversible",
			WorkspaceTransaction:    "none",
			PermissionSchemaVersion: 2, PermissionRevision: 7,
			PermissionDigest: digest, PermissionCapability: tool.CapabilityProcess,
			PermissionAccess: tool.AccessRead, Enforcement: "strong",
			Backend:       "seatbelt",
			WorkspaceRoot: "/workspace", ReadRoots: []string{"/workspace"},
			WritePaths:  []string{"/workspace/result.txt"},
			NetworkMode: "proxy_targets", LoopbackAllowed: true, ProcessAllowed: true,
			Provenance: []tool.PermissionProvenance{{
				Kind: "managed", Value: "grant", Digest: digest, Revision: 7,
			}},
			Denial: &sandbox.Denial{
				Backend: "seatbelt", Operation: sandbox.DenialWrite,
				Resource:   "/workspace/result.txt",
				ReasonCode: sandbox.ReasonPathWriteNotAuthorized,
			},
			Amendment: &tool.PermissionAmendmentReceipt{
				BasePermissionDigest: digest, Kind: "path_write",
				Resource: "/workspace/result.txt", Decision: "denied",
			},
			StartedAt: now, CompletedAt: now.Add(time.Millisecond),
			DurationMS: 1,
		}},
		TerminalStatus: tool.OutcomeRejected,
		TerminalOwner:  tool.TerminalOwnerGuard,
	}
	projected := appextension.ProjectToolExecutionReceipt(source)
	source.Attempts[0].ReadRoots[0] = "/tampered"
	source.Attempts[0].Denial.Resource = "/tampered"
	source.Attempts[0].Policy.Layer = "tampered"
	if projected == nil || projected.Tool.Name != "exec_command" ||
		projected.Attempts[0].Policy == nil ||
		*projected.Attempts[0].Policy != (protocol.ToolPolicyDecision{
			Action: "ask", Layer: "binding", Code: "host_process_approval_required",
		}) ||
		projected.TerminalOwner != "guard" ||
		len(projected.Attempts) != 1 ||
		projected.Attempts[0].PermissionDigest != digest ||
		projected.Attempts[0].OperationDigest != digest ||
		projected.Attempts[0].LeaseState != "settled" ||
		projected.Attempts[0].ReadRoots[0] != "/workspace" ||
		!projected.Attempts[0].LoopbackAllowed ||
		projected.Attempts[0].Denial.Resource != "/workspace/result.txt" {
		t.Fatalf("projected receipt = %+v", projected)
	}
	event, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 1, OperationID: "operation", ThreadID: "thread", TurnID: "turn",
		ItemID: "item",
	}, &protocol.ToolResultData{
		Tool: "exec_command", CallID: "call", Output: "denied",
		IsError: true, Execution: projected,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded protocol.Event
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	result, ok := decoded.Data.(*protocol.ToolResultData)
	if !ok || result.Execution == nil ||
		result.Execution.Attempts[0].PermissionDigest != digest ||
		result.Execution.Attempts[0].OperationDigest != digest ||
		result.Execution.Attempts[0].LeaseID != "lease-1" ||
		result.Execution.Attempts[0].Policy == nil ||
		result.Execution.Attempts[0].Policy.Layer != "binding" ||
		!result.Execution.Attempts[0].LoopbackAllowed {
		t.Fatalf("decoded result = %#v", decoded.Data)
	}
}

func TestToolExecutionReceiptProjectsPolicyDenial(t *testing.T) {
	source := &tool.ExecutionReceipt{
		Tool: tool.ToolRef{
			Name: "file_write", Source: "builtin:file_write",
			CatalogID: "catalog-1", Generation: 1, Revision: 1,
		},
		Source:      tool.InvocationSourceModel,
		Disposition: tool.DispositionWaitForTeardown,
		PolicyDenial: &tool.PolicyDecisionReceipt{
			Action: "deny", Layer: "hard_constraint", Code: "control_plane_protected",
		},
		TerminalStatus: tool.OutcomeRejected,
		TerminalOwner:  tool.TerminalOwnerGuard,
	}
	projected := appextension.ProjectToolExecutionReceipt(source)
	source.PolicyDenial.Code = "tampered"
	if projected.PolicyDenial == nil || *projected.PolicyDenial != (protocol.ToolPolicyDecision{
		Action: "deny", Layer: "hard_constraint", Code: "control_plane_protected",
	}) || len(projected.Attempts) != 0 {
		t.Fatalf("projected receipt = %+v", projected)
	}
	_, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 1, OperationID: "operation", ThreadID: "thread", TurnID: "turn",
		ItemID: "item",
	}, &protocol.ToolResultData{
		Tool: "file_write", CallID: "call", Output: "denied",
		IsError: true, Execution: &protocol.ToolExecutionReceipt{
			Tool: projected.Tool, Source: projected.Source,
			Disposition:    projected.Disposition,
			PolicyDenial:   &protocol.ToolPolicyDecision{Action: "deny"},
			TerminalStatus: projected.TerminalStatus,
			TerminalOwner:  projected.TerminalOwner,
		},
	})
	if err == nil {
		t.Fatal("NewEvent() accepted a policy denial without its decision layer")
	}
}
