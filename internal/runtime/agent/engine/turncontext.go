package engine

import (
	"fmt"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

// TurnIdentity binds a Scope to the durable Session and Turn revisions it may
// commit. These values never change after the Scope is opened.
type TurnIdentity struct {
	SessionID       string
	ThreadID        string
	TurnID          string
	ProfileRevision uint64
}

// TurnRequest is the host input frozen before a Scope starts.
type TurnRequest struct {
	TurnID      string
	Prompt      string
	Intent      protocol.TurnIntent
	Attachments []provider.Attachment
	Recovery    *protocol.TurnRecoveryContext
}

// TurnLimits freezes the budgets that bound one Scope.
type TurnLimits struct {
	MaxSteps        int
	MaxOutputTokens uint64
	Budget          Budget
	Context         agentcontext.Capacity
}

type TurnSnapshotSources struct {
	MCP            func() []MCPHealthSnapshot
	Skills         func() []SkillSummary
	SkillSelection func(string) ([]SkillSummary, SkillSelectionMetrics, error)
	Memory         func(string) (MemorySnapshot, error)
}

type MemorySnapshot = promptcontext.MemorySnapshot

// TurnSpec is the complete immutable input for one Engine Scope. Host profile,
// policy, route, catalog, and skill mutations apply only to the next Scope.
type TurnSpec struct {
	Identity TurnIdentity
	Request  TurnRequest
	Profile  protocol.SessionProfile
	// Purpose is what this turn samples for, derived from the frozen mode. It is
	// what selects Route out of the session's routing table.
	Purpose model.Purpose
	// Route is the model this turn samples on. Freezing it with the mode is what
	// keeps a mid-turn switch to plan from changing which model is already
	// answering.
	Route          model.ReadyRoute
	Provider       string
	Model          string
	ModelMetadata  *protocol.ModelMetadataProvenance
	Mode           policy.Mode
	Posture        policy.Permission
	Workspace      string
	Sandbox        string
	DelegationMode string
	Policy         *policy.Runtime
	Kernel         turnkernel.Policy
	Limits         TurnLimits
	World          agentcontext.WorldBaseline
	Window         agentcontext.WindowLedger
	Catalog        tool.CatalogSnapshot
	Skills         []SkillSummary
	SkillSelection SkillSelectionMetrics
	MCP            []MCPHealthSnapshot
	Memory         MemorySnapshot
}

// SkillSummary is a turn-frozen skill catalog entry (N10).
type SkillSummary = promptcontext.SkillSummary
type SkillSelectionMetrics = promptcontext.SkillSelectionMetrics

// PurposeForMode is which route a turn in this mode samples on. Plan mode is the
// only mode with a purpose of its own: operate is act with wider permissions, not
// a different kind of thinking, so giving it a third slot would ask an operator
// to configure a distinction the runtime does not make.
func PurposeForMode(mode policy.Mode) model.Purpose {
	if mode == policy.ModePlan {
		return model.PurposePlan
	}
	return model.PurposeAct
}

// SnapshotTurnSpec captures all mutable Session inputs before a Scope starts.
//
// It fails when the frozen mode's purpose has no route, which happens only under
// a locked route set. Failing here is deliberate: it is before the turn announces
// what it is about to do, so a locked session that cannot honor plan mode says so
// instead of sampling on the act model and reporting plan.
func SnapshotTurnSpec(
	options Options,
	identity TurnIdentity,
	request TurnRequest,
) (TurnSpec, error) {
	security := options.Security
	if security == nil {
		security = policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)
	} else {
		security = security.CloneSampling()
	}
	routes, err := effectiveRoutes(options)
	if err != nil {
		return TurnSpec{}, err
	}
	purpose := PurposeForMode(security.Mode)
	route, err := routes.For(purpose)
	if err != nil {
		return TurnSpec{}, err
	}
	if options.ToolCatalogSync != nil {
		if err := options.ToolCatalogSync(); err != nil {
			return TurnSpec{}, protocol.WrapProblem(
				protocol.CodeUnavailable,
				"tool catalog synchronization failed",
				true,
				err,
			)
		}
	}
	catalog, err := options.Tools.Snapshot()
	if err != nil {
		return TurnSpec{}, fmt.Errorf("snapshot turn tool catalog: %w", err)
	}
	request.Intent = protocol.NormalizeTurnIntent(request.Intent)
	request.Attachments = append([]provider.Attachment(nil), request.Attachments...)
	if request.Recovery != nil {
		recovery := *request.Recovery
		request.Recovery = &recovery
	}
	kernelPolicy := turnkernel.DefaultPolicy()
	kernelPolicy.CompletionRequired = options.RequireCompletionDeclaration
	// StructuredTerminalRequired stays off for the main agent and children.
	// A no-tool assistant body is the stop signal; turn_complete is optional.
	kernelPolicy.StructuredTerminalRequired = false
	kernelPolicy.VerificationRequired = options.Verify.Enabled()
	kernelPolicy.VerificationMustPass = options.Verify.Enabled() &&
		options.Verify.Mode == VerifyModeHard &&
		options.Verify.OnFailure != VerifyOnFailureRevert &&
		request.Intent == protocol.TurnIntentWorkspaceChange
	kernelPolicy.VerificationMode = options.Verify.Mode
	kernelPolicy.VerificationOnFailure = options.Verify.OnFailure
	kernelPolicy.VerificationRepairLimit =
		uint32(max(options.Verify.MaxRepairSteps, 0))
	kernelPolicy.ExecutionStepLimit = uint32(max(options.MaxSteps, 0))
	progressLease := kernelPolicy.ExecutionStepLimit
	if progressLease > 0 {
		progressLease += kernelPolicy.CompletionRepairLimit +
			kernelPolicy.WorkspaceRepairLimit +
			kernelPolicy.DeclarationRepairLimit
		// Verification repairs only consume samples on workspace-change
		// turns. Explore/answer children inherit soft verify options but
		// cannot spend that reserve, so it must not stretch their lease.
		if kernelPolicy.VerificationRequired &&
			request.Intent == protocol.TurnIntentWorkspaceChange {
			progressLease += kernelPolicy.VerificationRepairLimit
		}
	}
	kernelPolicy.Convergence = turnkernel.ConvergencePolicyForStepLimit(
		progressLease,
	)
	kernelPolicy.JournalRequired = options.Journal != nil
	planning := security.PlanningSnapshot()
	capacity := agentcontext.ResolveCapacity(
		route,
		options.MaxOutputTokens,
		options.Budget.MaxTurnTokens,
		options.Budget.MaxTokens,
	)
	spec := TurnSpec{
		Identity: identity,
		Request:  request,
		Profile: protocol.SessionProfile{
			Version:  protocol.SessionProfileVersion,
			Revision: identity.ProfileRevision,
			Mode:     string(security.Mode), Provider: route.ProviderID(),
			PlanningPolicy: planning.Planning,
			Model:          route.Model().ID, ReasoningEffort: options.ReasoningEffort,
			ApprovalPosture: string(security.Permission),
			ExecutionTarget: "local", MaxSteps: options.MaxSteps,
			PromptCacheRevision: identity.ProfileRevision,
		},
		Purpose:  purpose,
		Route:    route,
		Provider: route.ProviderID(), Model: route.Model().ID,
		ModelMetadata: modelMetadataProvenance(
			route.Model().MetadataProvenance,
		),
		Mode: security.Mode, Posture: security.Permission,
		Workspace: options.Workspace, Sandbox: sandboxIdentity(options.Tools),
		DelegationMode: options.DelegationMode,
		Policy:         security,
		Kernel:         kernelPolicy,
		Limits: TurnLimits{
			MaxSteps:        options.MaxSteps,
			MaxOutputTokens: options.MaxOutputTokens,
			Budget:          options.Budget,
			Context:         capacity,
		},
		Catalog: catalog,
	}
	if options.TurnSnapshots.SkillSelection != nil {
		spec.Skills, spec.SkillSelection, err =
			options.TurnSnapshots.SkillSelection(request.Prompt)
		if err != nil {
			return TurnSpec{}, fmt.Errorf("select turn skills: %w", err)
		}
		spec.Skills = append([]SkillSummary(nil), spec.Skills...)
	} else if options.TurnSnapshots.Skills != nil {
		spec.Skills = append([]SkillSummary(nil), options.TurnSnapshots.Skills()...)
	}
	if options.TurnSnapshots.MCP != nil {
		spec.MCP = append([]MCPHealthSnapshot(nil), options.TurnSnapshots.MCP()...)
	}
	if options.TurnSnapshots.Memory != nil {
		spec.Memory, err = options.TurnSnapshots.Memory(request.Prompt)
		if err != nil {
			spec.Memory = MemorySnapshot{
				Source:        "memory://records",
				FailureReason: "retrieval_failed",
			}
		}
		spec.Memory.SelectedIDs = append(
			[]string(nil),
			spec.Memory.SelectedIDs...,
		)
	}
	return spec, nil
}

// effectiveRoutes is the routing table an Options describes: the one it carries,
// or a single-route table built from Route for the callers that only ever had
// one model.
func effectiveRoutes(options Options) (model.RouteSet, error) {
	if options.Routes.Ready() {
		return options.Routes, nil
	}
	return model.NewRouteSet(options.Route, nil, false)
}

func sandboxIdentity(registry *tool.Registry) string {
	if registry == nil {
		return "unavailable"
	}
	backend := registry.SandboxBackend()
	if backend == nil {
		return "unavailable"
	}
	capability := backend.Capability()
	if !capability.Available {
		return "unavailable"
	}
	return fmt.Sprintf(
		"%s/%s/%s",
		capability.Platform,
		capability.Backend,
		capability.Effective.Identity(),
	)
}
