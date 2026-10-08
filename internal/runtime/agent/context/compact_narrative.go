package agentcontext

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const (
	NarrativeSchemaVersion = 1
	NarrativePrivacyClass  = "conversation_content"

	NarrativeDecision   = "decision"
	NarrativeRationale  = "rationale"
	NarrativePreference = "preference"
	NarrativeUnresolved = "unresolved"
	NarrativeTechnical  = "technical_concept"
	NarrativeFileCode   = "file_and_code"
	NarrativeErrorFix   = "error_and_fix"
	NarrativePendingJob = "pending_job"
	NarrativeCurrent    = "current_work"
	NarrativeNextStep   = "next_step"
	NarrativeCritical   = "critical_context"
)

type NarrativeExcerpt struct {
	MessageID string                `json:"message_id"`
	Role      provider.Role         `json:"role"`
	Turn      uint64                `json:"turn,omitempty"`
	Text      string                `json:"text"`
	Digest    string                `json:"digest"`
	Truncated bool                  `json:"truncated,omitempty"`
	Source    *NarrativeSourceRange `json:"source,omitempty"`
}

type NarrativeInputArtifact struct {
	Version         int                `json:"version"`
	ThreadID        protocol.ThreadID  `json:"thread_id"`
	SourceWindowID  string             `json:"source_window_id"`
	AuthorityDigest string             `json:"authority_digest"`
	RouteDigest     string             `json:"route_digest"`
	PrivacyClass    string             `json:"privacy_class"`
	RequiredKinds   []string           `json:"required_kinds,omitempty"`
	Excerpts        []NarrativeExcerpt `json:"excerpts"`
	Digest          string             `json:"digest"`
	ExpiresAt       time.Time          `json:"expires_at"`
	CompleteSources bool               `json:"complete_sources,omitempty"`
}

type NarrativeItem struct {
	ID               string   `json:"id"`
	Kind             string   `json:"kind"`
	Text             string   `json:"text"`
	SourceMessageIDs []string `json:"source_message_ids"`
	SourceDigest     string   `json:"source_digest"`
	CreatedTurn      uint64   `json:"created_turn"`
}

type NarrativeArtifact struct {
	Version         int                 `json:"version"`
	ThreadID        protocol.ThreadID   `json:"thread_id"`
	WindowID        string              `json:"window_id"`
	AuthorityDigest string              `json:"authority_digest"`
	InputDigest     string              `json:"input_digest"`
	RouteDigest     string              `json:"route_digest"`
	Body            Narrative           `json:"body"`
	Coverage        []NarrativeCoverage `json:"coverage,omitempty"`
	CreatedAt       time.Time           `json:"created_at"`
	ExpiresAt       time.Time           `json:"expires_at"`
	Digest          string              `json:"digest"`
}

type CompactedContext struct {
	Version             int                `json:"version"`
	CompactionID        string             `json:"compaction_id"`
	ThreadID            protocol.ThreadID  `json:"thread_id"`
	TurnID              protocol.TurnID    `json:"turn_id"`
	SourceWindowID      string             `json:"source_window_id"`
	TargetWindowID      string             `json:"target_window_id"`
	SourceContextDigest string             `json:"source_context_digest"`
	StablePrefixDigest  string             `json:"stable_prefix_digest"`
	Truth               TruthCapsule       `json:"truth"`
	Head                provider.Message   `json:"head"`
	Narrative           *NarrativeArtifact `json:"narrative,omitempty"`
	Tail                []provider.Message `json:"tail"`
	TailTurns           []uint64           `json:"tail_turns,omitempty"`
	AuthorityDigest     string             `json:"authority_digest"`
	NarrativeDigest     string             `json:"narrative_digest,omitempty"`
	TailDigest          string             `json:"tail_digest"`
	Digest              string             `json:"digest"`
}

type PreparedCompaction struct {
	Version             int                     `json:"version"`
	ID                  string                  `json:"id"`
	Phase               string                  `json:"phase"`
	Trigger             string                  `json:"trigger"`
	SourceWindowID      string                  `json:"source_window_id"`
	TargetWindowID      string                  `json:"target_window_id"`
	TargetWindow        WindowLedger            `json:"target_window"`
	SourceContextDigest string                  `json:"source_context_digest"`
	SourceHistoryDigest string                  `json:"source_history_digest"`
	SourceBytes         int                     `json:"source_bytes"`
	SourceTokens        uint64                  `json:"source_tokens"`
	RetainedTokens      uint64                  `json:"retained_tokens"`
	Cut                 int                     `json:"cut"`
	RemovedMessageIDs   []string                `json:"removed_message_ids"`
	TailMessageIDs      []string                `json:"tail_message_ids"`
	RequiredKinds       []string                `json:"required_kinds,omitempty"`
	Truth               TruthCapsule            `json:"truth"`
	NarrativeInput      *NarrativeInputArtifact `json:"narrative_input,omitempty"`
	DeterministicResult CompactedContext        `json:"deterministic_result"`
	Digest              string                  `json:"digest"`
}

type CompactionPlan = PreparedCompaction

func (p *PreparedCompaction) Seal() error {
	if p == nil {
		return errors.New("compaction plan is nil")
	}
	p.Version = NarrativeSchemaVersion
	p.RemovedMessageIDs = append([]string(nil), p.RemovedMessageIDs...)
	p.RequiredKinds = append([]string(nil), p.RequiredKinds...)
	if len(p.DeterministicResult.TailTurns) !=
		len(p.DeterministicResult.Tail) {
		return errors.New("compaction plan tail turn count is invalid")
	}
	p.TailMessageIDs = append([]string(nil), p.TailMessageIDs...)
	p.Digest = p.digest()
	return p.Validate()
}

func (p PreparedCompaction) Validate() error {
	if p.Version != NarrativeSchemaVersion || p.ID == "" ||
		p.Phase == "" || p.Trigger == "" ||
		p.SourceWindowID == "" || p.TargetWindowID == "" ||
		p.SourceContextDigest == "" || p.SourceHistoryDigest == "" ||
		p.SourceBytes < 1 || p.Cut < 1 ||
		len(p.RemovedMessageIDs) != p.Cut ||
		p.Digest == "" || p.Digest != p.digest() {
		return errors.New("compaction plan identity or digest is invalid")
	}
	if !p.TargetWindow.Valid() || p.TargetWindow.ID != p.TargetWindowID {
		return errors.New("compaction plan target window is invalid")
	}
	if p.SourceTokens != 0 && p.RetainedTokens >= p.SourceTokens {
		return errors.New("compaction plan does not reduce source tokens")
	}
	if err := p.Truth.Validate(); err != nil {
		return err
	}
	if p.NarrativeInput != nil {
		if err := p.NarrativeInput.Validate(time.Time{}); err != nil {
			return err
		}
	}
	if err := p.DeterministicResult.Validate(); err != nil {
		return err
	}
	if p.DeterministicResult.CompactionID != p.ID ||
		p.DeterministicResult.SourceWindowID != p.SourceWindowID ||
		p.DeterministicResult.TargetWindowID != p.TargetWindowID ||
		p.DeterministicResult.SourceContextDigest != p.SourceContextDigest ||
		p.DeterministicResult.Truth.Digest != p.Truth.Digest {
		return errors.New("compaction plan deterministic result fence is invalid")
	}
	if p.NarrativeInput != nil &&
		(p.NarrativeInput.ThreadID != p.DeterministicResult.ThreadID ||
			p.NarrativeInput.SourceWindowID != p.SourceWindowID) {
		return errors.New("compaction plan narrative input fence is invalid")
	}
	authorityDigest, err := p.Truth.AuthorityDigest()
	if err != nil || p.DeterministicResult.AuthorityDigest != authorityDigest {
		return errors.New("compaction plan authority fence is invalid")
	}
	if p.NarrativeInput != nil &&
		p.NarrativeInput.AuthorityDigest != authorityDigest {
		return errors.New("compaction plan narrative authority fence is invalid")
	}
	return nil
}

func (p PreparedCompaction) digest() string {
	p.Digest = ""
	encoded, _ := json.Marshal(p)
	return digestString(string(encoded))
}

type NarrativeLimits struct {
	MaxInputTokens  uint64
	MaxOutputTokens uint64
	MaxInputBytes   int
	// MaxOutputBytes is an optional operator ceiling. Zero means the
	// summary route's advertised MaxOutputTokens and remaining context
	// window decide the request budget.
	MaxOutputBytes  int
	MaxItems        int
	ItemMaxBytes    int
	ExcerptMaxBytes int
}

func BuildNarrativeInput(
	threadID protocol.ThreadID,
	sourceWindowID string,
	authorityDigest string,
	routeDigest string,
	removed []provider.Message,
	limits NarrativeLimits,
	now time.Time,
	ttl time.Duration,
	requiredKinds ...[]string,
) (NarrativeInputArtifact, error) {
	if threadID == "" || sourceWindowID == "" ||
		authorityDigest == "" || routeDigest == "" {
		return NarrativeInputArtifact{}, errors.New("narrative input identity is incomplete")
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	artifact := NarrativeInputArtifact{
		Version: NarrativeSchemaVersion, ThreadID: threadID, CompleteSources: true,
		SourceWindowID: sourceWindowID, AuthorityDigest: authorityDigest,
		RouteDigest: routeDigest, PrivacyClass: NarrativePrivacyClass,
		ExpiresAt: now.UTC().Add(ttl),
	}
	if len(requiredKinds) != 0 {
		artifact.RequiredKinds = append([]string(nil), requiredKinds[0]...)
	}
	calls := make(map[string]provider.ToolCall)
	for _, message := range removed {
		for _, block := range message.Blocks {
			if block.ToolCall != nil {
				calls[block.ToolCall.ID] = *block.ToolCall
			}
		}
	}
	// Index within each turn, so dropping earlier complete turns does not
	// change the identity of unchanged sources.
	indices := make(map[uint64]int)
	for _, message := range removed {
		if IsWorldStateMessage(message) {
			continue
		}
		index := indices[message.Turn]
		indices[message.Turn]++
		if message.Role != provider.RoleUser && message.Role != provider.RoleAssistant && message.Role != provider.RoleTool {
			continue
		}
		body := narrativeMessageText(message, calls)
		if body == "" {
			continue
		}
		id := StableMessageID(threadID, message, index)
		artifact.Excerpts = append(artifact.Excerpts, narrativeSourceExcerpts(id, message.Role, message.Turn, body, limits.ExcerptMaxBytes)...)
	}
	artifact.Digest = artifact.digest()
	return artifact, artifact.Validate(now)
}

func (a NarrativeInputArtifact) Validate(now time.Time) error {
	if a.Version != NarrativeSchemaVersion || a.ThreadID == "" ||
		a.SourceWindowID == "" || a.AuthorityDigest == "" ||
		a.RouteDigest == "" ||
		a.PrivacyClass != NarrativePrivacyClass ||
		a.Digest == "" || a.Digest != a.digest() ||
		a.ExpiresAt.IsZero() {
		return errors.New("narrative input artifact identity or digest is invalid")
	}
	if !now.IsZero() && !a.ExpiresAt.After(now) {
		return errors.New("narrative input artifact is stale")
	}
	seen := make(map[string]struct{}, len(a.Excerpts))
	for _, kind := range a.RequiredKinds {
		if !validNarrativeKind(kind) {
			return errors.New("narrative required kind is invalid")
		}
	}
	if !sort.StringsAreSorted(a.RequiredKinds) ||
		len(deduplicateStrings(a.RequiredKinds)) != len(a.RequiredKinds) {
		return errors.New("narrative required kinds are not canonical")
	}
	for _, excerpt := range a.Excerpts {
		if excerpt.MessageID == "" || excerpt.Text == "" ||
			excerpt.Digest != digestString(excerpt.Text) ||
			!utf8.ValidString(excerpt.Text) ||
			excerpt.Role != provider.RoleUser &&
				excerpt.Role != provider.RoleAssistant &&
				excerpt.Role != provider.RoleTool {
			return errors.New("narrative excerpt is invalid")
		}
		if err := validateNarrativeRange(excerpt); err != nil {
			return err
		}
		if _, duplicate := seen[excerpt.MessageID]; duplicate {
			return errors.New("narrative excerpt source is duplicated")
		}
		seen[excerpt.MessageID] = struct{}{}
	}
	return validateCompleteNarrativeSources(a)
}

func (a NarrativeInputArtifact) digest() string {
	a.Digest = ""
	encoded, _ := json.Marshal(a)
	return digestString(string(encoded))
}

func ValidateNarrativeJSON(
	raw []byte,
	input NarrativeInputArtifact,
	limits NarrativeLimits,
	createdTurn uint64,
	now time.Time,
) (NarrativeArtifact, error) {
	if err := input.Validate(now); err != nil {
		return NarrativeArtifact{}, err
	}
	if len(raw) == 0 || !utf8.Valid(raw) ||
		limits.MaxOutputBytes > 0 && len(raw) > limits.MaxOutputBytes {
		return NarrativeArtifact{}, errors.New("narrative output size or encoding is invalid")
	}
	var payload struct {
		TechnicalConcepts *[]narrativeJSONItem `json:"technical_concepts"`
		FilesAndCode      *[]narrativeJSONItem `json:"files_and_code"`
		ErrorsAndFixes    *[]narrativeJSONItem `json:"errors_and_fixes"`
		PendingJobs       *[]narrativeJSONItem `json:"pending_jobs"`
		CurrentWork       *[]narrativeJSONItem `json:"current_work"`
		NextSteps         *[]narrativeJSONItem `json:"next_steps"`
		CriticalContext   *[]narrativeJSONItem `json:"critical_context"`
		Decisions         *[]narrativeJSONItem `json:"decisions"`
		Rationale         *[]narrativeJSONItem `json:"rationale"`
		Preferences       *[]narrativeJSONItem `json:"preferences"`
		Unresolved        *[]narrativeJSONItem `json:"unresolved"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return NarrativeArtifact{}, fmt.Errorf("decode narrative: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return NarrativeArtifact{}, errors.New("narrative has trailing JSON")
		}
		return NarrativeArtifact{}, err
	}
	if payload.TechnicalConcepts == nil || payload.FilesAndCode == nil ||
		payload.ErrorsAndFixes == nil || payload.PendingJobs == nil ||
		payload.CurrentWork == nil || payload.NextSteps == nil ||
		payload.CriticalContext == nil || payload.Decisions == nil ||
		payload.Rationale == nil || payload.Preferences == nil ||
		payload.Unresolved == nil {
		return NarrativeArtifact{},
			errors.New("narrative output is missing a required array")
	}
	sources := make(map[string]NarrativeExcerpt, len(input.Excerpts))
	for _, excerpt := range input.Excerpts {
		sources[excerpt.MessageID] = excerpt
	}
	var items []NarrativeItem
	counts := make(map[string]int)
	appendItems := func(kind string, values []narrativeJSONItem) error {
		for _, value := range values {
			text := strings.TrimSpace(value.Text)
			if text == "" || (limits.ItemMaxBytes > 0 && len(text) > limits.ItemMaxBytes) ||
				!utf8.ValidString(text) || len(value.SourceMessageIDs) == 0 {
				return errors.New("narrative item is invalid")
			}
			ids := append([]string(nil), value.SourceMessageIDs...)
			sort.Strings(ids)
			ids = deduplicateStrings(ids)
			var sourceDigests []string
			for _, id := range ids {
				source, ok := sources[id]
				if !ok {
					return fmt.Errorf("narrative source %q is unknown", id)
				}
				sourceDigests = append(sourceDigests, source.Digest)
			}
			sourceDigest := digestString(strings.Join(sourceDigests, "\x00"))
			item := NarrativeItem{
				Kind: kind, Text: text, SourceMessageIDs: ids,
				SourceDigest: sourceDigest, CreatedTurn: createdTurn,
			}
			item.ID = stableNarrativeItemID(item)
			items = append(items, item)
			counts[kind]++
			if limits.MaxItems > 0 && len(items) > limits.MaxItems {
				return errors.New("narrative item count exceeds limit")
			}
		}
		return nil
	}
	for _, values := range []struct {
		kind  string
		items []narrativeJSONItem
	}{
		{NarrativeFileCode, *payload.FilesAndCode},
		{NarrativeCurrent, *payload.CurrentWork},
		{NarrativeNextStep, *payload.NextSteps},
		{NarrativePendingJob, *payload.PendingJobs},
		{NarrativeCritical, *payload.CriticalContext},
		{NarrativeTechnical, *payload.TechnicalConcepts},
		{NarrativeErrorFix, *payload.ErrorsAndFixes},
		{NarrativeDecision, *payload.Decisions},
		{NarrativeRationale, *payload.Rationale},
		{NarrativePreference, *payload.Preferences},
		{NarrativeUnresolved, *payload.Unresolved},
	} {
		if err := appendItems(values.kind, values.items); err != nil {
			return NarrativeArtifact{}, err
		}
	}
	for _, kind := range input.RequiredKinds {
		if counts[kind] == 0 {
			return NarrativeArtifact{}, fmt.Errorf(
				"narrative output is missing required %q context",
				kind,
			)
		}
	}
	coverage, err := narrativeCoverage(input, items)
	if err != nil {
		return NarrativeArtifact{}, err
	}
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if _, duplicate := seen[item.ID]; duplicate {
			return NarrativeArtifact{}, errors.New("narrative item is duplicated")
		}
		seen[item.ID] = struct{}{}
	}
	artifact := NarrativeArtifact{
		Version: NarrativeSchemaVersion, ThreadID: input.ThreadID,
		WindowID:        input.SourceWindowID,
		AuthorityDigest: input.AuthorityDigest,
		InputDigest:     input.Digest, RouteDigest: input.RouteDigest,
		Body: Narrative{Items: items}, Coverage: coverage, CreatedAt: now.UTC(),
		ExpiresAt: input.ExpiresAt,
	}
	artifact.Digest = artifact.digest()
	return artifact, artifact.Validate(now)
}

type narrativeJSONItem struct {
	Text             string   `json:"text"`
	SourceMessageIDs []string `json:"source_message_ids"`
}

func (a NarrativeArtifact) Validate(now time.Time) error {
	if a.Version != NarrativeSchemaVersion || a.ThreadID == "" ||
		a.WindowID == "" || a.AuthorityDigest == "" ||
		a.InputDigest == "" || a.RouteDigest == "" ||
		a.CreatedAt.IsZero() || a.ExpiresAt.IsZero() ||
		a.ExpiresAt.Before(a.CreatedAt) ||
		a.Digest == "" || a.Digest != a.digest() {
		return errors.New("narrative artifact identity or digest is invalid")
	}
	if !now.IsZero() && !a.ExpiresAt.After(now) {
		return errors.New("narrative artifact is stale")
	}
	for _, item := range a.Body.Items {
		if !validNarrativeKind(item.Kind) || item.ID == "" ||
			item.ID != stableNarrativeItemID(item) ||
			item.Text == "" || len(item.SourceMessageIDs) == 0 ||
			item.SourceDigest == "" ||
			!sort.StringsAreSorted(item.SourceMessageIDs) {
			return errors.New("narrative artifact item is invalid")
		}
		for index := 1; index < len(item.SourceMessageIDs); index++ {
			if item.SourceMessageIDs[index] ==
				item.SourceMessageIDs[index-1] {
				return errors.New("narrative artifact source is duplicated")
			}
		}
	}
	return validateNarrativeArtifactCoverage(a)
}

func (a NarrativeArtifact) digest() string {
	a.Digest = ""
	encoded, _ := json.Marshal(a)
	return digestString(string(encoded))
}

func (c *CompactedContext) Seal() error {
	if c == nil {
		return errors.New("compacted context is nil")
	}
	c.Version = NarrativeSchemaVersion
	authority, err := c.Truth.AuthorityDigest()
	if err != nil {
		return err
	}
	c.AuthorityDigest = authority
	c.Head = CloneMessage(c.Head)
	if c.Narrative != nil {
		if err := c.Narrative.Validate(time.Time{}); err != nil {
			return err
		}
		if c.Narrative.AuthorityDigest != authority ||
			c.Narrative.WindowID != c.SourceWindowID {
			return errors.New("narrative artifact fence does not match compacted context")
		}
		c.NarrativeDigest = c.Narrative.Digest
	} else {
		c.NarrativeDigest = ""
	}
	c.Tail = CloneMessages(c.Tail)
	if len(c.TailTurns) == 0 {
		c.TailTurns = make([]uint64, len(c.Tail))
		for index, message := range c.Tail {
			c.TailTurns[index] = message.Turn
		}
	} else {
		c.TailTurns = append([]uint64(nil), c.TailTurns...)
		if len(c.TailTurns) != len(c.Tail) {
			return errors.New("compacted context tail turn count is invalid")
		}
		for index, turn := range c.TailTurns {
			c.Tail[index].Turn = turn
		}
	}
	encodedTail, _ := json.Marshal(c.Tail)
	c.TailDigest = digestString(string(encodedTail))
	c.Digest = c.digest()
	return c.Validate()
}

func (c CompactedContext) Validate() error {
	if c.Version != NarrativeSchemaVersion || c.CompactionID == "" ||
		c.ThreadID == "" || c.TurnID == "" || c.SourceWindowID == "" ||
		c.TargetWindowID == "" || c.SourceContextDigest == "" ||
		c.StablePrefixDigest == "" || c.AuthorityDigest == "" ||
		c.TailDigest == "" || c.Digest == "" || c.Digest != c.digest() {
		return errors.New("compacted context identity or digest is invalid")
	}
	if _, found, err := ParseTruthCapsule(c.Head.Text()); err != nil {
		return err
	} else if !found {
		return errors.New("compacted context head has no truth capsule")
	}
	if len(c.TailTurns) != len(c.Tail) {
		return errors.New("compacted context tail turn count is invalid")
	}
	if err := c.Truth.Validate(); err != nil {
		return err
	}
	authorityDigest, err := c.Truth.AuthorityDigest()
	if err != nil || c.AuthorityDigest != authorityDigest {
		return errors.New("compacted context authority digest is invalid")
	}
	if c.Narrative == nil {
		if c.NarrativeDigest != "" {
			return errors.New("compacted context narrative digest is unexpected")
		}
	} else {
		if err := c.Narrative.Validate(time.Time{}); err != nil {
			return err
		}
		if c.NarrativeDigest != c.Narrative.Digest ||
			c.Narrative.AuthorityDigest != c.AuthorityDigest ||
			c.Narrative.WindowID != c.SourceWindowID {
			return errors.New("compacted context narrative fence is invalid")
		}
	}
	encodedTail, err := json.Marshal(c.Tail)
	if err != nil || c.TailDigest != digestString(string(encodedTail)) {
		return errors.New("compacted context tail digest is invalid")
	}
	if !toolPairsClosedForContext(c.Tail) {
		return errors.New("compacted context tail has an open tool pair")
	}
	return nil
}

func HistoryDigest(messages []provider.Message) string {
	encoded, _ := json.Marshal(CloneMessages(messages))
	return digestString(string(encoded))
}

func (c CompactedContext) digest() string {
	c.Digest = ""
	encoded, _ := json.Marshal(c)
	return digestString(string(encoded))
}

func StableMessageID(
	threadID protocol.ThreadID,
	message provider.Message,
	index int,
) string {
	turn := message.Turn
	message.Turn = 0
	encoded, _ := json.Marshal(message)
	sum := sha256.Sum256([]byte(
		string(threadID) + "\x00" +
			fmt.Sprint(turn) + "\x00" +
			fmt.Sprint(index) + "\x00" +
			string(message.Role) + "\x00" +
			digestString(string(encoded)),
	))
	return "msg_" + hex.EncodeToString(sum[:16])
}

func stableNarrativeItemID(item NarrativeItem) string {
	ids := append([]string(nil), item.SourceMessageIDs...)
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(
		item.Kind + "\x00" +
			strings.Join(strings.Fields(item.Text), " ") + "\x00" +
			strings.Join(ids, "\x00"),
	))
	return "narr_" + hex.EncodeToString(sum[:16])
}

func validNarrativeKind(kind string) bool {
	return kind == NarrativeDecision || kind == NarrativeRationale ||
		kind == NarrativePreference || kind == NarrativeUnresolved ||
		kind == NarrativeTechnical || kind == NarrativeFileCode ||
		kind == NarrativeErrorFix || kind == NarrativePendingJob ||
		kind == NarrativeCurrent || kind == NarrativeNextStep ||
		kind == NarrativeCritical
}

func narrativeMessageText(
	message provider.Message,
	toolCalls map[string]provider.ToolCall,
) string {
	var parts []string
	for _, block := range message.Blocks {
		switch {
		case block.Type == provider.ContentText && strings.TrimSpace(block.Text) != "":
			parts = append(parts, strings.TrimSpace(block.Text))
		case block.Type == provider.ContentReasoning && strings.TrimSpace(block.Text) != "":
			parts = append(parts, "Reasoning: "+strings.TrimSpace(block.Text))
		case block.ToolResult != nil:
			label := "Tool result"
			if call, ok := toolCalls[block.ToolResult.CallID]; ok {
				label = "Tool " + call.Name + " arguments: " + call.Arguments + "\nResult"
			}
			parts = append(parts, label+":\n"+block.ToolResult.Content)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func utf8Prefix(value string, maximum int) string {
	if maximum <= 0 || len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func deduplicateStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

func toolPairsClosedForContext(messages []provider.Message) bool {
	calls := make(map[string]int)
	results := make(map[string]int)
	for _, message := range messages {
		for _, block := range message.Blocks {
			if block.ToolCall != nil {
				calls[block.ToolCall.ID]++
			}
			if block.ToolResult != nil {
				results[block.ToolResult.CallID]++
			}
		}
	}
	if len(calls) != len(results) {
		return false
	}
	for id, count := range calls {
		if count != 1 || results[id] != 1 {
			return false
		}
	}
	return true
}
