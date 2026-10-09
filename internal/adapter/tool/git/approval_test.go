package git

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/orchestration/workspacebroker"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestGitPushApprovalModes(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	for _, test := range []struct {
		name      string
		posture   policy.Permission
		rule      policy.Action
		approvals int
		denied    bool
	}{
		{name: "full access", posture: policy.PermissionBypass},
		{name: "auto", posture: policy.PermissionAuto, approvals: 2},
		{name: "full access explicit ask", posture: policy.PermissionBypass, rule: policy.ActionAsk, approvals: 2},
		{name: "full access explicit deny", posture: policy.PermissionBypass, rule: policy.ActionDeny, denied: true},
		{name: "read only", posture: policy.PermissionNever, denied: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			runGit(t, root, "init", "-q", "-b", "main")
			runGit(t, root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "-qm", "seed")
			remote := filepath.Join(t.TempDir(), "remote.git")
			runGit(t, filepath.Dir(remote), "init", "--bare", "-q", remote)
			runGit(t, root, "remote", "add", "origin", remote)

			leases := authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{})
			broker, err := workspacebroker.New(root, leases, time.Minute, nil)
			if err != nil {
				t.Fatal(err)
			}
			registry := tool.NewRegistry(nil, nil)
			t.Cleanup(func() { _ = registry.Close() })
			if err := RegisterWithBackendAndRuntime(registry, root, gitTestBackend{}, broker); err != nil {
				t.Fatal(err)
			}
			runtime := policy.DefaultRuntime(policy.ModeAct, test.posture)
			runtime.ConfigurePlanning(policy.PlanningRequired)
			if test.rule != "" {
				runtime.User = []policy.Rule{{Tool: "git_push", Action: test.rule}}
			}
			g, err := guard.New(guard.Options{Registry: registry, Policy: runtime, Workspace: root, LeaseAuthority: leases})
			if err != nil {
				t.Fatal(err)
			}
			approvals := 0
			g.SetApprovalHandler(func(_ context.Context, request guard.ApprovalRequest) error {
				approvals++
				if approvals > test.approvals {
					return fmt.Errorf("unexpected approval under %s", test.name)
				}
				wantCode := "tool_approval_required"
				if test.rule == policy.ActionAsk {
					wantCode = "approval_required"
				}
				if request.Tool != "git_push" || request.ReasonCode != wantCode || request.ReplacementAllowed ||
					len(request.AllowedScopes) != 1 || request.AllowedScopes[0] != policy.ApprovalOnce {
					return fmt.Errorf("incorrect push approval: %+v", request)
				}
				return g.Decide(guard.ApprovalDecision{RequestID: request.RequestID, Scope: policy.ApprovalOnce, Approved: true})
			})
			for i := 1; i <= 2; i++ {
				result, err := g.Execute(t.Context(), fmt.Sprintf("push-%d", i), "git_push", json.RawMessage(`{"remote":"origin","branch":"main"}`))
				if test.denied {
					if err == nil || !(strings.Contains(err.Error(), "user_rule_denied") || strings.Contains(err.Error(), "permission_denied")) {
						t.Fatalf("denied push: result=%+v err=%v", result, err)
					}
				} else if err != nil || result.IsError {
					t.Fatalf("push: result=%+v err=%v", result, err)
				}
			}
			if approvals != test.approvals {
				t.Fatalf("approvals = %d, want %d", approvals, test.approvals)
			}
			command := exec.Command("git", "rev-parse", "--verify", "refs/heads/main")
			command.Dir = remote
			if output, err := command.CombinedOutput(); (err != nil) != test.denied {
				t.Fatalf("remote branch after pushes: %s, err=%v, denied=%t", output, err, test.denied)
			}
		})
	}
}
