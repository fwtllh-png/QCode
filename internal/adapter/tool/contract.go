package tool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

type (
	EffectKind    = securitymodel.EffectKind
	RiskLevel     = securitymodel.Risk
	Reversibility = securitymodel.Reversibility
)

const (
	EffectWorkspaceRead    = securitymodel.WorkspaceRead
	EffectWorkspaceEdit    = securitymodel.WorkspaceEdit
	EffectProcessReadOnly  = securitymodel.ProcessReadOnly
	EffectProcessMutating  = securitymodel.ProcessMutating
	EffectNetworkRead      = securitymodel.NetworkRead
	EffectNetworkMutating  = securitymodel.NetworkMutating
	EffectSessionMutation  = securitymodel.SessionMutation
	EffectAgentMessage     = securitymodel.AgentMessage
	EffectAgentLifecycle   = securitymodel.AgentLifecycle
	EffectExternalMutation = securitymodel.ExternalMutation

	RiskLow      = securitymodel.RiskLow
	RiskMedium   = securitymodel.RiskMedium
	RiskHigh     = securitymodel.RiskHigh
	RiskCritical = securitymodel.RiskCritical

	Reversible   = securitymodel.Reversible
	Bounded      = securitymodel.Bounded
	Irreversible = securitymodel.Irreversible
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

type ApprovalMode = securitymodel.ApprovalMode

const (
	ApprovalPolicyDefault = securitymodel.ApprovalDefault
	ApprovalPolicyOnce    = securitymodel.ApprovalOnce
)

// PlanningMode declares whether the consequential-action plan gate applies.
type PlanningMode string

const (
	PlanningDefault PlanningMode = ""
	PlanningExempt  PlanningMode = "exempt"
)

// ArgumentMatch selects invocations whose string argument Field equals one of
// Values after trimming and case folding.
type ArgumentMatch struct {
	Field  string   `json:"field"`
	Values []string `json:"values"`
}

func (m ArgumentMatch) Matches(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, candidate := range m.Values {
		if value != "" && value == strings.ToLower(strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
}

type EffectContract struct {
	Mode                   EffectMode           `json:"mode"`
	Kind                   EffectKind           `json:"kind,omitempty"`
	Risk                   RiskLevel            `json:"risk,omitempty"`
	Reversibility          Reversibility        `json:"reversibility,omitempty"`
	WorkspaceTransaction   WorkspaceTransaction `json:"workspace_transaction"`
	RequireReadBeforeWrite bool                 `json:"require_read_before_write,omitempty"`
	Approval               ApprovalMode         `json:"approval"`
	// ReadOnlyWhen declares argument values under which a fixed effect only
	// observes: the invocation is low-risk, reversible, and exempt from
	// adaptive planning.
	ReadOnlyWhen *ArgumentMatch `json:"read_only_when,omitempty"`
	Planning     PlanningMode   `json:"planning,omitempty"`
}

type RequiredControls = securitymodel.RequiredControls

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

// TrustedBinding is the Registry-owned authority contract. Guard projects its
// security fields into the assessment instead of trusting the presentation descriptor.
type TrustedBinding struct {
	SupportsHostExecution bool `json:"supports_host_execution,omitempty"`
	// SupportsFullAccess opts a builtin command executor into session-selected
	// process authority. External descriptors cannot grant this capability.
	SupportsFullAccess           bool               `json:"supports_full_access,omitempty"`
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
	// VerificationField names the argument that declares a verification run;
	// together with a non-empty ResourceResolver.ReadPathsField argument it
	// lets a plan gate ask once instead of holding.
	VerificationField          string `json:"verification_field,omitempty"`
	ValidateMissingWriteParent bool   `json:"validate_missing_write_parent,omitempty"`
	// IsolatesWriteTrees promises that, when an Isolator is bound and a tree
	// is declared, all workspace writes run in its copy and settle via the
	// File Broker / Journal. Preparation failure must not fall back in place.
	IsolatesWriteTrees bool `json:"isolates_write_trees,omitempty"`
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
		binding.Required.FilesystemRead = securitymodel.FilesystemReadDeclaredRoots
		binding.Required.Network = securitymodel.NetworkDirect
		binding.Required.PathIdentity = securitymodel.PathIdentityDescriptorRelative
		if descriptor.Capability == CapabilityProcess ||
			descriptor.Capability == CapabilityExternal {
			binding.Required.ProcessTree = securitymodel.ProcessTreeGroupKill
		}
		for _, resource := range descriptor.ResourceResolver.Templates {
			if resource.Access == AccessWrite &&
				(resource.Kind == "file" || resource.Kind == "directory" ||
					resource.Kind == "repo" || resource.Kind == "workspace") {
				binding.Required.FilesystemWrite = securitymodel.FilesystemWriteExactPaths
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
	if b.VerificationField != "" &&
		(!b.ProducesVerificationEvidence || b.ResourceResolver.ReadPathsField == "") {
		return errors.New("verification field requires verification evidence and covered paths")
	}
	if b.ValidateMissingWriteParent && b.Capability != CapabilityProcess {
		return errors.New("missing write targets require process capability")
	}
	if b.IsolatesWriteTrees && (b.Capability != CapabilityProcess ||
		b.SandboxRequirement != SandboxStrong || b.ResourceResolver.PathsField == "") {
		return errors.New("isolated write trees require a strong process sandbox and declared write paths")
	}
	if (b.SupportsFullAccess || b.SupportsHostExecution) && (b.Capability != CapabilityProcess || b.SandboxRequirement != SandboxStrong || b.Effect.Mode == EffectFixed) {
		return errors.New("full access requires a strong process binding with derived effects")
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
	switch e.Planning {
	case PlanningDefault, PlanningExempt:
	default:
		return fmt.Errorf("planning mode %q is invalid", e.Planning)
	}
	if match := e.ReadOnlyWhen; match != nil {
		if e.Mode != EffectFixed {
			return errors.New("read-only declaration requires a fixed effect")
		}
		if strings.TrimSpace(match.Field) == "" || len(match.Values) == 0 {
			return errors.New("read-only declaration requires a field and values")
		}
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
	if match := value.Effect.ReadOnlyWhen; match != nil {
		value.Effect.ReadOnlyWhen = &ArgumentMatch{
			Field: match.Field, Values: append([]string(nil), match.Values...),
		}
	}
	return value
}

func trustedBindingDigest(value TrustedBinding) string {
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
