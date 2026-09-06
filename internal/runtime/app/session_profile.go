package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (r *SessionService) SessionToolCatalog(
	ctx context.Context,
	sessionID string,
) (protocol.SessionToolCatalog, error) {
	if r.toolCatalog == nil {
		return protocol.SessionToolCatalog{}, runtimeProblem(protocol.CodeUnavailable, "session tool catalog is unavailable", nil)
	}
	profile, err := r.SessionProfile(ctx, sessionID)
	if err != nil {
		return protocol.SessionToolCatalog{}, err
	}
	snapshot, err := r.toolCatalog.Snapshot()
	if err != nil {
		return protocol.SessionToolCatalog{}, fmt.Errorf("snapshot tool catalog: %w", err)
	}
	enabled := make(map[string]bool, len(profile.Profile.EnabledToolIDs))
	for _, id := range profile.Profile.EnabledToolIDs {
		enabled[id] = true
	}
	allEnabled := len(enabled) == 0
	result := protocol.SessionToolCatalog{
		Version:   protocol.SessionToolCatalogVersion,
		CatalogID: snapshot.CatalogID, Generation: snapshot.Generation,
		Digest: snapshot.Digest,
	}
	entries := snapshot.Entries()
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		descriptor := entry.Descriptor
		if descriptor.Visibility != tool.VisibleModel {
			continue
		}
		sourceKind, sourceLabel := projectToolSource(entry.Name, entry.Source)
		id := tool.CatalogToolID(entry.Name, entry.Source)
		seen[id] = true
		result.Tools = append(result.Tools, protocol.SessionToolCatalogEntry{
			ID: id, Name: boundedCatalogText(entry.Name, 256),
			Description: boundedCatalogText(descriptor.Description, 4096),
			SourceKind:  sourceKind, SourceLabel: sourceLabel,
			Capability:         string(descriptor.Capability),
			AccessMode:         string(descriptor.AccessMode),
			RiskLevel:          catalogRiskLevel(descriptor.Capability, descriptor.AccessMode),
			SandboxRequirement: string(descriptor.SandboxRequirement),
			PolicyState:        "deferred",
			PolicyReason:       "Final policy decision requires validated arguments and resources",
			ConstitutionState:  "deferred",
			ConstitutionReason: "Final constitution decision is enforced by the Tool Guard",
			Availability:       string(descriptor.Availability),
			UnavailableReason:  boundedCatalogText(descriptor.UnavailableReason, 4096),
			State:              string(entry.State), Revision: entry.Revision,
			Enabled: allEnabled || enabled[id],
			Guarded: true,
		})
	}
	for _, id := range profile.Profile.EnabledToolIDs {
		if seen[id] {
			continue
		}
		sourceKind, name, ok := tool.ParseCatalogToolID(id)
		if !ok {
			continue
		}
		sourceLabel := strings.ToUpper(sourceKind[:1]) + sourceKind[1:]
		if sourceKind == "mcp" {
			sourceLabel = "MCP"
		}
		result.Tools = append(result.Tools, protocol.SessionToolCatalogEntry{
			ID: id, Name: name,
			Description:        "Tool is no longer registered in the Runtime catalog",
			SourceKind:         sourceKind,
			SourceLabel:        sourceLabel,
			Capability:         "unknown",
			AccessMode:         "unknown",
			RiskLevel:          "unknown",
			SandboxRequirement: "unknown",
			PolicyState:        "deferred",
			PolicyReason:       "Revoked Tool has no executable policy decision",
			ConstitutionState:  "deferred",
			ConstitutionReason: "Revoked Tool cannot reach Constitution evaluation",
			Availability:       "unavailable",
			UnavailableReason:  "Tool was revoked or its source is disconnected",
			State:              "revoked",
			Revision:           1,
			Enabled:            true,
			Guarded:            true,
		})
	}
	sort.Slice(result.Tools, func(i, j int) bool {
		return result.Tools[i].ID < result.Tools[j].ID
	})
	if err := result.Validate(); err != nil {
		return protocol.SessionToolCatalog{}, err
	}
	return result, nil
}

func catalogRiskLevel(capability tool.Capability, access tool.AccessMode) string {
	switch {
	case capability == tool.CapabilityRead && access == tool.AccessRead:
		return "low"
	case capability == tool.CapabilityWrite:
		return "medium"
	case capability == tool.CapabilityProcess ||
		capability == tool.CapabilityNetwork ||
		capability == tool.CapabilityExternal ||
		access == tool.AccessTree:
		return "high"
	default:
		return "unknown"
	}
}

func boundedCatalogText(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	end := maximum
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func projectToolSource(name, source string) (string, string) {
	switch kind := tool.CatalogSourceKind(name, source); kind {
	case "mcp":
		label := strings.TrimPrefix(source, "mcp:")
		if label == "helpers" {
			label = "MCP"
		}
		return "mcp", label
	case "external":
		return "external", "External"
	case "dynamic":
		return "dynamic", "Host"
	case "skill":
		return "skill", "Skills"
	default:
		return "builtin", "QCode"
	}
}

func (r *SessionService) SessionProfile(
	ctx context.Context,
	sessionID string,
) (protocol.SessionProfileSnapshot, error) {
	if r.profiles == nil {
		return protocol.SessionProfileSnapshot{}, runtimeProblem(protocol.CodeUnavailable, "session profiles are unavailable", nil)
	}
	if r.workspaceRoot != "" {
		if _, err := r.SessionStatus(ctx, sessionID); err != nil {
			return protocol.SessionProfileSnapshot{}, err
		}
	}
	profile, err := r.profiles.EnsureProfile(ctx, sessionID, r.defaultProfile)
	if err != nil {
		return protocol.SessionProfileSnapshot{}, err
	}
	if !profileFieldMutable(
		r.profileCapabilities.MutableFields,
		"reasoning_effort",
	) {
		profile.ReasoningEffort = r.defaultProfile.ReasoningEffort
	}
	capabilities, err := r.capabilitiesForProfile(profile)
	if err != nil {
		return protocol.SessionProfileSnapshot{}, err
	}
	if err := capabilities.Validate(profile); err != nil {
		return protocol.SessionProfileSnapshot{}, err
	}
	return protocol.SessionProfileSnapshot{
		Profile:      profile,
		Capabilities: capabilities,
	}, nil
}

func (r *Runtime) capabilitiesForProfile(
	profile protocol.SessionProfile,
) (protocol.SessionProfileCapabilities, error) {
	capabilities := r.profileCapabilities
	capabilities.Provider = profile.Provider
	capabilities.Model = profile.Model
	key := profile.Provider + "\x00" + profile.Model
	if modelCapabilities, ok := r.profileModels[key]; ok {
		capabilities.ModelCapabilities = modelCapabilities
		return capabilities, nil
	}
	if profile.Provider == r.defaultProfile.Provider &&
		profile.Model == r.defaultProfile.Model {
		return capabilities, nil
	}
	return protocol.SessionProfileCapabilities{}, runtimeProblem(
		protocol.CodeInvalidArgument,
		"session profile route is unavailable in this Runtime",
		nil,
	)
}

func profileFieldMutable(fields []string, target string) bool {
	for _, field := range fields {
		if field == target {
			return true
		}
	}
	return false
}

func (r *SessionService) SessionProfilesAvailable() bool {
	return r != nil && r.profiles != nil
}

func (r *SessionService) RestoreSessionProfile(
	ctx context.Context,
	sessionID string,
	threadID protocol.ThreadID,
) (protocol.SessionProfileSnapshot, error) {
	snapshot, err := r.sessionProfileForRestore(ctx, sessionID, threadID)
	if err != nil {
		return protocol.SessionProfileSnapshot{}, err
	}
	controller, ok := r.engine.(SessionProfileEngine)
	if !ok {
		return protocol.SessionProfileSnapshot{}, runtimeProblem(protocol.CodeUnavailable, "session profile updates are unsupported by this engine", nil)
	}
	r.active.mu.Lock()
	defer r.active.mu.Unlock()
	if _, active := r.active.byThread[threadID]; active {
		if r.active.profiles[threadID] == snapshot.Profile.Revision {
			return snapshot, nil
		}
		return protocol.SessionProfileSnapshot{}, retryableProblem(
			protocol.CodeConflict,
			"session profile cannot be restored while its thread has an active turn",
		)
	}
	if err := controller.ValidateSessionProfile(threadID, snapshot.Profile); err != nil {
		return protocol.SessionProfileSnapshot{}, err
	}
	if err := controller.ApplySessionProfile(threadID, snapshot.Profile); err != nil {
		return protocol.SessionProfileSnapshot{}, err
	}
	r.active.profiles[threadID] = snapshot.Profile.Revision
	return snapshot, nil
}

func (r *SessionService) UpdateSessionProfile(
	ctx context.Context,
	sessionID string,
	threadID protocol.ThreadID,
	expectedRevision uint64,
	patch protocol.SessionProfilePatch,
) (protocol.SessionProfileUpdateResult, error) {
	if r.profiles == nil {
		return protocol.SessionProfileUpdateResult{}, runtimeProblem(protocol.CodeUnavailable, "session profiles are unavailable", nil)
	}
	if r.workspaceRoot != "" {
		if _, err := r.SessionStatus(ctx, sessionID); err != nil {
			return protocol.SessionProfileUpdateResult{}, err
		}
		owner, err := r.sessionLifecycle.SessionForThread(ctx, threadID)
		if err != nil {
			return protocol.SessionProfileUpdateResult{}, err
		}
		if owner != sessionID {
			return protocol.SessionProfileUpdateResult{}, runtimeProblem(
				protocol.CodeConflict,
				"thread does not belong to session",
				nil,
			)
		}
	}
	controller, ok := r.engine.(SessionProfileEngine)
	if !ok {
		return protocol.SessionProfileUpdateResult{}, runtimeProblem(protocol.CodeUnavailable, "session profile updates are unsupported by this engine", nil)
	}
	r.active.mu.Lock()
	defer r.active.mu.Unlock()
	if _, active := r.active.byThread[threadID]; active {
		return protocol.SessionProfileUpdateResult{}, retryableProblem(
			protocol.CodeConflict,
			"session profile cannot change while its thread has an active turn",
		)
	}
	current, err := r.profiles.Profile(ctx, sessionID, r.defaultProfile)
	if err != nil {
		return protocol.SessionProfileUpdateResult{}, err
	}
	candidate, err := protocol.ApplySessionProfilePatch(current, patch)
	if err != nil {
		return protocol.SessionProfileUpdateResult{},
			runtimeProblem(protocol.CodeInvalidArgument, err.Error(), err)
	}
	if err := validateMutableProfilePatch(
		patch,
		r.profileCapabilities.MutableFields,
	); err != nil {
		return protocol.SessionProfileUpdateResult{}, err
	}
	if err := controller.ValidateSessionProfile(threadID, candidate.Profile); err != nil {
		return protocol.SessionProfileUpdateResult{},
			runtimeProblem(protocol.CodeInvalidArgument, err.Error(), err)
	}
	updated, err := r.profiles.UpdateProfile(
		ctx,
		sessionID,
		expectedRevision,
		r.defaultProfile,
		patch,
	)
	if err != nil {
		return protocol.SessionProfileUpdateResult{}, err
	}
	if err := controller.ApplySessionProfile(threadID, updated.Profile); err != nil {
		return protocol.SessionProfileUpdateResult{}, fmt.Errorf(
			"apply persisted session profile: %w",
			err,
		)
	}
	r.active.profiles[threadID] = updated.Profile.Revision
	return updated, nil
}

func validateMutableProfilePatch(
	patch protocol.SessionProfilePatch,
	mutable []string,
) error {
	allowed := make(map[string]bool, len(mutable))
	for _, field := range mutable {
		allowed[field] = true
	}
	fields := []struct {
		name string
		set  bool
	}{
		{"mode", patch.Mode != nil},
		{"planning_policy", patch.PlanningPolicy != nil},
		{"provider", patch.Provider != nil},
		{"model", patch.Model != nil},
		{"reasoning_effort", patch.ReasoningEffort != nil},
		{"enabled_tool_ids", patch.EnabledToolIDs != nil},
		{"approval_posture", patch.ApprovalPosture != nil},
		{"execution_target", patch.ExecutionTarget != nil},
		{"max_steps", patch.MaxSteps != nil},
	}
	for _, field := range fields {
		if field.set && !allowed[field.name] {
			return runtimeProblem(
				protocol.CodeConflict,
				fmt.Sprintf("session profile field %s is immutable in this runtime", field.name),
				nil,
			)
		}
	}
	return nil
}
