package tool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/effect"
)

type (
	EffectKind    = effect.Kind
	RiskLevel     = effect.Risk
	Reversibility = effect.Reversibility
)

const (
	EffectWorkspaceRead    = effect.WorkspaceRead
	EffectWorkspaceEdit    = effect.WorkspaceEdit
	EffectProcessReadOnly  = effect.ProcessReadOnly
	EffectProcessMutating  = effect.ProcessMutating
	EffectNetworkRead      = effect.NetworkRead
	EffectNetworkMutating  = effect.NetworkMutating
	EffectSessionMutation  = effect.SessionMutation
	EffectAgentMessage     = effect.AgentMessage
	EffectAgentLifecycle   = effect.AgentLifecycle
	EffectExternalMutation = effect.ExternalMutation

	RiskLow      = effect.RiskLow
	RiskMedium   = effect.RiskMedium
	RiskHigh     = effect.RiskHigh
	RiskCritical = effect.RiskCritical

	Reversible   = effect.Reversible
	Bounded      = effect.Bounded
	Irreversible = effect.Irreversible
)

type EffectMode string

const (
	EffectDerived EffectMode = "resource_derived"
	EffectFixed   EffectMode = "fixed"
)

type WorkspaceTransaction string

const (
	TransactionNone        WorkspaceTransaction = "none"
	TransactionBeforeImage WorkspaceTransaction = "before_image"
	TransactionBrokerOwned WorkspaceTransaction = "broker_owned"
)

type ApprovalMode string

const (
	ApprovalPolicyDefault ApprovalMode = "default"
	ApprovalPolicyOnce    ApprovalMode = "once_required"
)

type EffectContract struct {
	Mode                   EffectMode           `json:"mode"`
	Kind                   EffectKind           `json:"kind,omitempty"`
	Risk                   RiskLevel            `json:"risk,omitempty"`
	Reversibility          Reversibility        `json:"reversibility,omitempty"`
	WorkspaceTransaction   WorkspaceTransaction `json:"workspace_transaction"`
	RequireReadBeforeWrite bool                 `json:"require_read_before_write,omitempty"`
	Approval               ApprovalMode         `json:"approval"`
}

type RequiredControls = controlmatrix.Requirements

// ExternalDescriptor is the untrusted, model-visible portion of a tool
// declaration. Requested never grants authority; it is retained for review and
// diagnostics only.
type ExternalDescriptor struct {
	Name           string           `json:"name"`
	Description    string           `json:"description"`
	DiscoveryTerms []string         `json:"discovery_terms,omitempty"`
	InputSchema    map[string]any   `json:"input_schema"`
	Visibility     Visibility       `json:"visibility"`
	Aliases        []Alias          `json:"aliases,omitempty"`
	Deferred       DeferredLoading  `json:"deferred_loading"`
	Availability   Availability     `json:"availability"`
	Unavailable    string           `json:"unavailable_reason,omitempty"`
	Requested      RequestedEffects `json:"requested_effects"`
}

type RequestedEffects struct {
	Capability         Capability         `json:"capability"`
	ResourceResolver   ResourceResolver   `json:"resource_resolver"`
	AccessMode         AccessMode         `json:"access_mode"`
	ParallelPolicy     ParallelPolicy     `json:"parallel_policy"`
	RepeatPolicy       RepeatPolicy       `json:"repeat_policy,omitempty"`
	SandboxRequirement SandboxRequirement `json:"sandbox_requirement"`
}

// TrustedBinding is the Registry-owned authority contract. Guard, Policy and
// Authority consume this value instead of trusting the presentation descriptor.
type TrustedBinding struct {
	Capability                   Capability         `json:"capability"`
	ResourceResolver             ResourceResolver   `json:"resource_resolver"`
	AccessMode                   AccessMode         `json:"access_mode"`
	ParallelPolicy               ParallelPolicy     `json:"parallel_policy"`
	RepeatPolicy                 RepeatPolicy       `json:"repeat_policy,omitempty"`
	SandboxRequirement           SandboxRequirement `json:"sandbox_requirement"`
	Effect                       EffectContract     `json:"effect"`
	Required                     RequiredControls   `json:"required_controls"`
	RecordsWorkspaceRead         bool               `json:"records_workspace_read,omitempty"`
	ProducesVerificationEvidence bool               `json:"produces_verification_evidence,omitempty"`
	ValidateMissingWriteParent   bool               `json:"validate_missing_write_parent,omitempty"`
}

type TrustedBindingProvider interface {
	TrustedBinding() TrustedBinding
}

func ExternalFromDescriptor(descriptor Descriptor) ExternalDescriptor {
	return ExternalDescriptor{
		Name: descriptor.Name, Description: descriptor.Description,
		DiscoveryTerms: append([]string(nil), descriptor.DiscoveryTerms...),
		InputSchema:    cloneStringMap(descriptor.InputSchema),
		Visibility:     descriptor.Visibility,
		Aliases:        append([]Alias(nil), descriptor.Aliases...),
		Deferred:       descriptor.DeferredLoading,
		Availability:   descriptor.Availability,
		Unavailable:    descriptor.UnavailableReason,
		Requested: RequestedEffects{
			Capability:         descriptor.Capability,
			ResourceResolver:   cloneResourceResolver(descriptor.ResourceResolver),
			AccessMode:         descriptor.AccessMode,
			ParallelPolicy:     descriptor.ParallelPolicy,
			RepeatPolicy:       descriptor.RepeatPolicy,
			SandboxRequirement: descriptor.SandboxRequirement,
		},
	}
}

func TrustedBindingFromDescriptor(descriptor Descriptor) TrustedBinding {
	binding := TrustedBinding{
		Capability:         descriptor.Capability,
		ResourceResolver:   cloneResourceResolver(descriptor.ResourceResolver),
		AccessMode:         descriptor.AccessMode,
		ParallelPolicy:     descriptor.ParallelPolicy,
		RepeatPolicy:       descriptor.RepeatPolicy,
		SandboxRequirement: descriptor.SandboxRequirement,
		Effect: EffectContract{
			Mode: EffectDerived, WorkspaceTransaction: TransactionNone,
			Approval: ApprovalPolicyDefault,
		},
	}
	if descriptor.SandboxRequirement == SandboxStrong {
		binding.Required.FilesystemRead = controlmatrix.FilesystemReadDeclaredRoots
		binding.Required.Network = controlmatrix.NetworkDirect
		binding.Required.PathIdentity = controlmatrix.PathIdentityDescriptorRelative
		if descriptor.Capability == CapabilityProcess ||
			descriptor.Capability == CapabilityExternal {
			binding.Required.ProcessTree = controlmatrix.ProcessTreeGroupKill
		}
		for _, resource := range descriptor.ResourceResolver.Templates {
			if resource.Access == AccessWrite &&
				(resource.Kind == "file" || resource.Kind == "directory" ||
					resource.Kind == "repo" || resource.Kind == "workspace") {
				binding.Required.FilesystemWrite = controlmatrix.FilesystemWriteExactPaths
			}
		}
	}
	return binding
}

func ApplyTrustedBinding(
	descriptor Descriptor,
	binding TrustedBinding,
) Descriptor {
	return ExternalFromDescriptor(descriptor).Descriptor(binding)
}

func (b TrustedBinding) Journaled() bool {
	return b.Effect.WorkspaceTransaction == TransactionBeforeImage
}

func (d ExternalDescriptor) Descriptor(binding TrustedBinding) Descriptor {
	return Descriptor{
		Name: d.Name, Description: d.Description,
		DiscoveryTerms:     append([]string(nil), d.DiscoveryTerms...),
		InputSchema:        cloneStringMap(d.InputSchema),
		Visibility:         d.Visibility,
		Capability:         binding.Capability,
		ResourceResolver:   cloneResourceResolver(binding.ResourceResolver),
		AccessMode:         binding.AccessMode,
		ParallelPolicy:     binding.ParallelPolicy,
		RepeatPolicy:       binding.RepeatPolicy,
		SandboxRequirement: binding.SandboxRequirement,
		Aliases:            append([]Alias(nil), d.Aliases...),
		DeferredLoading:    d.Deferred,
		Availability:       d.Availability,
		UnavailableReason:  d.Unavailable,
	}
}

func (b TrustedBinding) Validate() error {
	descriptor := Descriptor{
		Name: "binding", Description: "trusted binding",
		InputSchema:        map[string]any{"type": "object"},
		Visibility:         VisibleInternal,
		Capability:         b.Capability,
		ResourceResolver:   b.ResourceResolver,
		AccessMode:         b.AccessMode,
		ParallelPolicy:     b.ParallelPolicy,
		RepeatPolicy:       b.RepeatPolicy,
		SandboxRequirement: b.SandboxRequirement,
		Availability:       AvailabilityAvailable,
	}
	if err := validateDescriptor(descriptor); err != nil {
		return err
	}
	if err := b.Effect.Validate(); err != nil {
		return err
	}
	for _, resource := range b.ResourceResolver.Templates {
		switch resource.Kind {
		case "process":
			if b.Capability != CapabilityProcess && b.Capability != CapabilityExternal {
				return errors.New("process resource requires process or external capability")
			}
		case "host", "url":
			if b.Capability != CapabilityNetwork &&
				b.Capability != CapabilityProcess &&
				b.Capability != CapabilityExternal {
				return errors.New("network resource requires network, process or external capability")
			}
		}
		if resource.Access == AccessWrite && b.Capability == CapabilityRead {
			return errors.New("write resource cannot use read capability")
		}
	}
	if b.Effect.WorkspaceTransaction == TransactionBeforeImage &&
		b.Effect.Kind != EffectWorkspaceEdit {
		return errors.New("before-image transaction requires workspace edit effect")
	}
	if b.RecordsWorkspaceRead &&
		(b.Capability != CapabilityRead || b.AccessMode != AccessRead) {
		return errors.New("workspace read evidence requires read-only capability")
	}
	if b.ProducesVerificationEvidence &&
		b.Capability != CapabilityProcess {
		return errors.New("verification evidence requires process capability")
	}
	if b.ValidateMissingWriteParent && b.Capability != CapabilityProcess {
		return errors.New("missing write targets require process capability")
	}
	if b.SandboxRequirement == SandboxStrong {
		if b.Required.FilesystemRead == "" || b.Required.Network == "" {
			return errors.New("strong sandbox requires filesystem-read and network controls")
		}
		if (b.Capability == CapabilityProcess || b.Capability == CapabilityExternal) &&
			b.Required.ProcessTree == "" {
			return errors.New("strong process sandbox requires process-tree control")
		}
	}
	if err := b.Required.Validate(); err != nil {
		return fmt.Errorf("required controls: %w", err)
	}
	return nil
}

func (e EffectContract) Validate() error {
	switch e.Mode {
	case EffectDerived:
		if e.Kind != "" || e.Risk != "" || e.Reversibility != "" {
			return errors.New("derived effect cannot carry a fixed classification")
		}
	case EffectFixed:
		if !e.Kind.Valid() || !e.Risk.Valid() || !e.Reversibility.Valid() {
			return errors.New("fixed effect classification is invalid")
		}
	default:
		return errors.New("effect mode is invalid")
	}
	switch e.WorkspaceTransaction {
	case TransactionNone, TransactionBeforeImage, TransactionBrokerOwned:
	default:
		return errors.New("workspace transaction is invalid")
	}
	if e.RequireReadBeforeWrite &&
		e.WorkspaceTransaction != TransactionBeforeImage {
		return errors.New("read-before-write requires a before-image transaction")
	}
	switch e.Approval {
	case ApprovalPolicyDefault, ApprovalPolicyOnce:
	default:
		return fmt.Errorf("approval policy %q is invalid", e.Approval)
	}
	return nil
}

func cloneResourceResolver(value ResourceResolver) ResourceResolver {
	value.Templates = append([]ResourceTemplate(nil), value.Templates...)
	return value
}

func cloneExternalDescriptor(value ExternalDescriptor) ExternalDescriptor {
	value.InputSchema = cloneStringMap(value.InputSchema)
	value.Aliases = append([]Alias(nil), value.Aliases...)
	value.DiscoveryTerms = append([]string(nil), value.DiscoveryTerms...)
	value.Requested.ResourceResolver = cloneResourceResolver(
		value.Requested.ResourceResolver,
	)
	return value
}

func cloneTrustedBinding(value TrustedBinding) TrustedBinding {
	value.ResourceResolver = cloneResourceResolver(value.ResourceResolver)
	return value
}

func trustedBindingDigest(value TrustedBinding) string {
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
