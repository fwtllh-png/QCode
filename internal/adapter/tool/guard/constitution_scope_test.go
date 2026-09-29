package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestConstitutionProtectsDirectoryDescendantsAndLiteralWorkspaceNames(t *testing.T) {
	for _, name := range []string{"tree", "workspace{literal}"} {
		t.Run(name, func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(filepath.Join(workspace, "app"), 0700); err != nil {
				t.Fatal(err)
			}
			workspace, err := filepath.EvalSymlinks(workspace)
			if err != nil {
				t.Fatal(err)
			}
			runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)
			runtime.Constitution = []policy.Rule{{Tool: "*", Resource: "**/.env", RequireWrite: true, Action: policy.ActionHold, Code: "constitution_hold:repo"}}
			g, err := New(Options{Registry: tool.NewRegistry(nil, nil), Policy: runtime, Workspace: workspace})
			if err != nil {
				t.Fatal(err)
			}
			descriptor := tool.Descriptor{ResourceResolver: tool.ResourceResolver{PathsField: "write_paths"}}
			target := "app/.env"
			if name == "tree" {
				target = "app"
			}
			raw, _ := json.Marshal(map[string]any{"command": "printf changed > app/.env", "write_paths": []string{target}})
			resources, err := g.resolveResources("exec_command", descriptor, raw)
			if err != nil {
				t.Fatal(err)
			}
			invocation := resolvePolicyFixture(policyInvocationFixture{CallID: "review", Tool: "exec_command", Arguments: raw, Resources: resources, Capability: tool.CapabilityProcess, Access: tool.AccessRead, Sandbox: tool.SandboxStrong, Workspace: workspace, Validated: true})
			snapshot, err := g.samplePolicy()
			if err != nil {
				t.Fatal(err)
			}
			got := snapshot.Decide(invocation)
			t.Logf("resource=%+v canonical_rule=%q decision=%+v", resources, snapshot.Constitution[0].ResourcePath, got)
			if name == "tree" {
				exact := invocation
				resolved := exact.Assessment.Input()
				resolved.Resources = []securitymodel.Resource{{Class: securitymodel.ClassPath, Path: filepath.Join(workspace, "app/.env"), Access: tool.AccessWrite}}
				exact.Assessment = securitymodel.Assess(resolved)
				if exactDecision := snapshot.Decide(exact); exactDecision.Code != "constitution_hold:repo" {
					t.Fatalf("exact path setup did not hold: %+v", exactDecision)
				}
			}
			if got.Code != "constitution_hold:repo" {
				t.Errorf("protected .env write was not held under bypass")
			}
			runtime.SetPermission(policy.PermissionAuto)
			snapshot, err = g.samplePolicy()
			if err != nil {
				t.Fatal(err)
			}
			automatic := snapshot.Decide(invocation)
			classified := invocation.Assessment
			t.Logf("auto decision=%+v effect=%+v", automatic, classified.Effect())
			if name == "tree" {
				exact := invocation
				resolved := exact.Assessment.Input()
				resolved.Resources = []securitymodel.Resource{{Class: securitymodel.ClassPath, Path: filepath.Join(workspace, "app/.env"), Access: tool.AccessWrite}}
				exact.Assessment = securitymodel.Assess(resolved)
				if exactDecision := snapshot.Decide(exact); exactDecision.Code != "constitution_hold:repo" {
					t.Fatalf("exact path under auto did not hold: %+v", exactDecision)
				}
			}
			if automatic.Code != "constitution_hold:repo" {
				t.Errorf("protected .env write was not held under auto")
			}
		})
	}
}
