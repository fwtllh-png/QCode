package protocol

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

const SessionProfileVersion = 1

type SessionProfile struct {
	Version             int      `json:"version"`
	Revision            uint64   `json:"revision"`
	Mode                string   `json:"mode"`
	PlanningPolicy      string   `json:"planning_policy,omitempty"`
	Provider            string   `json:"provider"`
	Model               string   `json:"model"`
	ReasoningEffort     string   `json:"reasoning_effort,omitempty"`
	EnabledToolIDs      []string `json:"enabled_tool_ids,omitempty"`
	ApprovalPosture     string   `json:"approval_posture"`
	ExecutionTarget     string   `json:"execution_target"`
	MaxSteps            int      `json:"max_steps"`
	PromptCacheRevision uint64   `json:"prompt_cache_revision"`
}

type SessionProfilePatch struct {
	PlanningPolicy  *string   `json:"planning_policy,omitempty"`
	Provider        *string   `json:"provider,omitempty"`
	Model           *string   `json:"model,omitempty"`
	ReasoningEffort *string   `json:"reasoning_effort,omitempty"`
	EnabledToolIDs  *[]string `json:"enabled_tool_ids,omitempty"`
	ApprovalPosture *string   `json:"approval_posture,omitempty"`
	ExecutionTarget *string   `json:"execution_target,omitempty"`
	MaxSteps        *int      `json:"max_steps,omitempty"`
}

type ModelCapabilities struct {
	DisplayName            string                  `json:"display_name"`
	ContextWindow          uint64                  `json:"context_window"`
	MaxOutputTokens        uint64                  `json:"max_output_tokens"`
	Streaming              bool                    `json:"streaming"`
	Reasoning              bool                    `json:"reasoning"`
	ToolCalls              bool                    `json:"tool_calls"`
	ParallelToolCalls      string                  `json:"parallel_tool_calls"`
	NativeSearch           bool                    `json:"native_search"`
	IncrementalResponses   bool                    `json:"incremental_responses"`
	Vision                 bool                    `json:"vision"`
	ImageInput             bool                    `json:"image_input"`
	PromptCache            bool                    `json:"prompt_cache"`
	AutomaticPromptCache   bool                    `json:"automatic_prompt_cache"`
	ThinkingToggle         bool                    `json:"thinking_toggle"`
	ReasoningEfforts       []string                `json:"reasoning_efforts,omitempty"`
	DefaultReasoningEffort string                  `json:"default_reasoning_effort,omitempty"`
	MetadataProvenance     ModelMetadataProvenance `json:"metadata_provenance"`
	CredentialStatus       string                  `json:"credential_status"`
	Availability           string                  `json:"availability"`
	UnavailableReason      string                  `json:"unavailable_reason,omitempty"`
	SelectionMode          string                  `json:"selection_mode"`
}

type ModelMetadataProvenance struct {
	CanonicalID  string `json:"canonical_id"`
	WireID       string `json:"wire_id"`
	Limits       string `json:"limits"`
	Capabilities string `json:"capabilities"`
	Pricing      string `json:"pricing"`
}

func (p ModelMetadataProvenance) Validate() error {
	for _, entry := range []struct {
		field string
		value string
	}{
		{field: "canonical_id", value: p.CanonicalID},
		{field: "wire_id", value: p.WireID},
		{field: "limits", value: p.Limits},
		{field: "capabilities", value: p.Capabilities},
		{field: "pricing", value: p.Pricing},
	} {
		switch entry.value {
		case "bundled", "config", "startup", "fixture",
			"provider_discovery", "operator_config", "mixed":
		default:
			return fmt.Errorf(
				"model metadata %s provenance is invalid",
				entry.field,
			)
		}
	}
	return nil
}

type SessionProfileCapabilities struct {
	Provider          string            `json:"provider"`
	Model             string            `json:"model"`
	ModelCapabilities ModelCapabilities `json:"model_capabilities"`
	MutableFields     []string          `json:"mutable_fields"`
}

type SessionProfileSnapshot struct {
	Profile      SessionProfile             `json:"profile"`
	Capabilities SessionProfileCapabilities `json:"capabilities"`
}

type SessionProfileUpdateResult struct {
	Profile          SessionProfile `json:"profile"`
	PromptCacheReset bool           `json:"prompt_cache_reset"`
	ResetReason      string         `json:"reset_reason,omitempty"`
}

func (p SessionProfile) Validate() error {
	if p.Version != SessionProfileVersion {
		return fmt.Errorf("unsupported session profile version %d", p.Version)
	}
	if p.Revision == 0 || p.PromptCacheRevision == 0 {
		return errors.New("session profile revisions must be positive")
	}
	if p.Mode != "act" {
		return errors.New("session profile mode must be act")
	}
	if !slices.Contains([]string{"", "off", "adaptive", "required"}, p.PlanningPolicy) {
		return errors.New("session profile planning_policy is invalid")
	}
	if !validProfileIdentifier(p.Provider) || !validProfileIdentifier(p.Model) || strings.ContainsAny(p.Model, "\t ") {
		return errors.New("session profile provider and model are invalid")
	}
	switch p.ReasoningEffort {
	case "", "none", "off", "minimal", "low", "medium", "high", "max", "xhigh":
	default:
		return errors.New("session profile reasoning_effort is invalid")
	}
	switch p.ApprovalPosture {
	case "never", "suggest", "auto", "bypass":
	default:
		return errors.New("session profile approval_posture is invalid")
	}
	if p.ExecutionTarget != "local" {
		return errors.New("session profile execution_target must be local")
	}
	if p.MaxSteps < 0 {
		return errors.New("session profile max_steps must be non-negative")
	}
	if len(p.EnabledToolIDs) > 512 {
		return errors.New("session profile accepts at most 512 enabled tools")
	}
	seen := make(map[string]struct{}, len(p.EnabledToolIDs))
	for _, id := range p.EnabledToolIDs {
		if !validProfileIdentifier(id) {
			return errors.New("session profile enabled tool id is invalid")
		}
		if _, exists := seen[id]; exists {
			return errors.New("session profile enabled tool ids must be unique")
		}
		seen[id] = struct{}{}
	}
	return nil
}

func (p SessionProfilePatch) Validate() error {
	if p.Provider == nil && p.Model == nil &&
		p.PlanningPolicy == nil &&
		p.ReasoningEffort == nil && p.EnabledToolIDs == nil &&
		p.ApprovalPosture == nil && p.ExecutionTarget == nil &&
		p.MaxSteps == nil {
		return errors.New("session profile patch must change at least one field")
	}
	return nil
}

func ApplySessionProfilePatch(
	current SessionProfile,
	patch SessionProfilePatch,
) (SessionProfileUpdateResult, error) {
	if err := current.Validate(); err != nil {
		return SessionProfileUpdateResult{}, err
	}
	if err := patch.Validate(); err != nil {
		return SessionProfileUpdateResult{}, err
	}
	next := current
	var cacheReasons []string
	setCacheReason := func(changed bool, field string) {
		if changed {
			cacheReasons = append(cacheReasons, field)
		}
	}
	applyCached := func(target *string, change *string, field string) {
		if change != nil {
			setCacheReason(*target != *change, field)
			*target = *change
		}
	}
	applyCached(&next.PlanningPolicy, patch.PlanningPolicy, "planning_policy")
	applyCached(&next.Provider, patch.Provider, "provider")
	applyCached(&next.Model, patch.Model, "model")
	applyCached(&next.ReasoningEffort, patch.ReasoningEffort, "reasoning_effort")
	if patch.EnabledToolIDs != nil {
		tools := sortedToolIDs(*patch.EnabledToolIDs)
		setCacheReason(
			!slices.Equal(sortedToolIDs(next.EnabledToolIDs), tools),
			"enabled_tool_ids",
		)
		next.EnabledToolIDs = tools
	}
	if patch.ApprovalPosture != nil {
		next.ApprovalPosture = *patch.ApprovalPosture
	}
	if patch.ExecutionTarget != nil {
		next.ExecutionTarget = *patch.ExecutionTarget
	}
	if patch.MaxSteps != nil {
		next.MaxSteps = *patch.MaxSteps
	}
	if err := next.Validate(); err != nil {
		return SessionProfileUpdateResult{}, err
	}
	if equalSessionProfile(next, current) {
		return SessionProfileUpdateResult{Profile: current}, nil
	}
	next.Revision++
	if len(cacheReasons) != 0 {
		next.PromptCacheRevision++
	}
	return SessionProfileUpdateResult{
		Profile: next, PromptCacheReset: len(cacheReasons) != 0,
		ResetReason: strings.Join(cacheReasons, ","),
	}, nil
}

func (c SessionProfileCapabilities) Validate(profile SessionProfile) error {
	if c.Provider != profile.Provider || c.Model != profile.Model {
		return errors.New("session profile capabilities do not match the profile route")
	}
	model := c.ModelCapabilities
	if strings.TrimSpace(model.DisplayName) == "" ||
		len(model.DisplayName) > 256 ||
		strings.ContainsAny(model.DisplayName, "\x00\r\n") ||
		model.ContextWindow == 0 ||
		model.MaxOutputTokens == 0 ||
		model.MaxOutputTokens > model.ContextWindow {
		return errors.New("session model capability identity or limits are invalid")
	}
	if err := model.MetadataProvenance.Validate(); err != nil {
		return err
	}
	if !model.Reasoning &&
		(len(model.ReasoningEfforts) != 0 ||
			model.DefaultReasoningEffort != "" ||
			model.ThinkingToggle) {
		return errors.New("session model reasoning controls require reasoning capability")
	}
	seenEfforts := make(map[string]struct{}, len(model.ReasoningEfforts))
	for _, effort := range model.ReasoningEfforts {
		if !slices.Contains(
			[]string{"none", "off", "minimal", "low", "medium", "high", "xhigh", "max"},
			effort,
		) {
			return fmt.Errorf("session model reasoning effort %q is invalid", effort)
		}
		if _, duplicate := seenEfforts[effort]; duplicate {
			return fmt.Errorf("session model reasoning effort %q is duplicated", effort)
		}
		seenEfforts[effort] = struct{}{}
	}
	if effort := model.DefaultReasoningEffort; effort != "" {
		if _, exists := seenEfforts[effort]; !exists {
			return errors.New(
				"session model default reasoning effort is not advertised",
			)
		}
	}
	if model.AutomaticPromptCache && !model.PromptCache {
		return errors.New(
			"session model automatic prompt cache requires prompt cache capability",
		)
	}
	switch model.ParallelToolCalls {
	case "supported", "unsupported", "unknown":
	default:
		return errors.New("session model parallel tool capability is invalid")
	}
	switch model.CredentialStatus {
	case "configured", "missing", "invalid", "unknown":
	default:
		return errors.New("session model credential status is invalid")
	}
	switch model.Availability {
	case "available", "unavailable":
	default:
		return errors.New("session model availability is invalid")
	}
	if model.Availability == "unavailable" &&
		strings.TrimSpace(model.UnavailableReason) == "" {
		return errors.New("unavailable session model requires a reason")
	}
	switch model.SelectionMode {
	case "hot", "restart_required", "fixed":
	default:
		return errors.New("session model selection mode is invalid")
	}
	for _, field := range c.MutableFields {
		switch field {
		case "provider", "model", "reasoning_effort",
			"enabled_tool_ids", "approval_posture", "execution_target", "max_steps":
		default:
			return fmt.Errorf("unknown mutable session profile field %q", field)
		}
	}
	if !model.Reasoning &&
		(len(model.ReasoningEfforts) != 0 ||
			model.DefaultReasoningEffort != "") {
		return errors.New("non-reasoning model cannot advertise reasoning efforts")
	}
	if model.DefaultReasoningEffort != "" &&
		!slices.Contains(model.ReasoningEfforts, model.DefaultReasoningEffort) {
		return errors.New("default reasoning effort is not advertised")
	}
	return nil
}

func validProfileIdentifier(value string) bool {
	return value != "" && len(value) <= 256 &&
		!strings.ContainsAny(value, "\x00\r\n")
}

// sortedToolIDs returns a sorted copy of tool ids so enabled_tool_ids is
// compared as a set: reordering the same tools never changes profile equality
// or the PromptCacheRevision, because tool-set order is not part of the
// projected prompt prefix (tool definitions are canonically sorted upstream).
func sortedToolIDs(ids []string) []string {
	if ids == nil {
		return nil
	}
	result := append([]string(nil), ids...)
	slices.Sort(result)
	return result
}

func equalSessionProfile(left, right SessionProfile) bool {
	return left.Version == right.Version &&
		left.Revision == right.Revision &&
		left.Mode == right.Mode && left.PlanningPolicy == right.PlanningPolicy &&
		left.Provider == right.Provider &&
		left.Model == right.Model &&
		left.ReasoningEffort == right.ReasoningEffort &&
		slices.Equal(
			sortedToolIDs(left.EnabledToolIDs),
			sortedToolIDs(right.EnabledToolIDs),
		) &&
		left.ApprovalPosture == right.ApprovalPosture &&
		left.ExecutionTarget == right.ExecutionTarget &&
		left.MaxSteps == right.MaxSteps &&
		left.PromptCacheRevision == right.PromptCacheRevision
}
