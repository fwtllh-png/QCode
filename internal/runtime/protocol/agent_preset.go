package protocol

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const AgentPresetVersion = 1

var (
	ErrAgentPresetNameConflict     = errors.New("agent preset name conflict")
	ErrAgentPresetNotFound         = errors.New("agent preset not found")
	ErrAgentPresetRevisionConflict = errors.New("agent preset revision conflict")
)

type AgentPresetScope string

const AgentPresetScopeWorkspace AgentPresetScope = "workspace"

type AgentPresetProfile struct {
	Mode            string   `json:"mode"`
	PlanningPolicy  string   `json:"planning_policy,omitempty"`
	Provider        string   `json:"provider"`
	Model           string   `json:"model"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	EnabledToolIDs  []string `json:"enabled_tool_ids,omitempty"`
	ApprovalPosture string   `json:"approval_posture"`
	ExecutionTarget string   `json:"execution_target"`
	MaxSteps        int      `json:"max_steps"`
}

func NewAgentPresetProfile(profile SessionProfile) AgentPresetProfile {
	return AgentPresetProfile{
		Mode: profile.Mode, PlanningPolicy: "adaptive",
		Provider: profile.Provider, Model: profile.Model,
		ReasoningEffort: profile.ReasoningEffort,
		EnabledToolIDs:  sortedToolIDs(profile.EnabledToolIDs),
		ApprovalPosture: profile.ApprovalPosture,
		ExecutionTarget: profile.ExecutionTarget,
		MaxSteps:        profile.MaxSteps,
	}
}

func (p AgentPresetProfile) Validate() error {
	profile := p.sessionProfile(1, 1)
	return profile.Validate()
}

func (p AgentPresetProfile) Patch(current SessionProfile) SessionProfilePatch {
	var patch SessionProfilePatch
	setStringPatch(&patch.Provider, p.Provider, current.Provider)
	setStringPatch(&patch.Model, p.Model, current.Model)
	setStringPatch(&patch.ReasoningEffort, p.ReasoningEffort, current.ReasoningEffort)
	tools := append([]string(nil), p.EnabledToolIDs...)
	slices.Sort(tools)
	currentTools := append([]string(nil), current.EnabledToolIDs...)
	slices.Sort(currentTools)
	if !slices.Equal(tools, currentTools) {
		patch.EnabledToolIDs = &tools
	}
	setStringPatch(&patch.ApprovalPosture, p.ApprovalPosture, current.ApprovalPosture)
	setStringPatch(&patch.ExecutionTarget, p.ExecutionTarget, current.ExecutionTarget)
	if p.MaxSteps != current.MaxSteps {
		value := p.MaxSteps
		patch.MaxSteps = &value
	}
	return patch
}

func setStringPatch(target **string, next, current string) {
	if next != current {
		*target = &next
	}
}

func (p AgentPresetProfile) sessionProfile(
	revision, promptCacheRevision uint64,
) SessionProfile {
	return SessionProfile{
		Version: SessionProfileVersion, Revision: revision,
		Mode: p.Mode, PlanningPolicy: "adaptive",
		Provider:        p.Provider,
		Model:           p.Model,
		ReasoningEffort: p.ReasoningEffort,
		EnabledToolIDs:  sortedToolIDs(p.EnabledToolIDs),
		ApprovalPosture: p.ApprovalPosture,
		ExecutionTarget: p.ExecutionTarget,
		MaxSteps:        p.MaxSteps, PromptCacheRevision: promptCacheRevision,
	}
}

type AgentPreset struct {
	Version     int                `json:"version"`
	ID          string             `json:"id"`
	Revision    uint64             `json:"revision"`
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Scope       AgentPresetScope   `json:"scope"`
	Profile     AgentPresetProfile `json:"profile"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`
}

func (p AgentPreset) Validate() error {
	if p.Version != AgentPresetVersion || !validProfileIdentifier(p.ID) ||
		p.Revision == 0 || p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
		return errors.New("agent preset identity, revision, and timestamps are required")
	}
	if strings.TrimSpace(p.Name) == "" || len(p.Name) > 80 ||
		strings.ContainsAny(p.Name, "\x00\r\n") ||
		len(p.Description) > 512 || strings.ContainsRune(p.Description, '\x00') {
		return errors.New("agent preset name or description is invalid")
	}
	if p.Scope != AgentPresetScopeWorkspace {
		return fmt.Errorf("agent preset scope %q is invalid", p.Scope)
	}
	return p.Profile.Validate()
}

type AgentPresetList struct {
	Version  int           `json:"version"`
	Revision uint64        `json:"revision"`
	Presets  []AgentPreset `json:"presets"`
}

func (l AgentPresetList) Validate() error {
	if l.Version != AgentPresetVersion {
		return errors.New("agent preset list version is invalid")
	}
	if len(l.Presets) > 128 || (len(l.Presets) > 0 && l.Revision == 0) {
		return errors.New("agent preset list size or revision is invalid")
	}
	seen := make(map[string]struct{}, len(l.Presets))
	for _, preset := range l.Presets {
		if err := preset.Validate(); err != nil {
			return err
		}
		if _, ok := seen[preset.ID]; ok {
			return errors.New("agent preset list contains a duplicate id")
		}
		seen[preset.ID] = struct{}{}
	}
	return nil
}

type AgentPresetMutationResult struct {
	Version   int          `json:"version"`
	Revision  uint64       `json:"revision"`
	Preset    *AgentPreset `json:"preset,omitempty"`
	DeletedID string       `json:"deleted_id,omitempty"`
	Duplicate bool         `json:"duplicate,omitempty"`
}

type AgentPresetListRequest struct {
	SessionID string `json:"session_id"`
}

func (r AgentPresetListRequest) Validate() error {
	if !validProfileIdentifier(r.SessionID) {
		return errors.New("agent preset list session_id is invalid")
	}
	return nil
}

type AgentPresetSaveRequest struct {
	SessionID        string             `json:"session_id"`
	ID               string             `json:"id"`
	ExpectedRevision uint64             `json:"expected_revision"`
	Name             string             `json:"name"`
	Description      string             `json:"description,omitempty"`
	Profile          AgentPresetProfile `json:"profile"`
}

func (r AgentPresetSaveRequest) Validate() error {
	if !validProfileIdentifier(r.SessionID) ||
		!validProfileIdentifier(r.ID) {
		return errors.New("agent preset save identity is invalid")
	}
	candidate := AgentPreset{
		Version: AgentPresetVersion, ID: r.ID, Revision: 1,
		Name: r.Name, Description: r.Description,
		Scope: AgentPresetScopeWorkspace, Profile: r.Profile,
		CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
	}
	return candidate.Validate()
}

type AgentPresetDeleteRequest struct {
	SessionID        string `json:"session_id"`
	ID               string `json:"id"`
	ExpectedRevision uint64 `json:"expected_revision"`
}

func (r AgentPresetDeleteRequest) Validate() error {
	if !validProfileIdentifier(r.SessionID) ||
		!validProfileIdentifier(r.ID) ||
		r.ExpectedRevision == 0 {
		return errors.New("agent preset delete identity and revision are required")
	}
	return nil
}

type AgentPresetApplyRequest struct {
	SessionID               string   `json:"session_id"`
	ThreadID                ThreadID `json:"thread_id"`
	PresetID                string   `json:"preset_id"`
	ExpectedProfileRevision uint64   `json:"expected_profile_revision"`
}

func (r AgentPresetApplyRequest) Validate() error {
	if !validProfileIdentifier(r.SessionID) || r.ThreadID == "" ||
		!validProfileIdentifier(r.PresetID) ||
		r.ExpectedProfileRevision == 0 {
		return errors.New("agent preset apply identity and revision are required")
	}
	return nil
}

type AgentPresetApplyResult struct {
	Version         int                        `json:"version"`
	PresetID        string                     `json:"preset_id"`
	ProfileUpdate   SessionProfileUpdateResult `json:"profile_update"`
	RestartRequired bool                       `json:"restart_required"`
	RestartReason   string                     `json:"restart_reason,omitempty"`
}
