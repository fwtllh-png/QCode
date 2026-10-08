package tool

import (
	"context"
	"encoding/json"
	"testing"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestTrustedBindingRejectsCrossFieldAuthorityConflicts(t *testing.T) {
	cases := []struct {
		name    string
		binding TrustedBinding
	}{
		{
			name: "read capability with write resource",
			binding: bindingFixture(
				CapabilityRead,
				ResourceTemplate{
					Kind: "file", Field: "path", Access: AccessWrite,
				},
			),
		},
		{
			name: "process resource with write capability",
			binding: bindingFixture(
				CapabilityWrite,
				ResourceTemplate{
					Kind: "process", ID: "host", Access: AccessWrite,
				},
			),
		},
		{
			name: "network resource with read capability",
			binding: bindingFixture(
				CapabilityRead,
				ResourceTemplate{
					Kind: "host", Field: "host", Access: AccessRead,
				},
			),
		},
		{
			name: "read evidence with process capability",
			binding: func() TrustedBinding {
				value := bindingFixture(CapabilityProcess)
				value.RecordsWorkspaceRead = true
				return value
			}(),
		},
		{
			name: "verification evidence with read capability",
			binding: func() TrustedBinding {
				value := bindingFixture(CapabilityRead)
				value.ProducesVerificationEvidence = true
				return value
			}(),
		},
		{
			name: "missing write parent with read capability",
			binding: func() TrustedBinding {
				value := bindingFixture(CapabilityRead)
				value.ValidateMissingWriteParent = true
				return value
			}(),
		},
		{
			name: "strong sandbox without required controls",
			binding: func() TrustedBinding {
				value := bindingFixture(CapabilityProcess)
				value.Required = RequiredControls{}
				return value
			}(),
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := test.binding.Validate(); err == nil {
				t.Fatal("conflicting trusted binding was accepted")
			}
		})
	}
}

func TestExternalRequestedEffectsDoNotOverrideTrustedBinding(t *testing.T) {
	external := ExternalDescriptor{
		Name: "external", Description: "external request",
		InputSchema: map[string]any{"type": "object"},
		Visibility:  VisibleModel, Availability: AvailabilityAvailable,
		Requested: RequestedEffects{
			Capability:         CapabilityProcess,
			AccessMode:         AccessTree,
			ParallelPolicy:     ParallelSerial,
			SandboxRequirement: SandboxNone,
		},
	}
	binding := bindingFixture(CapabilityRead)
	descriptor := external.Descriptor(binding)
	if descriptor.Capability != CapabilityRead ||
		descriptor.AccessMode != AccessRead ||
		descriptor.SandboxRequirement != SandboxStrong {
		t.Fatalf("external request widened authority: %+v", descriptor)
	}
	registration := NewExternalRegistration(
		external, binding, contractExecutor{descriptor: descriptor},
	)
	registry := NewRegistry(nil, nil)
	if err := registry.RegisterTrusted("fixture:external", registration); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := snapshot.Lookup("external")
	if !ok {
		t.Fatal("external entry is missing")
	}
	presentation := entry.PresentationDescriptor()
	if presentation.Capability != CapabilityRead ||
		presentation.SandboxRequirement != SandboxStrong ||
		entry.External.Requested.Capability != CapabilityProcess ||
		entry.External.Requested.SandboxRequirement != SandboxNone {
		t.Fatalf("snapshot mixed requested and trusted authority: %+v", entry)
	}
}

func TestIsolatedWriteBindingRequiresDeclaredStrongProcess(t *testing.T) {
	binding := bindingFixture(CapabilityProcess)
	binding.ResourceResolver.PathsField = "write_paths"
	binding.Required.ProcessTree = securitymodel.ProcessTreeGroupKill
	binding.IsolatesWriteTrees = true
	if err := binding.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*TrustedBinding){
		"read capability": func(b *TrustedBinding) { b.Capability = CapabilityRead },
		"no sandbox":      func(b *TrustedBinding) { b.SandboxRequirement = SandboxNone },
		"no write paths":  func(b *TrustedBinding) { b.ResourceResolver.PathsField = "" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := binding
			change(&invalid)
			if err := invalid.Validate(); err == nil {
				t.Fatal("invalid isolated write contract accepted")
			}
		})
	}
}

func TestRegistryRejectsLegacyExternalSourceRegistration(t *testing.T) {
	registry := NewRegistry(nil, nil)
	executor := contractExecutor{
		descriptor: contractDescriptor("external"),
	}
	if _, err := registry.Reconcile(
		"mcp:hostile", registry.Generation(),
		[]Registration{NewRegistration(executor)},
	); err == nil {
		t.Fatal("legacy external registration was accepted")
	}
}

func bindingFixture(
	capability Capability,
	resources ...ResourceTemplate,
) TrustedBinding {
	return TrustedBinding{
		Capability:       capability,
		ResourceResolver: ResourceResolver{Templates: resources},
		AccessMode:       AccessRead, ParallelPolicy: ParallelConcurrent,
		RepeatPolicy: RepeatExecute, SandboxRequirement: SandboxStrong,
		Effect: EffectContract{
			Mode: EffectDerived, WorkspaceTransaction: TransactionNone,
			Approval: ApprovalPolicyDefault,
		},
		Required: RequiredControls{
			FilesystemRead: securitymodel.FilesystemReadDeclaredRoots,
			Network:        securitymodel.NetworkDenied,
			PathIdentity:   securitymodel.PathIdentityDescriptorRelative,
		},
	}
}

type contractExecutor struct {
	descriptor Descriptor
}

func (e contractExecutor) Descriptor() Descriptor { return e.descriptor }
func (contractExecutor) Execute(
	context.Context,
	json.RawMessage,
) (Result, error) {
	return Result{}, nil
}

func contractDescriptor(name string) Descriptor {
	return Descriptor{
		Name: name, Description: name,
		InputSchema: map[string]any{"type": "object"},
		Visibility:  VisibleModel, Capability: CapabilityRead,
		AccessMode: AccessRead, ParallelPolicy: ParallelConcurrent,
		RepeatPolicy: RepeatExecute, SandboxRequirement: SandboxNone,
		Availability: AvailabilityAvailable,
	}
}
