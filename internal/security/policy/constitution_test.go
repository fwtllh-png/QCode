package policy_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitypolicy "github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestConstitutionHoldSurvivesBypass(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	repoPath := filepath.Join(workspace, ".qcode", "constitution.json")
	if err := os.MkdirAll(filepath.Dir(repoPath), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := securitypolicy.ConstitutionDocument{
		Version: 1, DenyWriteGlobs: []string{"secrets/"},
		Prompt: "do not write secrets",
	}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(repoPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := securitypolicy.LoadConstitution(workspace, home)
	if err != nil {
		t.Fatal(err)
	}
	if !bundle.Status.Loaded || bundle.Status.RuleCount == 0 {
		t.Fatalf("status = %+v", bundle.Status)
	}
	runtime := securitypolicy.DefaultRuntime(securitypolicy.ModeAct, securitypolicy.PermissionBypass)
	runtime.Constitution = bundle.Rules
	decision := runtime.Decide(writeInvocation("secrets/token"))
	if decision.Action != securitypolicy.ActionDeny || decision.Layer != securitypolicy.LayerHard ||
		!strings.Contains(decision.Code, "constitution_hold") {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestConstitutionWriteHoldCoversEveryWriterTool(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	writeDoc(t, filepath.Join(workspace, ".qcode", "constitution.json"), securitypolicy.ConstitutionDocument{
		Version: 1, DenyWriteGlobs: []string{"secrets/"},
	})
	bundle, err := securitypolicy.LoadConstitution(workspace, home)
	if err != nil {
		t.Fatal(err)
	}
	runtime := securitypolicy.DefaultRuntime(securitypolicy.ModeAct, securitypolicy.PermissionBypass)
	runtime.Constitution = bundle.Rules
	secretWrite := tool.Resource{
		Kind: "file", Path: "secrets/token", Access: tool.AccessWrite,
	}
	tests := []struct {
		name       string
		tool       string
		capability securitypolicy.Capability
		resources  []tool.Resource
		held       bool
	}{
		{
			name: "file_apply transaction write", tool: "file_apply",
			capability: securitypolicy.CapabilityWrite,
			resources:  []tool.Resource{secretWrite}, held: true,
		},
		{
			name: "integrate_agent expanded merge write", tool: "integrate_agent",
			capability: securitypolicy.CapabilityWrite,
			resources: []tool.Resource{
				{Kind: "agent", ID: "agent-1", Access: tool.AccessWrite},
				secretWrite,
			},
			held: true,
		},
		{
			name: "process declared write path", tool: "run_command",
			capability: securitypolicy.CapabilityProcess,
			resources: []tool.Resource{
				{Kind: "repo", ID: ".", Access: tool.AccessRead, Tree: true},
				secretWrite,
			},
			held: true,
		},
		{
			name: "tree-wide write into the protected tree", tool: "file_patch",
			capability: securitypolicy.CapabilityWrite,
			resources: []tool.Resource{
				{Kind: "file", Path: "secrets/token", Access: tool.AccessTree},
			},
			held: true,
		},
		{
			name: "file_read of a protected path", tool: "file_read",
			capability: securitypolicy.CapabilityRead,
			resources: []tool.Resource{
				{Kind: "file", Path: "secrets/token", Access: tool.AccessRead},
			},
			held: false,
		},
		{
			name: "process without a protected write path", tool: "run_command",
			capability: securitypolicy.CapabilityProcess,
			resources: []tool.Resource{
				{Kind: "repo", ID: ".", Access: tool.AccessRead, Tree: true},
			},
			held: false,
		},
		{
			name: "write outside the protected tree", tool: "file_write",
			capability: securitypolicy.CapabilityWrite,
			resources: []tool.Resource{
				{Kind: "file", Path: "src/main.go", Access: tool.AccessWrite},
			},
			held: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := runtime.Decide(resolvePolicyFixture(policyInvocationFixture{
				CallID: "c-" + test.name, Tool: test.tool,
				Capability: test.capability, Validated: true,
				Workspace: "/workspace", Arguments: json.RawMessage(`{}`),
				Resources: test.resources,
			}))
			if test.held && (decision.Action != securitypolicy.ActionDeny ||
				!strings.Contains(decision.Code, "constitution_hold")) {
				t.Fatalf("held decision = %+v", decision)
			}
			if !test.held && strings.Contains(decision.Code, "constitution_hold") {
				t.Fatalf("unexpected hold decision = %+v", decision)
			}
		})
	}
}

func TestConstitutionGlobsHoldMatchingWrites(t *testing.T) {
	workspace := t.TempDir()
	writeDoc(t, filepath.Join(workspace, ".qcode", "constitution.json"), securitypolicy.ConstitutionDocument{
		Version: 1, DenyWriteGlobs: []string{"*.pem", "**/.env", "config/*.key", "secrets/[ab]", "a?b"},
	})
	bundle, err := securitypolicy.LoadConstitution(workspace, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runtime := securitypolicy.DefaultRuntime(securitypolicy.ModeAct, securitypolicy.PermissionBypass)
	runtime.Constitution = bundle.Rules
	for path, held := range map[string]bool{
		"server.pem": true, "certs/server.pem": false,
		".env": true, "app/.env": true, "app/.envrc": false,
		"config/db.key": true, "config/nested/db.key": false,
		"secrets/a": true, "secrets/c": false,
		"axb": true, "axxb": false,
	} {
		decision := runtime.Decide(writeInvocation(path))
		if got := strings.Contains(decision.Code, "constitution_hold"); got != held {
			t.Errorf("write %q: decision = %+v, held = %t", path, decision, held)
		}
	}
}

func TestConstitutionRejectsUnsupportedWildcards(t *testing.T) {
	for _, glob := range []string{"{x,y}", "src/a**b", "secrets/[ab", "**x/y"} {
		t.Run(glob, func(t *testing.T) {
			workspace := t.TempDir()
			writeDoc(t, filepath.Join(workspace, ".qcode", "constitution.json"), securitypolicy.ConstitutionDocument{
				Version: 1, DenyWriteGlobs: []string{"secrets/", glob},
			})
			_, err := securitypolicy.LoadConstitution(workspace, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), glob) {
				t.Fatalf("Load error = %v, want rejection naming %q", err, glob)
			}
		})
	}
}

func TestConstitutionDirectorySuffixesProtectSubtree(t *testing.T) {
	for _, glob := range []string{"secrets", "secrets/", "secrets/*", "secrets/**", "./secrets/"} {
		t.Run(glob, func(t *testing.T) {
			workspace := t.TempDir()
			writeDoc(t, filepath.Join(workspace, ".qcode", "constitution.json"), securitypolicy.ConstitutionDocument{
				Version: 1, DenyWriteGlobs: []string{glob, "  "},
			})
			bundle, err := securitypolicy.LoadConstitution(workspace, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if len(bundle.Rules) != 1 || bundle.Rules[0].Resource != "secrets" ||
				!bundle.Rules[0].RequireWrite {
				t.Fatalf("rules = %+v", bundle.Rules)
			}
		})
	}
}

func TestRepoOverridesUserPrompt(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	writeDoc(t, filepath.Join(home, ".qcode", "constitution.json"), securitypolicy.ConstitutionDocument{
		Version: 1, Prompt: "user prompt", HoldTools: []string{"run_command"},
	})
	writeDoc(t, filepath.Join(workspace, ".qcode", "constitution.json"), securitypolicy.ConstitutionDocument{
		Version: 1, Prompt: "repo prompt",
	})
	bundle, err := securitypolicy.LoadConstitution(workspace, home)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Prompt != "repo prompt" {
		t.Fatalf("prompt = %q", bundle.Prompt)
	}
}

func TestWriteTemplateIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "constitution.json")
	if err := securitypolicy.WriteConstitutionTemplate(path, false); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if err := securitypolicy.WriteConstitutionTemplate(path, false); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatal("template rewritten without force")
	}
	var doc securitypolicy.ConstitutionDocument
	if err := json.Unmarshal(first, &doc); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(doc.DenyWriteGlobs, "**/.env") {
		t.Fatalf("template globs = %v, want nested .env protection", doc.DenyWriteGlobs)
	}
}

func writeInvocation(path string) securitypolicy.Invocation {
	return resolvePolicyFixture(policyInvocationFixture{
		CallID: "c-" + path, Tool: "file_write", Capability: securitypolicy.CapabilityWrite, Validated: true,
		Workspace: "/workspace", Arguments: json.RawMessage(`{}`),
		Resources: []tool.Resource{{Kind: "file", Path: path, Access: tool.AccessWrite}},
	})
}

func writeDoc(t *testing.T, path string, doc securitypolicy.ConstitutionDocument) {
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
