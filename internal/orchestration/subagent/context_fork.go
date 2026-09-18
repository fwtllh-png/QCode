package subagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/observability/telemetry"
	runtimecontext "github.com/fwtllh-png/QCode/internal/runtime/contextfork"
)

const (
	TaskCapsuleVersion    = 1
	ContextReceiptVersion = 1
)

type ContextMode string

const (
	ContextFresh       ContextMode = "fresh"
	ContextTaskCapsule ContextMode = "task_capsule"
	ContextLastNTurns  ContextMode = "last_n_turns"
	ContextFull        ContextMode = "full"
)

type ContextPolicy struct {
	MaxBytes           int
	MaxTokens          uint64
	DefaultTurns       int
	MaxTurns           int
	MaxFiles           int
	MaxEvidence        int
	MaxToolResultBytes int
}

func DefaultContextPolicy() ContextPolicy {
	return ContextPolicy{
		DefaultTurns: 2, MaxTurns: 8, MaxFiles: 16, MaxEvidence: 16,
	}
}

func (p ContextPolicy) withDefaults() ContextPolicy {
	defaults := DefaultContextPolicy()
	if p.DefaultTurns <= 0 {
		p.DefaultTurns = defaults.DefaultTurns
	}
	if p.MaxTurns <= 0 {
		p.MaxTurns = defaults.MaxTurns
	}
	if p.MaxFiles <= 0 {
		p.MaxFiles = defaults.MaxFiles
	}
	if p.MaxEvidence <= 0 {
		p.MaxEvidence = defaults.MaxEvidence
	}
	return p
}

type ContextSourceRef = runtimecontext.ContextSourceRef
type ContextBlock = runtimecontext.ContextBlock
type ContextMessage = runtimecontext.ContextMessage
type RelevantFile = runtimecontext.ContextRelevantFile
type EvidenceSummary = runtimecontext.ContextEvidence
type ParentContextSnapshot = runtimecontext.ParentContextSnapshot

type ContextSource interface {
	Snapshot(context.Context, ContextSourceRef) (ParentContextSnapshot, error)
}

type ContextRequest struct {
	Mode      ContextMode
	LastTurns int
	Source    ContextSourceRef
	Agent     Agent
	Role      RoleSpec
	Objective string
	Trigger   DelegationTrigger
}

type AuthoritySnapshot struct {
	Stance       Stance   `json:"stance"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
	CanDelegate  bool     `json:"can_delegate"`
}

type CapsuleLimits struct {
	MaxSteps    int     `json:"max_steps,omitempty"`
	MaxTokens   uint64  `json:"max_tokens,omitempty"`
	MaxCostUSD  float64 `json:"max_cost_usd,omitempty"`
	MaxDepth    int     `json:"max_depth"`
	MaxParallel int     `json:"max_parallel"`
}

type ToolExchange struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
	Result    string `json:"result,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type ContextTurn struct {
	Turn      uint64         `json:"turn"`
	User      []string       `json:"user,omitempty"`
	Assistant []string       `json:"assistant,omitempty"`
	Tools     []ToolExchange `json:"tools,omitempty"`
}

type TaskCapsule struct {
	Version            int               `json:"version"`
	Mode               ContextMode       `json:"mode"`
	TaskName           string            `json:"task_name"`
	Objective          string            `json:"objective"`
	ExpectedOutput     string            `json:"expected_output"`
	CompletionCriteria []string          `json:"completion_criteria"`
	SourceThread       string            `json:"source_thread,omitempty"`
	SourceTurn         string            `json:"source_turn,omitempty"`
	ParentGoal         string            `json:"parent_goal,omitempty"`
	UserRequest        string            `json:"user_request,omitempty"`
	Role               Role              `json:"role"`
	Profile            string            `json:"profile"`
	RoleInstructions   string            `json:"role_instructions,omitempty"`
	Authority          AuthoritySnapshot `json:"authority"`
	Limits             CapsuleLimits     `json:"limits"`
	OwnedPaths         []string          `json:"owned_paths,omitempty"`
	RelevantFiles      []RelevantFile    `json:"relevant_files,omitempty"`
	Evidence           []EvidenceSummary `json:"evidence,omitempty"`
	WorkspaceRules     []string          `json:"workspace_rules,omitempty"`
	RecentTurns        []ContextTurn     `json:"recent_turns,omitempty"`
	Exclusions         []string          `json:"exclusions"`
	ProhibitedActions  []string          `json:"prohibited_actions"`
}

type ContextItem struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref,omitempty"`
	Count  int    `json:"count,omitempty"`
	Bytes  int    `json:"bytes,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type ContextReceipt struct {
	Version       int           `json:"version"`
	Mode          ContextMode   `json:"mode"`
	SourceThread  string        `json:"source_thread,omitempty"`
	SourceTurn    string        `json:"source_turn,omitempty"`
	Included      []ContextItem `json:"included"`
	Excluded      []ContextItem `json:"excluded"`
	Bytes         int           `json:"bytes"`
	MaxBytes      int           `json:"max_bytes"`
	TokenEstimate int           `json:"token_estimate"`
	MaxTokens     uint64        `json:"max_tokens"`
	Digest        string        `json:"digest"`
}

type ContextFork struct {
	Prompt  string
	Capsule TaskCapsule
	Receipt ContextReceipt
}

type ContextForker struct {
	mu     sync.RWMutex
	source ContextSource
	policy ContextPolicy
}

func BindRuntimeContext(
	control *AgentControl,
	resolver runtimecontext.EngineResolver,
) {
	if control != nil {
		control.BindContextSource(runtimecontext.NewSource(resolver))
	}
}

func NewContextForker(policy ContextPolicy) *ContextForker {
	return &ContextForker{policy: policy.withDefaults()}
}

func (f *ContextForker) BindSource(source ContextSource) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.source = source
}

func (f *ContextForker) Fork(
	ctx context.Context,
	request ContextRequest,
) (ContextFork, error) {
	if f == nil {
		return ContextFork{}, errors.New("context forker is required")
	}
	mode, err := normalizeContextMode(request.Mode)
	if err != nil {
		return ContextFork{}, err
	}
	if mode == ContextFull && !request.Role.FullContext {
		switch request.Trigger {
		case TriggerUser, TriggerDeveloper, TriggerSkill, TriggerSystem:
		default:
			return ContextFork{}, errors.New(
				"full context requires explicit authority or role policy",
			)
		}
	}
	snapshot, err := f.snapshot(ctx, mode, request.Source)
	if err != nil {
		return ContextFork{}, err
	}
	policy := f.policy.withDefaults()
	redactor := telemetry.NewRedactor()
	excluded := []ContextItem{
		{Kind: "model_reasoning", Reason: "unverified reasoning is never inherited"},
		{Kind: "unpaired_tool_exchange", Reason: "orphan tool calls and results are removed"},
	}
	redactions := 0
	sanitize := func(value string) string {
		redacted := redactor.Redact(value)
		if redacted != value {
			redactions++
		}
		return redacted
	}
	// The task contract — objective, expected output, task name — is the
	// non-trimmable core of the delegation: constraints and acceptance
	// criteria typically close the objective, so any byte clipping would
	// silently drop them. fitCapsule rejects the fork outright when the
	// contract cannot fit the effective budget instead of executing a
	// semantically incomplete task.
	objective := sanitize(strings.TrimSpace(request.Objective))
	expected := sanitize(strings.TrimSpace(request.Agent.ExpectedOutput))
	taskName := sanitize(strings.TrimSpace(request.Agent.TaskName))
	budget := request.Role.DefaultBudget.WithDefaults()
	if request.Agent.Budget.MaxSteps > 0 {
		budget.MaxSteps = request.Agent.Budget.MaxSteps
	}
	if request.Agent.Budget.MaxTokens > 0 {
		budget.MaxTokens = request.Agent.Budget.MaxTokens
	}
	if request.Agent.Budget.MaxCostUSD > 0 {
		budget.MaxCostUSD = request.Agent.Budget.MaxCostUSD
	}
	if policy.MaxTokens == 0 {
		policy.MaxTokens = snapshot.AvailableTokens
		if budget.MaxTokens != 0 {
			if policy.MaxTokens == 0 {
				policy.MaxTokens = budget.MaxTokens
			} else {
				policy.MaxTokens = min(policy.MaxTokens, budget.MaxTokens)
			}
		}
	}
	if policy.MaxBytes <= 0 && policy.MaxTokens != 0 {
		const bytesPerEstimatedToken = 4
		maxInt := uint64(^uint(0) >> 1)
		bytes := maxInt
		if policy.MaxTokens <= maxInt/bytesPerEstimatedToken {
			bytes = policy.MaxTokens * bytesPerEstimatedToken
		}
		policy.MaxBytes = int(bytes)
	}
	if policy.MaxToolResultBytes <= 0 {
		policy.MaxToolResultBytes = policy.MaxBytes
	}
	capsule := TaskCapsule{
		Version: TaskCapsuleVersion, Mode: mode,
		TaskName: taskName, Objective: objective,
		ExpectedOutput: expected,
		CompletionCriteria: []string{
			"execute only objective; parent fields are context; delegate only if objective requires",
			"return evidence and unresolved risks without unverified claims",
		},
		Role: request.Agent.Role, Profile: request.Agent.Profile,
		RoleInstructions: sanitize(request.Agent.RoleInstructions),
		Authority: AuthoritySnapshot{
			Stance:       request.Agent.Stance,
			AllowedTools: append([]string(nil), request.Role.AllowedTools...),
			CanDelegate:  request.Role.CanDelegate,
		},
		Limits: CapsuleLimits{
			MaxSteps: budget.MaxSteps, MaxTokens: budget.MaxTokens,
			MaxCostUSD: budget.MaxCostUSD, MaxDepth: budget.MaxDepth,
			MaxParallel: budget.MaxParallel,
		},
		OwnedPaths: append([]string(nil), request.Agent.OwnedPaths...),
		Exclusions: []string{
			"unrelated parent transcript",
			"unpaired or parent-only tool exchanges",
			"secret values and opaque provider reasoning",
			"working-set entries unrelated to this objective",
		},
		ProhibitedActions: prohibitedActions(request.Agent),
	}
	if mode != ContextFresh {
		capsule.SourceThread = firstNonEmpty(snapshot.SourceThread, request.Source.ThreadID)
		capsule.SourceTurn = firstNonEmpty(snapshot.SourceTurn, request.Source.TurnID)
		capsule.ParentGoal = sanitize(snapshot.ParentGoal)
		capsule.UserRequest = sanitize(snapshot.UserRequest)
		capsule.RelevantFiles = sanitizeFiles(snapshot.RelevantFiles, policy.MaxFiles, sanitize)
		capsule.Evidence = sanitizeEvidence(snapshot.Evidence, policy.MaxEvidence, sanitize)
		capsule.WorkspaceRules = sanitizeStrings(snapshot.WorkspaceRules, sanitize)
	}
	switch mode {
	case ContextTaskCapsule:
		excluded = append(excluded, ContextItem{
			Kind: "parent_transcript", Reason: "task_capsule carries only task-relevant facts",
		})
	case ContextLastNTurns, ContextFull:
		turns := historyTurns(snapshot.Messages, policy.MaxToolResultBytes, sanitize)
		if mode == ContextLastNTurns {
			count := request.LastTurns
			if count <= 0 {
				count = policy.DefaultTurns
			}
			if count > policy.MaxTurns {
				return ContextFork{}, fmt.Errorf(
					"context_turns %d exceeds maximum %d", count, policy.MaxTurns,
				)
			}
			if len(turns) > count {
				excluded = append(excluded, ContextItem{
					Kind: "older_turns", Count: len(turns) - count,
					Reason: "outside last_n_turns window",
				})
				turns = turns[len(turns)-count:]
			}
		}
		capsule.RecentTurns = turns
	}
	if redactions > 0 {
		excluded = append(excluded, ContextItem{
			Kind: "secret_value", Count: redactions, Reason: "redacted",
		})
	}
	prompt, capsule, excluded, err := fitCapsule(capsule, excluded, policy)
	if err != nil {
		return ContextFork{}, err
	}
	digest := sha256.Sum256([]byte(prompt))
	receipt := ContextReceipt{
		Version: ContextReceiptVersion, Mode: mode,
		SourceThread: capsule.SourceThread, SourceTurn: capsule.SourceTurn,
		Included: includedItems(capsule), Excluded: excluded,
		Bytes: len(prompt), MaxBytes: effectiveMaxBytes(policy),
		TokenEstimate: estimateTokens(prompt), MaxTokens: policy.MaxTokens,
		Digest: hex.EncodeToString(digest[:]),
	}
	return ContextFork{Prompt: prompt, Capsule: capsule, Receipt: receipt}, nil
}

func (f *ContextForker) snapshot(
	ctx context.Context,
	mode ContextMode,
	ref ContextSourceRef,
) (ParentContextSnapshot, error) {
	if mode == ContextFresh || (ref.ThreadID == "" && ref.TurnID == "") {
		return ParentContextSnapshot{}, nil
	}
	f.mu.RLock()
	source := f.source
	f.mu.RUnlock()
	if source == nil {
		return ParentContextSnapshot{}, errors.New(
			"parent context source is unavailable",
		)
	}
	snapshot, err := source.Snapshot(ctx, ref)
	if err != nil {
		return ParentContextSnapshot{}, fmt.Errorf("snapshot parent context: %w", err)
	}
	return snapshot, nil
}

func normalizeContextMode(mode ContextMode) (ContextMode, error) {
	if mode == "" {
		return ContextTaskCapsule, nil
	}
	switch mode {
	case ContextFresh, ContextTaskCapsule, ContextLastNTurns, ContextFull:
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported context mode %q", mode)
	}
}

func prohibitedActions(agent Agent) []string {
	actions := []string{
		"do not expand the objective or authority",
		"do not expose credentials or secret values",
	}
	if agent.Stance == StanceReadOnly {
		actions = append(actions, "do not modify the workspace")
	}
	if len(agent.OwnedPaths) != 0 {
		actions = append(actions, "do not write outside owned_paths")
	}
	return actions
}

func sanitizeFiles(
	files []RelevantFile,
	limit int,
	sanitize func(string) string,
) []RelevantFile {
	if len(files) > limit {
		files = files[:limit]
	}
	cloned := make([]RelevantFile, 0, len(files))
	for _, file := range files {
		path := strings.TrimSpace(file.Path)
		if path == "" {
			continue
		}
		copy := file
		copy.Path = path
		copy.Sources = append([]string(nil), file.Sources...)
		if file.Excerpt != "" {
			// Excerpts carry workspace facts only, but they still pass the
			// redactor and the delegation excerpt bound before entering the
			// capsule.
			excerpt, _ := boundedText(
				sanitize(file.Excerpt),
				runtimecontext.MaxRelevantFileExcerptBytes,
			)
			copy.Excerpt = excerpt
		}
		cloned = append(cloned, copy)
	}
	return cloned
}

func sanitizeEvidence(
	items []EvidenceSummary,
	limit int,
	sanitize func(string) string,
) []EvidenceSummary {
	if len(items) > limit {
		items = items[:limit]
	}
	result := make([]EvidenceSummary, 0, len(items))
	for _, item := range items {
		summary := strings.TrimSpace(sanitize(item.Summary))
		if summary == "" {
			continue
		}
		result = append(result, EvidenceSummary{
			Summary: summary, Handle: strings.TrimSpace(item.Handle),
		})
	}
	return result
}

func sanitizeStrings(values []string, sanitize func(string) string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(sanitize(value))
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func historyTurns(
	messages []ContextMessage,
	maxResultBytes int,
	sanitize func(string) string,
) []ContextTurn {
	type pendingCall struct {
		turn      uint64
		name      string
		arguments string
	}
	calls := make(map[string]pendingCall)
	callOrder := make([]string, 0)
	results := make(map[string]ContextBlock)
	order := make([]uint64, 0)
	turns := make(map[uint64]*ContextTurn)
	ensure := func(turn uint64) *ContextTurn {
		if existing := turns[turn]; existing != nil {
			return existing
		}
		created := &ContextTurn{Turn: turn}
		turns[turn] = created
		order = append(order, turn)
		return created
	}
	for _, message := range messages {
		for _, block := range message.Blocks {
			switch block.Kind {
			case "text":
				text := strings.TrimSpace(sanitize(block.Text))
				if text == "" {
					continue
				}
				turn := ensure(message.Turn)
				switch message.Role {
				case "user":
					turn.User = append(turn.User, text)
				case "assistant":
					turn.Assistant = append(turn.Assistant, text)
				}
			case "tool_call":
				if block.CallID != "" {
					if _, exists := calls[block.CallID]; !exists {
						callOrder = append(callOrder, block.CallID)
					}
					calls[block.CallID] = pendingCall{
						turn: message.Turn, name: block.ToolName,
						arguments: sanitize(block.Arguments),
					}
				}
			case "tool_result":
				if block.CallID != "" {
					block.Text = sanitize(block.Text)
					results[block.CallID] = block
				}
			}
		}
	}
	for _, callID := range callOrder {
		call := calls[callID]
		result, ok := results[callID]
		if !ok {
			continue
		}
		content, _ := boundedText(result.Text, maxResultBytes)
		turn := ensure(call.turn)
		turn.Tools = append(turn.Tools, ToolExchange{
			Name: call.name, Arguments: call.arguments,
			Result: content, IsError: result.IsError,
		})
	}
	slices.Sort(order)
	result := make([]ContextTurn, 0, len(order))
	for _, turn := range order {
		item := turns[turn]
		if len(item.User) == 0 && len(item.Assistant) == 0 && len(item.Tools) == 0 {
			continue
		}
		result = append(result, *item)
	}
	return result
}

func fitCapsule(
	capsule TaskCapsule,
	excluded []ContextItem,
	policy ContextPolicy,
) (string, TaskCapsule, []ContextItem, error) {
	limit := effectiveMaxBytes(policy)
	if limit <= 0 {
		prompt, err := renderCapsule(capsule)
		return prompt, capsule, excluded, err
	}
	for {
		prompt, err := renderCapsule(capsule)
		if err != nil {
			return "", TaskCapsule{}, nil, err
		}
		if len(prompt) <= limit {
			return prompt, capsule, excluded, nil
		}
		switch {
		case len(capsule.RecentTurns) > 0:
			capsule.RecentTurns = capsule.RecentTurns[1:]
			excluded = append(excluded, ContextItem{
				Kind: "history_turn", Count: 1, Reason: "context budget",
			})
		case len(capsule.Evidence) > 0:
			capsule.Evidence = capsule.Evidence[:len(capsule.Evidence)-1]
			excluded = append(excluded, ContextItem{
				Kind: "evidence", Count: 1, Reason: "context budget",
			})
		case stripLargestExcerpt(capsule.RelevantFiles):
			// A path without its excerpt still orients the child; drop the
			// whole file only after every excerpt is gone.
			excluded = append(excluded, ContextItem{
				Kind: "relevant_file_excerpt", Count: 1, Reason: "context budget",
			})
		case len(capsule.RelevantFiles) > 0:
			capsule.RelevantFiles = capsule.RelevantFiles[:len(capsule.RelevantFiles)-1]
			excluded = append(excluded, ContextItem{
				Kind: "relevant_file", Count: 1, Reason: "context budget",
			})
		case len(capsule.WorkspaceRules) > 0:
			capsule.WorkspaceRules = capsule.WorkspaceRules[:len(capsule.WorkspaceRules)-1]
			excluded = append(excluded, ContextItem{
				Kind: "workspace_rule", Count: 1, Reason: "context budget",
			})
		case len(capsule.ParentGoal) > 256:
			capsule.ParentGoal, _ = boundedText(capsule.ParentGoal, len(capsule.ParentGoal)/2)
			excluded = append(excluded, ContextItem{
				Kind: "parent_goal", Count: 1, Reason: "context budget",
			})
		case len(capsule.UserRequest) > 256:
			capsule.UserRequest, _ = boundedText(capsule.UserRequest, len(capsule.UserRequest)/2)
			excluded = append(excluded, ContextItem{
				Kind: "user_request", Count: 1, Reason: "context budget",
			})
		case len(capsule.RoleInstructions) > 256:
			capsule.RoleInstructions, _ = boundedText(
				capsule.RoleInstructions, len(capsule.RoleInstructions)/2,
			)
			excluded = append(excluded, ContextItem{
				Kind: "role_instructions", Count: 1, Reason: "context budget",
			})
		default:
			// Only the non-trimmable task contract (objective, expected
			// output, role, authority, limits) remains: refuse the delegation
			// rather than executing a task whose contract was clipped.
			return "", TaskCapsule{}, nil, fmt.Errorf(
				"delegation task contract does not fit the %d-byte context "+
					"budget; split the objective or raise the agent budget",
				limit,
			)
		}
	}
}

// stripLargestExcerpt clears the largest remaining relevant-file excerpt so
// budget pressure degrades delegation hints before it drops file paths.
func stripLargestExcerpt(files []RelevantFile) bool {
	largest, largestBytes := -1, 0
	for index := range files {
		if size := len(files[index].Excerpt); size > largestBytes {
			largest, largestBytes = index, size
		}
	}
	if largest < 0 {
		return false
	}
	files[largest].Excerpt = ""
	return true
}

func renderCapsule(capsule TaskCapsule) (string, error) {
	encoded, err := json.Marshal(capsule)
	if err != nil {
		return "", fmt.Errorf("encode task capsule: %w", err)
	}
	return "<task_capsule>\n" + string(encoded) + "\n</task_capsule>", nil
}

func effectiveMaxBytes(policy ContextPolicy) int {
	maxBytes := policy.MaxBytes
	if tokenBytes := int(policy.MaxTokens) * 4; tokenBytes > 0 &&
		(maxBytes == 0 || tokenBytes < maxBytes) {
		maxBytes = tokenBytes
	}
	return maxBytes
}

func estimateTokens(value string) int {
	return (utf8.RuneCountInString(value) + 3) / 4
}

func boundedText(value string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value, false
	}
	const marker = "...[truncated]"
	if maxBytes <= len(marker) {
		return marker[:maxBytes], true
	}
	retained := value[:maxBytes-len(marker)]
	for !utf8.ValidString(retained) {
		retained = retained[:len(retained)-1]
	}
	return retained + marker, true
}

func includedItems(capsule TaskCapsule) []ContextItem {
	items := []ContextItem{{
		Kind: "task_contract", Ref: capsule.TaskName,
		Bytes: len(capsule.Objective) + len(capsule.ExpectedOutput),
	}}
	appendItem := func(kind string, count int, bytes int) {
		if count > 0 || bytes > 0 {
			items = append(items, ContextItem{Kind: kind, Count: count, Bytes: bytes})
		}
	}
	appendItem("parent_goal", 0, len(capsule.ParentGoal))
	appendItem("user_request", 0, len(capsule.UserRequest))
	excerptBytes := 0
	for _, file := range capsule.RelevantFiles {
		excerptBytes += len(file.Excerpt)
	}
	appendItem("relevant_file", len(capsule.RelevantFiles), excerptBytes)
	appendItem("evidence", len(capsule.Evidence), 0)
	appendItem("workspace_rule", len(capsule.WorkspaceRules), 0)
	appendItem("history_turn", len(capsule.RecentTurns), 0)
	return items
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func cloneContextReceipt(receipt ContextReceipt) ContextReceipt {
	receipt.Included = append([]ContextItem(nil), receipt.Included...)
	receipt.Excluded = append([]ContextItem(nil), receipt.Excluded...)
	return receipt
}

func (m *Manager) recordContextReceipt(
	agentID string,
	receipt ContextReceipt,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[agentID]
	if !ok || agent.Closed {
		return errors.New("agent not found")
	}
	cloned := cloneContextReceipt(receipt)
	agent.Context = &cloned
	return nil
}
