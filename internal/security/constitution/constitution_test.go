package constitution_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/constitution"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestConstitutionHoldSurvivesBypass(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	repoPath := filepath.Join(workspace, ".qcode", "constitution.json")
	if err := os.MkdirAll(filepath.Dir(repoPath), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := constitution.Document{
		Version: 1, DenyWriteGlobs: []string{"secrets/"},
		Prompt: "do not write secrets",
	}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(repoPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := constitution.Load(workspace, home)
	if err != nil {
		t.Fatal(err)
	}
	if !bundle.Status.Loaded || bundle.Status.RuleCount == 0 {
		t.Fatalf("status = %+v", bundle.Status)
	}
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)
	runtime.Repository = bundle.Rules
	decision := runtime.Evaluate(policy.Invocation{
		CallID: "c1", Tool: "file_write", Capability: policy.CapabilityWrite, Validated: true,
		Arguments: json.RawMessage(`{"path":"secrets/token"}`),
		Resources: []tool.Resource{{
			Kind: "file", Path: "secrets/token", Access: tool.AccessWrite,
		}},
	})
	if decision.Action != policy.ActionDeny ||
		!strings.Contains(decision.Code, "constitution_hold") {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestConstitutionWriteHoldCoversEveryWriterTool(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	writeDoc(t, filepath.Join(workspace, ".qcode", "constitution.json"), constitution.Document{
		Version: 1, DenyWriteGlobs: []string{"secrets/"},
	})
	bundle, err := constitution.Load(workspace, home)
	if err != nil {
		t.Fatal(err)
	}
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)
	runtime.Repository = bundle.Rules
	secretWrite := tool.Resource{
		Kind: "file", Path: "secrets/token", Access: tool.AccessWrite,
	}
	tests := []struct {
		name       string
		tool       string
		capability policy.Capability
		resources  []tool.Resource
		held       bool
	}{
		{
			name: "file_apply transaction write", tool: "file_apply",
			capability: policy.CapabilityWrite,
			resources:  []tool.Resource{secretWrite}, held: true,
		},
		{
			name: "integrate_agent expanded merge write", tool: "integrate_agent",
			capability: policy.CapabilityWrite,
			resources: []tool.Resource{
				{Kind: "agent", ID: "agent-1", Access: tool.AccessWrite},
				secretWrite,
			},
			held: true,
		},
		{
			name: "exec_command declared write path", tool: "exec_command",
			capability: policy.CapabilityProcess,
			resources: []tool.Resource{
				{Kind: "repo", ID: ".", Access: tool.AccessRead, Tree: true},
				secretWrite,
			},
			held: true,
		},
		{
			name: "file_read of a protected path", tool: "file_read",
			capability: policy.CapabilityRead,
			resources: []tool.Resource{
				{Kind: "file", Path: "secrets/token", Access: tool.AccessRead},
			},
			held: false,
		},
		{
			name: "exec_command without a protected write path", tool: "exec_command",
			capability: policy.CapabilityProcess,
			resources: []tool.Resource{
				{Kind: "repo", ID: ".", Access: tool.AccessRead, Tree: true},
			},
			held: false,
		},
		{
			name: "write outside the protected tree", tool: "file_write",
			capability: policy.CapabilityWrite,
			resources: []tool.Resource{
				{Kind: "file", Path: "src/main.go", Access: tool.AccessWrite},
			},
			held: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := runtime.Evaluate(policy.Invocation{
				CallID: "c-" + test.name, Tool: test.tool,
				Capability: test.capability, Validated: true,
				Arguments: json.RawMessage(`{}`),
				Resources: test.resources,
			})
			if test.held && (decision.Action != policy.ActionDeny ||
				!strings.Contains(decision.Code, "constitution_hold")) {
				t.Fatalf("held decision = %+v", decision)
			}
			if !test.held && strings.Contains(decision.Code, "constitution_hold") {
				t.Fatalf("unexpected hold decision = %+v", decision)
			}
		})
	}
}

func TestRepoOverridesUserPrompt(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	writeDoc(t, filepath.Join(home, ".qcode", "constitution.json"), constitution.Document{
		Version: 1, Prompt: "user prompt", HoldTools: []string{"exec_command"},
	})
	writeDoc(t, filepath.Join(workspace, ".qcode", "constitution.json"), constitution.Document{
		Version: 1, Prompt: "repo prompt",
	})
	bundle, err := constitution.Load(workspace, home)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Prompt != "repo prompt" {
		t.Fatalf("prompt = %q", bundle.Prompt)
	}
}

func TestWriteTemplateIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "constitution.json")
	if err := constitution.WriteTemplate(path, false); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if err := constitution.WriteTemplate(path, false); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatal("template rewritten without force")
	}
}

func writeDoc(t *testing.T, path string, doc constitution.Document) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
