package shell

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestGuardianRecoveryRestoresOnlyMatchingHumanWait(t *testing.T) {
	for _, outcome := range []string{"approve", "deny", "cancel", "changed_arguments", "changed_identity", "revoked"} {
		t.Run(outcome, func(t *testing.T) {
			root, first, registry, ctx, arguments := guardianShellRegistryFixture(t)
			dataDir := t.TempDir()
			store, err := state.Open(t.Context(), state.Options{DataDir: dataDir})
			if err != nil {
				t.Fatal(err)
			}
			reviewer := newPipelineReviewer(root)
			reviewer.prompt = true
			stopped := errors.New("fixture process stopped while pending")
			first.SetApprovalHandler(func(_ context.Context, request toolguard.ApprovalRequest) error {
				// Exercise the public approval projection's round trip: internal
				// grant details are intentionally not present after deserialization.
				body, _ := json.Marshal(request)
				var data protocol.ApprovalRequiredData
				if err := json.Unmarshal(body, &data); err != nil {
					return err
				}
				if request.Grant != nil {
					data.GrantPreview = &protocol.ApprovalGrantPreview{Kind: request.Grant.Kind, Key: request.Grant.Key, Summary: request.Grant.Summary}
				}
				event, err := protocol.NewEvent(protocol.EventMeta{Sequence: 1, OperationID: "op", ThreadID: "thread", TurnID: "turn", ItemID: "approval"}, &data)
				if err != nil {
					return err
				}
				if err := store.Append(t.Context(), event); err != nil {
					return err
				}
				return stopped
			})
			if _, err := first.Execute(pipelineContext(ctx, reviewer), "call", "exec_command", arguments); !errors.Is(err, stopped) {
				t.Fatalf("capture approval: %v", err)
			}
			if err := store.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			store, err = state.Open(t.Context(), state.Options{DataDir: dataDir})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close(context.Background())
			events, err := store.Replay(t.Context(), 0)
			if err != nil || len(events) != 1 {
				t.Fatalf("missing persisted approval: %v %v", events, err)
			}
			body, _ := json.Marshal(events[0].Data)
			var saved toolguard.ApprovalRequest
			if err := json.Unmarshal(body, &saved); err != nil {
				t.Fatal(err)
			}
			current := policy.DefaultRuntime(policy.ModeAct, policy.PermissionAuto)
			if outcome == "revoked" {
				current.SetPermission(policy.PermissionNever)
			}
			restarted, err := toolguard.New(toolguard.Options{Registry: registry, Workspace: root, Policy: current,
				Isolator: newShellIsolator(t, root, registry.SandboxBackend()),
				Approvals: func(context.Context, toolguard.ApprovalRequest) error {
					t.Error("duplicate approval emitted")
					return errors.New("unexpected approval")
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.RestoreApproval(saved); err != nil {
				t.Fatal(err)
			}
			restores := 0
			restarted.SetApprovalRecoveryHandler(func(request toolguard.ApprovalRequest) error {
				restores++
				if request.RequestID != saved.RequestID || request.GuardianReviewID != saved.GuardianReviewID {
					t.Error("approval identity changed")
				}
				return restarted.Decide(toolguard.ApprovalDecision{RequestID: request.RequestID, Approved: outcome == "approve", Canceled: outcome == "cancel", Scope: policy.ApprovalOnce})
			})
			if outcome == "changed_arguments" {
				var changed map[string]any
				_ = json.Unmarshal(arguments, &changed)
				changed["command"] = "printf changed > generated/out.txt"
				arguments, _ = json.Marshal(changed)
			}
			nextReviewer := newPipelineReviewer(root)
			if outcome == "changed_identity" {
				identity := tool.InvocationIdentityFrom(ctx)
				identity.TurnID = "another-turn"
				ctx = tool.WithInvocationIdentity(ctx, identity)
			}
			result, executeErr := restarted.Execute(pipelineContext(ctx, nextReviewer), "call", "exec_command", arguments)
			if nextReviewer.reviews.Load() != 0 {
				t.Fatal("recovery resubmitted the old call to Guardian")
			}
			_, statErr := os.Stat(filepath.Join(root, "generated/out.txt"))
			if outcome == "approve" {
				if executeErr != nil || result.IsError || statErr != nil {
					t.Fatalf("restored approval failed: %+v %v %v", result, executeErr, statErr)
				}
				if result.Execution == nil || result.Execution.GuardianReviewID != saved.GuardianReviewID {
					t.Fatal("restored execution lost review reference")
				}
			} else if !os.IsNotExist(statErr) {
				t.Fatal("recovery executed an unapproved operation")
			}
			if (outcome == "changed_arguments" || outcome == "changed_identity") && (executeErr == nil || restores != 0) {
				t.Fatal("changed operation used the old approval")
			}
		})
	}
}
