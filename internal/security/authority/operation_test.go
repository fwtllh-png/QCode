package authority

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/effect"
)

func TestExecutionOperationNormalizesResourcesAndArguments(t *testing.T) {
	root := t.TempDir()
	invocation := fixturePreparedInvocation(root)
	input := OperationInput{
		WorkspaceRoot: root, WorkspaceGeneration: 3,
		Invocation: invocation,
		Effect: effect.Effect{
			Kind: effect.ProcessReadOnly, Risk: effect.RiskLow,
			Reversibility: effect.Reversible,
		},
		Required: RequiredControls{
			FilesystemRead: controlmatrix.FilesystemReadDeclaredRoots,
			Network:        controlmatrix.NetworkDenied,
			ProcessTree:    controlmatrix.ProcessTreeGroupKill,
			PathIdentity:   controlmatrix.PathIdentityDescriptorRelative,
		},
	}
	first, err := BuildExecutionOperation(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Invocation.Arguments = json.RawMessage("{\n  \"command\": \"go test ./...\"\n}")
	input.Invocation.Resources[0], input.Invocation.Resources[1] =
		input.Invocation.Resources[1], input.Invocation.Resources[0]
	second, err := BuildExecutionOperation(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest ||
		first.Process == nil ||
		first.Process.ArgumentsDigest == "" ||
		first.Subject.Generation != invocation.Ref.Generation ||
		first.WorkspaceGeneration != 3 {
		t.Fatalf("operations first=%+v second=%+v", first, second)
	}
	if len(first.Resources) != 2 ||
		first.Resources[0].Namespace != NamespaceProcess ||
		first.Resources[1].Namespace != NamespaceWorkspace ||
		first.Resources[1].RelativePath != "report.txt" {
		t.Fatalf("resources = %+v", first.Resources)
	}
}

func TestExecutionOperationDigestRejectsMutation(t *testing.T) {
	root := t.TempDir()
	operation, err := BuildExecutionOperation(OperationInput{
		WorkspaceRoot: root, WorkspaceGeneration: 1,
		Invocation: fixturePreparedInvocation(root),
		Effect: effect.Effect{
			Kind: effect.ProcessReadOnly, Risk: effect.RiskLow,
			Reversibility: effect.Reversible,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	operation.Subject.Generation++
	if err := operation.Validate(); err == nil {
		t.Fatal("mutated operation was accepted")
	}
}

func TestExecutionOperationRejectsTraversalResource(t *testing.T) {
	root := t.TempDir()
	invocation := fixturePreparedInvocation(root)
	invocation.Resources = []tool.Resource{{
		Kind: "file", Path: filepath.Join(root, "..", "outside"),
		Access: tool.AccessRead,
	}}
	_, err := BuildExecutionOperation(OperationInput{
		WorkspaceRoot: root, WorkspaceGeneration: 1,
		Invocation: invocation,
		Effect: effect.Effect{
			Kind: effect.WorkspaceRead, Risk: effect.RiskLow,
			Reversibility: effect.Reversible,
		},
	})
	if err == nil {
		t.Fatal("workspace escape became an implicit host toolchain resource")
	}
}

func TestExecutionOperationBindsAuthorizedHostRoot(t *testing.T) {
	workspace := t.TempDir()
	toolchain := t.TempDir()
	invocation := fixturePreparedInvocation(workspace)
	invocation.Resources = []tool.Resource{{
		Kind: "file", Path: filepath.Join(toolchain, "bin", "go"),
		Access: tool.AccessRead,
	}}
	operation, err := BuildExecutionOperation(OperationInput{
		WorkspaceRoot: workspace, WorkspaceGeneration: 1,
		Invocation: invocation, HostReadRoots: []string{toolchain},
		Effect: effect.Effect{
			Kind: effect.ProcessReadOnly, Risk: effect.RiskLow,
			Reversibility: effect.Reversible,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resource := operation.Resources[0]
	if resource.Namespace != NamespaceHostToolchain ||
		resource.RelativePath != filepath.Join("bin", "go") ||
		resource.RootID == "" {
		t.Fatalf("host resource = %+v", resource)
	}
}

func TestExecutionOperationCanonicalizesNetworkTarget(t *testing.T) {
	root := t.TempDir()
	invocation := fixturePreparedInvocation(root)
	invocation.Resources = []tool.Resource{{
		Kind: "url", ID: "HTTPS://API.Example.COM/v1/items",
		Access: tool.AccessRead, Methods: []string{"get", "GET"},
	}}
	operation, err := BuildExecutionOperation(OperationInput{
		WorkspaceRoot: root, WorkspaceGeneration: 1,
		Invocation: invocation,
		Effect: effect.Effect{
			Kind: effect.NetworkRead, Risk: effect.RiskMedium,
			Reversibility: effect.Bounded,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resource := operation.Resources[0]
	if resource.ID != "https://api.example.com:443" ||
		resource.Protocol != "https" ||
		resource.Port != 443 ||
		len(resource.Methods) != 1 ||
		resource.Methods[0] != "GET" {
		t.Fatalf("network resource = %+v", resource)
	}
}

func fixturePreparedInvocation(root string) tool.PreparedInvocation {
	invocation := tool.PreparedInvocation{
		CallID: "call-1", Tool: "exec_command",
		Ref: tool.ToolRef{
			Name: "exec_command", Source: "builtin:exec_command",
			CatalogID: "catalog-1", Generation: 2, Revision: 3, Authority: 4,
		},
		Arguments: json.RawMessage(`{"command":"go test ./..."}`),
		Resources: []tool.Resource{
			{
				Kind: "file", Path: filepath.Join(root, "report.txt"),
				Access: tool.AccessRead,
			},
			{Kind: "process", ID: "workspace", Access: tool.AccessWrite},
		},
		Descriptor: tool.Descriptor{
			Name: "exec_command", Capability: tool.CapabilityProcess,
			AccessMode: tool.AccessRead, SandboxRequirement: tool.SandboxStrong,
		},
		Source: tool.InvocationSourceModel,
	}
	invocation.Binding = tool.TrustedBindingFromDescriptor(
		invocation.Descriptor,
	)
	return invocation
}
