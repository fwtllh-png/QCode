package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	completiontool "github.com/fwtllh-png/QCode/internal/adapter/tool/completion"
	filetool "github.com/fwtllh-png/QCode/internal/adapter/tool/file"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type noChangeProvider struct {
	calls []provider.ToolCall
	index int
}

func (p *noChangeProvider) Stream(context.Context, provider.ModelRequest) (provider.Stream, error) {
	if p.index >= len(p.calls) {
		return &providerfixture.SliceStream{Events: []provider.StreamEvent{{Type: provider.EventTextDelta, Text: "No changes remain."}, {Type: provider.EventMessageStop}}}, nil
	}
	call := p.calls[p.index]
	p.index++
	return &providerfixture.SliceStream{Events: []provider.StreamEvent{
		{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{ID: call.ID, Name: call.Name, Arguments: call.Arguments}},
		{Type: provider.EventMessageStop},
	}}, nil
}

func TestWorkspaceChangeCanCompleteUnchanged(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_edit_needed", true: "edit_restored"}[restore], func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "value.txt"), []byte("already correct\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			registry := tool.NewRegistry(nil, nil)
			files, err := filetool.NewWithBackend(root, noChangeSandboxBackend{})
			if err != nil {
				t.Fatal(err)
			}
			if err := files.Register(registry); err != nil {
				t.Fatal(err)
			}
			if err := completiontool.Register(registry); err != nil {
				t.Fatal(err)
			}
			runtime := &noChangeProvider{calls: []provider.ToolCall{
				{ID: "read", Name: "file_read", Arguments: `{"path":"value.txt"}`},
				{ID: "complete", Name: "turn_complete", Arguments: `{"status":"complete","summary":"Already correct.","pending_actions":[],"no_change_reason":"The requested content is already present.","no_change_evidence":["read"]}`},
			}}
			if restore {
				runtime.calls = []provider.ToolCall{
					{ID: "read", Name: "file_read", Arguments: `{"path":"value.txt"}`},
					{ID: "edit", Name: "file_edit", Arguments: `{"path":"value.txt","old":"already correct","new":"temporary"}`},
					{ID: "read-edited", Name: "file_read", Arguments: `{"path":"value.txt"}`},
					{ID: "restore", Name: "file_edit", Arguments: `{"path":"value.txt","old":"temporary","new":"already correct"}`},
				}
			}
			worker, err := newTestAgentEngine(agentengine.Options{
				ProviderConfig: agentengine.ProviderConfig{Provider: runtime, Route: runtimeTestRoute(t), MaxOutputTokens: 128},
				ToolConfig:     agentengine.ToolConfig{Tools: registry},
				SecurityConfig: agentengine.SecurityConfig{Workspace: root, Journal: newTestWorkspaceJournal(t, root), Security: policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)},
			})
			if err != nil {
				t.Fatal(err)
			}
			receipt, terminal := runWorkspaceChangeTurn(t, worker)
			completed, ok := terminal.Data.(*protocol.TurnCompletedData)
			if !ok || completed.Outcome != protocol.TurnOutcomeUnchanged || receipt.Outcome != protocol.TurnOutcomeUnchanged || len(receipt.Changes) != 0 || receipt.WorkspaceOutcome.Status != "unchanged" {
				t.Fatalf("receipt=%+v terminal=%+v", receipt, terminal)
			}
			if receipt.Verification != (protocol.ReceiptVerification{}) {
				t.Fatalf("verification=%+v", receipt.Verification)
			}
			if !restore && (receipt.Completion == nil || receipt.Completion.NoChangeReason == "" || len(receipt.Completion.NoChangeEvidence) != 1) {
				t.Fatalf("completion evidence lost: %+v", receipt.Completion)
			}
		})
	}
}

type noChangeSandboxBackend struct{}

func (b noChangeSandboxBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "fixture", Backend: "fixture",
		Available: true,
		Effective: securitymodel.Controls{FilesystemRead: securitymodel.FilesystemReadDeclaredRoots,

			FilesystemWrite: securitymodel.FilesystemWriteExactPaths,

			Network: securitymodel.NetworkDenied,

			ProcessTree: securitymodel.ProcessTreeGroupKill,

			CrossProcess: securitymodel.CrossProcessUnrestricted,
			Syscall:      securitymodel.SyscallDenyDangerous, IPC: securitymodel.IPCUnrestricted, PathIdentity: securitymodel.PathIdentityDescriptorRelative,
			ArtifactOrigin: securitymodel.ArtifactOriginUnverifiedPath, DurableRecovery: securitymodel.DurableRecoveryMemoryOnly},
	}
}

func (b noChangeSandboxBackend) Prepare(_ context.Context, command sandbox.Command) (sandbox.Command, error) {
	command.PreparedPolicyID = "engine-fixture"
	return command, nil
}
