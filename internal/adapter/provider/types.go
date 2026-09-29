package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type ContentType string

const (
	ContentText       ContentType = "text"
	ContentReasoning  ContentType = "reasoning"
	ContentToolCall   ContentType = "tool_call"
	ContentToolResult ContentType = "tool_result"
	ContentSearch     ContentType = "search"
	ContentCitation   ContentType = "citation"
	ContentImage      ContentType = "image"
)

type Attachment struct {
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
	Name      string `json:"name,omitempty"`
	Handle    string `json:"handle,omitempty"`
}

func (a Attachment) DataURL() string {
	return "data:" + a.MediaType + ";base64," + base64.StdEncoding.EncodeToString(a.Data)
}

func (a Attachment) Base64() string {
	return base64.StdEncoding.EncodeToString(a.Data)
}

type ContentBlock struct {
	Type       ContentType   `json:"type"`
	Text       string        `json:"text,omitempty"`
	ID         string        `json:"id,omitempty"`
	ToolCall   *ToolCall     `json:"tool_call,omitempty"`
	ToolResult *ToolResult   `json:"tool_result,omitempty"`
	Search     *SearchResult `json:"search,omitempty"`
	Citation   *Citation     `json:"citation,omitempty"`
	Attachment *Attachment   `json:"attachment,omitempty"`
}
type ToolResult struct {
	CallID    string            `json:"call_id"`
	Content   string            `json:"content"`
	IsError   bool              `json:"is_error,omitempty"`
	Admission *AdmissionReceipt `json:"admission,omitempty"`
}
type Message struct {
	Role       Role                 `json:"role"`
	Blocks     []ContentBlock       `json:"content"`
	Provenance *AssistantProvenance `json:"provenance,omitempty"`
	Turn       uint64               `json:"-"`
}

func TextMessage(role Role, text string) Message {
	return Message{Role: role, Blocks: []ContentBlock{{Type: ContentText, Text: text}}}
}
func (m Message) Text() string {
	var result string
	for _, block := range m.Blocks {
		if block.Type == ContentText {
			result += block.Text
		}
	}
	return result
}

type ToolCall struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Arguments         string `json:"arguments"`
	CatalogID         string `json:"-"`
	CatalogGeneration uint64 `json:"-"`
	CatalogRevision   uint64 `json:"-"`
	CatalogAuthority  uint64 `json:"-"`
}
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}
type ModelRequest struct {
	Route            model.ReadyRoute  `json:"-"`
	Projection       ProjectionContext `json:"-"`
	LogicalRequestID string            `json:"-"`
	TransportAttempt uint32            `json:"-"`
	Purpose          model.Purpose     `json:"purpose,omitempty"`
	Messages         []Message         `json:"messages"`
	MaxOutputTokens  uint64            `json:"max_output_tokens"`
	Temperature      *float64          `json:"temperature,omitempty"`
	ReasoningEffort  string            `json:"reasoning_effort,omitempty"`
	NativeSearch     bool              `json:"native_search,omitempty"`
	Tools            []ToolDefinition  `json:"tools,omitempty"`
	Idempotent       bool              `json:"idempotent,omitempty"`
	PromptCacheKey   string            `json:"prompt_cache_key,omitempty"`
	Store            *bool             `json:"store,omitempty"`
	ParallelTools    *bool             `json:"parallel_tools,omitempty"`
	Include          []string          `json:"include,omitempty"`
}

func StickyPromptCacheKey(key string, route model.ReadyRoute) string {
	if key == "" || !route.Model().Capabilities.PromptCache {
		return ""
	}
	return key
}
func (r ModelRequest) Validate() error {
	if err := r.Route.Validate(); err != nil {
		return fmt.Errorf("route: %w", err)
	}
	if len(r.Messages) == 0 {
		return errors.New("messages are required")
	}
	for index, message := range r.Messages {
		switch message.Role {
		case RoleSystem, RoleUser, RoleAssistant, RoleTool:
		default:
			return fmt.Errorf("messages[%d] has invalid role %q", index, message.Role)
		}
		if len(message.Blocks) == 0 {
			return fmt.Errorf("messages[%d] has no content", index)
		}
		if err := validateMessageProvenance(message); err != nil {
			return fmt.Errorf("messages[%d]: %w", index, err)
		}
		if err := ValidateReplayForRoute(
			message, r.Route, r.Route.Adapter(),
		); err != nil {
			return fmt.Errorf("messages[%d]: %w", index, err)
		}
		for blockIndex, block := range message.Blocks {
			if err := block.Validate(); err != nil {
				return fmt.Errorf("messages[%d].content[%d]: %w", index, blockIndex, err)
			}
		}
	}
	if r.MaxOutputTokens == 0 {
		return errors.New("max_output_tokens must be positive")
	}
	descriptor := r.Route.Model()
	if r.MaxOutputTokens > descriptor.Limits.MaxOutputTokens {
		return fmt.Errorf("max_output_tokens exceeds model limit %d", descriptor.Limits.MaxOutputTokens)
	}
	caps := descriptor.Capabilities
	if r.ReasoningEffort != "" && !caps.SupportsReasoningEffort(r.ReasoningEffort) {
		return fmt.Errorf("model does not support reasoning effort %q", r.ReasoningEffort)
	}
	if r.NativeSearch && !caps.NativeSearch {
		return errors.New("model does not support provider-native search")
	}
	if len(r.Tools) > 0 && !caps.ToolCalls {
		return errors.New("model does not support tool calls")
	}
	if r.Purpose == model.PurposeVision {
		if err := model.RequireCapabilities(descriptor.ID, caps, []model.Capability{model.CapVision}); err != nil {
			return err
		}
	}
	if hasImageBlock(r.Messages) {
		if !caps.ImageInput && !caps.Vision {
			return fmt.Errorf("model %q does not support image input", descriptor.ID)
		}
	}
	if r.PromptCacheKey != "" && !caps.PromptCache {
		return fmt.Errorf("model %q does not support prompt cache", descriptor.ID)
	}
	for index, definition := range r.Tools {
		if definition.Name == "" || definition.Description == "" || definition.InputSchema == nil {
			return fmt.Errorf("tools[%d] is incomplete", index)
		}
	}
	return nil
}
func hasImageBlock(messages []Message) bool {
	for _, message := range messages {
		for _, block := range message.Blocks {
			if block.Type == ContentImage {
				return true
			}
		}
	}
	return false
}
func (b ContentBlock) Validate() error {
	switch b.Type {
	case ContentText:
		if b.Text == "" {
			return errors.New("text is required")
		}
	case ContentReasoning:
		if b.Text == "" {
			return errors.New("reasoning text is required")
		}
	case ContentToolCall:
		if b.ToolCall == nil || b.ToolCall.ID == "" || b.ToolCall.Name == "" {
			return errors.New("complete tool call is required")
		}
	case ContentToolResult:
		if b.ToolResult == nil || b.ToolResult.CallID == "" {
			return errors.New("tool result call_id is required")
		}
	case ContentSearch:
		if b.Search == nil {
			return errors.New("search result is required")
		}
	case ContentCitation:
		if b.Citation == nil || b.Citation.URL == "" {
			return errors.New("citation URL is required")
		}
	case ContentImage:
		if b.Attachment == nil || len(b.Attachment.Data) == 0 {
			return errors.New("image bytes are required")
		}
		if !strings.HasPrefix(b.Attachment.MediaType, "image/") {
			return fmt.Errorf("image media type %q is not an image", b.Attachment.MediaType)
		}
	default:
		return fmt.Errorf("unknown content block type %q", b.Type)
	}
	return nil
}

type StreamEventType string

const (
	EventMessageStart       StreamEventType = "message_start"
	EventTextDelta          StreamEventType = "text_delta"
	EventReasoningDelta     StreamEventType = "reasoning_delta"
	EventReasoningSignature StreamEventType = "reasoning_signature"
	EventToolCallDelta      StreamEventType = "tool_call_delta"
	EventSearchResult       StreamEventType = "search_result"
	EventCitation           StreamEventType = "citation"
	EventUsage              StreamEventType = "usage"
	EventReplayState        StreamEventType = "replay_state"
	EventResponseState      StreamEventType = "response_state"
	EventTransportProgress  StreamEventType = "transport_progress"
	EventMessageStop        StreamEventType = "message_stop"
)

type StopReason string

const (
	StopReasonEndTurn       StopReason = "end_turn"
	StopReasonToolUse       StopReason = "tool_use"
	StopReasonMaxTokens     StopReason = "max_tokens"
	StopReasonContentFilter StopReason = "content_filter"
	StopReasonIncomplete    StopReason = "incomplete"
	StopReasonUnknown       StopReason = "unknown"
)

func (r StopReason) Incomplete() bool {
	switch r {
	case StopReasonMaxTokens, StopReasonContentFilter, StopReasonIncomplete:
		return true
	default:
		return false
	}
}

// Usage counts one provider call. Cached and reasoning are subsets:
//
//	CachedTokens    ⊆ InputTokens
//	ReasoningTokens ⊆ OutputTokens
type Usage struct {
	InputTokens     uint64            `json:"input_tokens"`
	OutputTokens    uint64            `json:"output_tokens"`
	ReasoningTokens uint64            `json:"reasoning_tokens,omitempty"`
	CachedTokens    uint64            `json:"cached_tokens,omitempty"`
	Transport       TransportMetadata `json:"-"`
}

func (u Usage) Total() uint64 {
	return u.InputTokens + u.OutputTokens
}

// Consistent reports whether subset counters stay inside their totals.
func (u Usage) Consistent() bool {
	return u.CachedTokens <= u.InputTokens && u.ReasoningTokens <= u.OutputTokens
}

func (u *Usage) Add(other Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.ReasoningTokens += other.ReasoningTokens
	u.CachedTokens += other.CachedTokens
}

// MergeCumulative keeps the latest per-transport snapshot without shrinking a
// counter. Adapters emit cumulative snapshots; this merge makes category-specific
// or repeated reports idempotent. Distinct transports still use Add.
func MergeCumulative(current, update Usage) Usage {
	merged := update
	if current.InputTokens > merged.InputTokens {
		merged.InputTokens = current.InputTokens
	}
	if current.OutputTokens > merged.OutputTokens {
		merged.OutputTokens = current.OutputTokens
	}
	if current.ReasoningTokens > merged.ReasoningTokens {
		merged.ReasoningTokens = current.ReasoningTokens
	}
	if current.CachedTokens > merged.CachedTokens {
		merged.CachedTokens = current.CachedTokens
	}
	if merged.Transport == (TransportMetadata{}) {
		merged.Transport = current.Transport
	}
	return merged
}

// SameSnapshot reports whether two usage values name the same counters.
func SameSnapshot(left, right Usage) bool {
	return left.InputTokens == right.InputTokens &&
		left.OutputTokens == right.OutputTokens &&
		left.ReasoningTokens == right.ReasoningTokens &&
		left.CachedTokens == right.CachedTokens
}

// DoubledSnapshot is the Usage.Add-on-cumulative bug: every reported counter
// becomes twice the previous snapshot of the same transport.
func DoubledSnapshot(previous, next Usage) bool {
	if previous.InputTokens == 0 || previous.OutputTokens == 0 {
		return false
	}
	if next.InputTokens != previous.InputTokens*2 ||
		next.OutputTokens != previous.OutputTokens*2 {
		return false
	}
	if previous.ReasoningTokens != 0 &&
		next.ReasoningTokens != previous.ReasoningTokens*2 {
		return false
	}
	if previous.CachedTokens != 0 &&
		next.CachedTokens != previous.CachedTokens*2 {
		return false
	}
	return true
}

type ToolCallFragment struct {
	Index     int    `json:"index"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}
type SearchResult struct {
	Query   string   `json:"query,omitempty"`
	Sources []Source `json:"sources"`
	Error   string   `json:"error,omitempty"`
}
type Source struct {
	ID    string `json:"id,omitempty"`
	Title string `json:"title,omitempty"`
	URL   string `json:"url"`
}
type Citation struct {
	SourceID string `json:"source_id,omitempty"`
	URL      string `json:"url"`
	Title    string `json:"title,omitempty"`
	Start    int    `json:"start,omitempty"`
	End      int    `json:"end,omitempty"`
}
type StreamEvent struct {
	Type       StreamEventType   `json:"type"`
	EventID    string            `json:"event_id,omitempty"`
	Sequenced  bool              `json:"sequenced,omitempty"`
	Sequence   uint64            `json:"sequence,omitempty"`
	Ordinal    uint32            `json:"ordinal,omitempty"`
	StopReason StopReason        `json:"stop_reason,omitempty"`
	Index      int               `json:"index,omitempty"`
	Block      *ContentBlock     `json:"block,omitempty"`
	Text       string            `json:"text,omitempty"`
	Signature  string            `json:"signature,omitempty"`
	ToolCall   *ToolCallFragment `json:"tool_call,omitempty"`
	Search     *SearchResult     `json:"search,omitempty"`
	Citation   *Citation         `json:"citation,omitempty"`
	// Usage is the cumulative snapshot for this transport attempt. Adapters
	// must normalize Provider-specific delta reports before exposing them.
	Usage          *Usage          `json:"usage,omitempty"`
	Replay         *ReplayState    `json:"replay,omitempty"`
	ReplayFragment json.RawMessage `json:"-"`
	Response       *ResponseState  `json:"response,omitempty"`
}

type ResponseState struct {
	ID     string            `json:"id"`
	Output []json.RawMessage `json:"output,omitempty"`
}

func (e StreamEvent) Validate() error {
	if e.Block != nil {
		if err := e.Block.Validate(); err != nil {
			return fmt.Errorf("stream block: %w", err)
		}
	}
	switch e.Type {
	case EventMessageStart:
		if e.StopReason != "" {
			return errors.New("message start cannot have a stop reason")
		}
		return nil
	case EventMessageStop:
		switch e.StopReason {
		case "", StopReasonEndTurn, StopReasonToolUse, StopReasonMaxTokens,
			StopReasonContentFilter, StopReasonIncomplete, StopReasonUnknown:
		default:
			return fmt.Errorf("unknown stop reason %q", e.StopReason)
		}
		return nil
	case EventTextDelta, EventReasoningDelta:
		if e.Text == "" && e.Block == nil {
			return errors.New("stream text delta is empty")
		}
	case EventReasoningSignature:
		if e.Signature == "" {
			return errors.New("reasoning signature is empty")
		}
	case EventToolCallDelta:
		if e.ToolCall == nil {
			return errors.New("tool call fragment is required")
		}
	case EventUsage:
		if e.Usage == nil {
			return errors.New("usage is required")
		}
	case EventReplayState:
		if e.Replay == nil || e.Replay.Version != ReplayVersion ||
			len(e.Replay.Data) == 0 || !json.Valid(e.Replay.Data) {
			return errors.New("versioned replay state is required")
		}
	case EventResponseState:
		if e.Response == nil || e.Response.ID == "" {
			return errors.New("response state id is required")
		}
	case EventTransportProgress:
		return nil
	case EventSearchResult:
		if e.Search == nil {
			return errors.New("search result is required")
		}
		for index, source := range e.Search.Sources {
			if source.URL == "" {
				return fmt.Errorf("search source %d URL is required", index)
			}
		}
	case EventCitation:
		if e.Citation == nil || e.Citation.URL == "" {
			return errors.New("citation URL is required")
		}
	default:
		return fmt.Errorf("unknown stream event type %q", e.Type)
	}
	return nil
}

type Stream interface {
	Recv() (StreamEvent, error)
	Close() error // Concurrent-safe and unblocks Recv.
}
type TransportMetadata struct {
	LogicalRequestID       string            `json:"logical_request_id,omitempty"`
	TransportRequestID     string            `json:"transport_request_id,omitempty"`
	Attempt                uint32            `json:"attempt,omitempty"`
	RequestBytes           uint64            `json:"request_bytes"`
	RouteCooldownWaitMS    uint64            `json:"route_cooldown_wait_ms,omitempty"`
	LogicalRequestDigest   string            `json:"logical_request_digest,omitempty"`
	TransportPayloadDigest string            `json:"transport_payload_digest,omitempty"`
	Incremental            bool              `json:"incremental,omitempty"`
	Projection             ProjectionReceipt `json:"projection"`
}
type MetadataStream interface {
	Stream
	TransportMetadata() TransportMetadata
}

func Metadata(stream Stream) TransportMetadata {
	if value, ok := stream.(MetadataStream); ok {
		return value.TransportMetadata()
	}
	return TransportMetadata{}
}

type Provider interface {
	Stream(context.Context, ModelRequest) (Stream, error)
}
