package assembly

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

const ResponseAssemblyVersion = 1

type TransportMetadata = provider.TransportMetadata
type StopReason = provider.StopReason
type ContentBlock = provider.ContentBlock
type ToolCallFragment = provider.ToolCallFragment
type Usage = provider.Usage
type ReplayState = provider.ReplayState
type ResponseState = provider.ResponseState
type StreamEvent = provider.StreamEvent
type ToolCall = provider.ToolCall
type ContentType = provider.ContentType

const (
	EventMessageStart       = provider.EventMessageStart
	EventTransportProgress  = provider.EventTransportProgress
	EventTextDelta          = provider.EventTextDelta
	EventReasoningDelta     = provider.EventReasoningDelta
	EventReasoningSignature = provider.EventReasoningSignature
	EventSearchResult       = provider.EventSearchResult
	EventCitation           = provider.EventCitation
	EventToolCallDelta      = provider.EventToolCallDelta
	EventUsage              = provider.EventUsage
	EventReplayState        = provider.EventReplayState
	EventResponseState      = provider.EventResponseState
	EventMessageStop        = provider.EventMessageStop

	StopReasonEndTurn    = provider.StopReasonEndTurn
	StopReasonIncomplete = provider.StopReasonIncomplete
	StopReasonMaxTokens  = provider.StopReasonMaxTokens
	StopReasonToolUse    = provider.StopReasonToolUse

	ContentText      = provider.ContentText
	ContentReasoning = provider.ContentReasoning
	ContentSearch    = provider.ContentSearch
	ContentCitation  = provider.ContentCitation
)

type ResponseCompleteness string

const (
	ResponseEmpty      ResponseCompleteness = "empty"
	ResponseStreaming  ResponseCompleteness = "streaming"
	ResponseIncomplete ResponseCompleteness = "incomplete"
	ResponseComplete   ResponseCompleteness = "complete"
	ResponseFailed     ResponseCompleteness = "failed"
)

// ResponseAssembly is the persistable, provider-neutral record of one logical
// model sample. Each transport attempt gets its own segment so an interrupted
// tool fragment can never be confused with a later complete call.
type ResponseAssembly struct {
	Version          int                  `json:"version"`
	LogicalRequestID string               `json:"logical_request_id"`
	State            ResponseCompleteness `json:"state"`
	Segments         []ResponseSegment    `json:"segments,omitempty"`
	// One-based segment indexes whose rejected tool calls Runtime authorized
	// for regeneration. Persist before opening another transport.
	ToolArgumentRepairs []uint32 `json:"tool_argument_repairs,omitempty"`
}

type ResponseSegment struct {
	Transport             TransportMetadata    `json:"transport"`
	State                 ResponseCompleteness `json:"state"`
	StopReason            StopReason           `json:"stop_reason,omitempty"`
	Blocks                []ContentBlock       `json:"blocks,omitempty"`
	ToolFragments         []ToolCallFragment   `json:"tool_fragments,omitempty"`
	Usage                 Usage                `json:"usage"`
	Replay                *ReplayState         `json:"replay,omitempty"`
	Response              *ResponseState       `json:"response,omitempty"`
	Seen                  map[string]string    `json:"seen,omitempty"`
	HasSequence           bool                 `json:"has_sequence,omitempty"`
	LastSequence          uint64               `json:"last_sequence,omitempty"`
	LastOrdinal           uint32               `json:"last_ordinal,omitempty"`
	EventCount            uint64               `json:"event_count"`
	Meaningful            bool                 `json:"meaningful"`
	Error                 string               `json:"error,omitempty"`
	RejectedToolArguments bool                 `json:"rejected_tool_arguments,omitempty"`
}

func NewResponseAssembly(logicalRequestID string) *ResponseAssembly {
	return &ResponseAssembly{
		Version:          ResponseAssemblyVersion,
		LogicalRequestID: logicalRequestID,
		State:            ResponseEmpty,
	}
}

func CloneResponseAssembly(source *ResponseAssembly) *ResponseAssembly {
	if source == nil {
		return nil
	}
	data, err := json.Marshal(source)
	if err != nil {
		return nil
	}
	var cloned ResponseAssembly
	if json.Unmarshal(data, &cloned) != nil {
		return nil
	}
	return &cloned
}

func (a *ResponseAssembly) BeginTransport(metadata TransportMetadata) error {
	if a == nil {
		return errors.New("response assembly is nil")
	}
	if a.Version == 0 {
		a.Version = ResponseAssemblyVersion
	}
	if a.LogicalRequestID == "" {
		a.LogicalRequestID = metadata.LogicalRequestID
	}
	if a.LogicalRequestID == "" {
		return errors.New("response assembly logical request id is empty")
	}
	if metadata.LogicalRequestID == "" {
		metadata.LogicalRequestID = a.LogicalRequestID
	}
	if metadata.LogicalRequestID != a.LogicalRequestID {
		return errors.New("response transport changed logical request identity")
	}
	if a.State == ResponseFailed && !a.toolArgumentRepairAuthorized() {
		return fmt.Errorf("response assembly is already %s", a.State)
	}
	if len(a.Segments) != 0 {
		previous := &a.Segments[len(a.Segments)-1]
		if previous.State == ResponseStreaming {
			previous.State = ResponseIncomplete
			previous.Error = "transport replaced before a complete stop event"
		}
	}
	if metadata.Attempt == 0 {
		metadata.Attempt = uint32(len(a.Segments) + 1)
	}
	if metadata.TransportRequestID == "" {
		metadata.TransportRequestID = fmt.Sprintf(
			"%s/transport/%d",
			a.LogicalRequestID,
			metadata.Attempt,
		)
	}
	for _, segment := range a.Segments {
		if segment.Transport.TransportRequestID == metadata.TransportRequestID {
			return fmt.Errorf(
				"response transport id %q is duplicated",
				metadata.TransportRequestID,
			)
		}
	}
	a.Segments = append(a.Segments, ResponseSegment{
		Transport: metadata,
		State:     ResponseStreaming,
		Seen:      make(map[string]string),
	})
	a.State = ResponseStreaming
	return nil
}

// Apply records one normalized event. The boolean is false only for an exact
// duplicate carrying a stable provider event identity.
func (a *ResponseAssembly) Apply(event StreamEvent) (bool, error) {
	segment, err := a.current()
	if err != nil {
		return false, err
	}
	if segment.State != ResponseStreaming {
		return false, fmt.Errorf("response segment is already %s", segment.State)
	}
	if err := event.Validate(); err != nil {
		return false, err
	}
	key, digest, err := responseEventIdentity(event)
	if err != nil {
		return false, err
	}
	if key != "" {
		if previous, exists := segment.Seen[key]; exists {
			if previous != digest {
				return false, fmt.Errorf(
					"provider event identity %q changed payload",
					key,
				)
			}
			return false, nil
		}
		if event.Sequenced {
			switch {
			case segment.HasSequence && event.Sequence < segment.LastSequence:
				return false, fmt.Errorf(
					"provider event sequence moved backward from %d to %d",
					segment.LastSequence,
					event.Sequence,
				)
			case segment.HasSequence &&
				event.Sequence == segment.LastSequence &&
				event.Ordinal <= segment.LastOrdinal:
				return false, fmt.Errorf(
					"provider event ordinal moved backward at sequence %d",
					event.Sequence,
				)
			}
			segment.HasSequence = true
			segment.LastSequence = event.Sequence
			segment.LastOrdinal = event.Ordinal
		}
		segment.Seen[key] = digest
	}
	segment.EventCount++
	switch event.Type {
	case EventMessageStart:
	case EventTransportProgress:
	case EventTextDelta:
		segment.Meaningful = true
		segment.Blocks = appendResponseBlock(
			segment.Blocks,
			responseEventBlock(event, ContentText),
		)
	case EventReasoningDelta:
		segment.Meaningful = true
		segment.Blocks = appendResponseBlock(
			segment.Blocks,
			responseEventBlock(event, ContentReasoning),
		)
	case EventReasoningSignature:
		return false, errors.New(
			"provider signature was not captured as replay state",
		)
	case EventSearchResult:
		segment.Meaningful = true
		segment.Blocks = append(
			segment.Blocks,
			responseEventBlock(event, ContentSearch),
		)
	case EventCitation:
		segment.Meaningful = true
		segment.Blocks = append(
			segment.Blocks,
			responseEventBlock(event, ContentCitation),
		)
	case EventToolCallDelta:
		segment.Meaningful = true
		if err := appendToolFragment(segment, *event.ToolCall); err != nil {
			return false, err
		}
	case EventUsage:
		// Provider adapters expose cumulative snapshots within one transport.
		// Max-consistent merge keeps category-specific or repeated reports
		// idempotent while TotalUsage still sums distinct transports.
		segment.Usage = provider.MergeCumulative(segment.Usage, *event.Usage)
	case EventReplayState:
		if segment.Replay != nil {
			return false, errors.New("provider emitted duplicate replay state")
		}
		replay := *event.Replay
		replay.Data = append(json.RawMessage(nil), event.Replay.Data...)
		segment.Replay = &replay
	case EventResponseState:
		if segment.Response != nil && segment.Response.ID != event.Response.ID {
			return false, errors.New("provider changed response state identity")
		}
		response := *event.Response
		response.Output = cloneRawMessages(event.Response.Output)
		segment.Response = &response
	case EventMessageStop:
		segment.StopReason = event.StopReason
		if segment.StopReason == "" {
			segment.StopReason = StopReasonEndTurn
		}
		if segment.StopReason.Incomplete() {
			segment.State = ResponseIncomplete
			a.State = ResponseIncomplete
		} else {
			segment.State = ResponseComplete
			a.State = ResponseComplete
		}
	default:
		return false, fmt.Errorf("unknown provider event %q", event.Type)
	}
	return true, nil
}

func (a *ResponseAssembly) Interrupt(cause error) error {
	segment, err := a.current()
	if err != nil {
		return err
	}
	if segment.State == ResponseComplete || segment.State == ResponseFailed {
		return nil
	}
	segment.State = ResponseIncomplete
	segment.StopReason = StopReasonIncomplete
	if cause != nil {
		segment.Error = cause.Error()
	}
	a.State = ResponseIncomplete
	return nil
}

func (a *ResponseAssembly) Fail(cause error) error {
	segment, err := a.current()
	if err != nil {
		return err
	}
	segment.State = ResponseFailed
	if cause != nil {
		segment.Error = cause.Error()
	}
	a.State = ResponseFailed
	return nil
}

func (a *ResponseAssembly) CurrentBlocks() []ContentBlock {
	segment := a.currentOrNil()
	if segment == nil {
		return nil
	}
	return cloneContentBlocks(segment.Blocks)
}

func (a *ResponseAssembly) ConfirmedBlocks() []ContentBlock {
	if a == nil {
		return nil
	}
	var blocks []ContentBlock
	for _, segment := range a.Segments {
		for _, block := range cloneContentBlocks(segment.Blocks) {
			blocks = appendResponseBlock(blocks, block)
		}
	}
	return blocks
}

func (a *ResponseAssembly) CurrentUsage() Usage {
	segment := a.currentOrNil()
	if segment == nil {
		return Usage{}
	}
	return segment.Usage
}

func (a *ResponseAssembly) TotalUsage() Usage {
	var usage Usage
	if a == nil {
		return usage
	}
	for _, segment := range a.Segments {
		usage.Add(segment.Usage)
	}
	return usage
}

func (a *ResponseAssembly) CurrentMeaningful() bool {
	segment := a.currentOrNil()
	return segment != nil && segment.Meaningful
}

func (a *ResponseAssembly) CurrentReplay() *ReplayState {
	segment := a.currentOrNil()
	if segment == nil || segment.Replay == nil {
		return nil
	}
	replay := *segment.Replay
	replay.Data = append(json.RawMessage(nil), segment.Replay.Data...)
	return &replay
}

func (a *ResponseAssembly) CurrentStopReason() StopReason {
	segment := a.currentOrNil()
	if segment == nil {
		return ""
	}
	return segment.StopReason
}

func (a *ResponseAssembly) TransportCount() int {
	if a == nil {
		return 0
	}
	return len(a.Segments)
}

func (a *ResponseAssembly) NextTransportAttempt() uint32 {
	if a == nil {
		return 1
	}
	return uint32(len(a.Segments) + 1)
}

func (a *ResponseAssembly) EventCount() uint64 {
	var count uint64
	if a == nil {
		return 0
	}
	for _, segment := range a.Segments {
		count += segment.EventCount
	}
	return count
}

func (a *ResponseAssembly) IncompleteToolFragments() []ToolCallFragment {
	if a == nil {
		return nil
	}
	var fragments []ToolCallFragment
	for _, segment := range a.Segments {
		if segment.State == ResponseFailed {
			if segment.RejectedToolArguments {
				fragments = nil
			}
			continue
		}
		if segment.State != ResponseComplete ||
			!toolFragmentsComplete(segment.ToolFragments) {
			fragments = append(
				fragments,
				cloneToolFragments(segment.ToolFragments)...,
			)
		}
	}
	return fragments
}

func (a *ResponseAssembly) ExecutableToolCalls() ([]ToolCall, error) {
	segment := a.currentOrNil()
	if segment == nil || segment.State != ResponseComplete {
		return nil, errors.New("response is not complete")
	}
	fragments := cloneToolFragments(segment.ToolFragments)
	sort.Slice(fragments, func(i, j int) bool {
		return fragments[i].Index < fragments[j].Index
	})
	calls := make([]ToolCall, 0, len(fragments))
	for _, fragment := range fragments {
		arguments := fragment.Arguments
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		if fragment.ID == "" || fragment.Name == "" ||
			!json.Valid([]byte(arguments)) {
			return nil, fmt.Errorf(
				"tool call fragment %d is incomplete",
				fragment.Index,
			)
		}
		// Recheck persisted assemblies as well as live streams. encoding/json
		// otherwise silently accepts the last value of a duplicated member.
		var members jsonMemberTracker
		if err := members.Append(arguments); err != nil {
			return nil, fmt.Errorf("tool call fragment %d: %w", fragment.Index, err)
		}
		calls = append(calls, ToolCall{
			ID: fragment.ID, Name: fragment.Name,
			Arguments: arguments,
		})
	}
	return calls, nil
}

func toolFragmentsComplete(fragments []ToolCallFragment) bool {
	for _, fragment := range fragments {
		if strings.TrimSpace(fragment.Arguments) == "" &&
			fragment.ID != "" && fragment.Name != "" {
			continue
		}
		if fragment.ID == "" || fragment.Name == "" ||
			!json.Valid([]byte(fragment.Arguments)) {
			return false
		}
	}
	return true
}

func (a *ResponseAssembly) Validate() error {
	if a == nil {
		return errors.New("response assembly is nil")
	}
	if a.Version != ResponseAssemblyVersion {
		return fmt.Errorf("unsupported response assembly version %d", a.Version)
	}
	if strings.TrimSpace(a.LogicalRequestID) == "" {
		return errors.New("response assembly logical request id is empty")
	}
	switch a.State {
	case ResponseEmpty, ResponseStreaming, ResponseIncomplete,
		ResponseComplete, ResponseFailed:
	default:
		return fmt.Errorf("invalid response assembly state %q", a.State)
	}
	if len(a.Segments) == 0 {
		if a.State != ResponseEmpty || len(a.ToolArgumentRepairs) != 0 {
			return errors.New("response assembly without segments is not empty")
		}
		return nil
	}
	for index, segment := range a.Segments {
		if segment.Transport.LogicalRequestID != a.LogicalRequestID ||
			segment.Transport.TransportRequestID == "" ||
			segment.Transport.Attempt == 0 {
			return fmt.Errorf("response segment %d has invalid attribution", index)
		}
		switch segment.State {
		case ResponseStreaming, ResponseIncomplete, ResponseComplete,
			ResponseFailed:
		default:
			return fmt.Errorf(
				"response segment %d has invalid state %q",
				index,
				segment.State,
			)
		}
		if segment.RejectedToolArguments && (segment.State != ResponseFailed || len(segment.ToolFragments) == 0) {
			return fmt.Errorf("response segment %d has invalid tool rejection", index)
		}
	}
	if err := a.validateToolArgumentRepairs(); err != nil {
		return err
	}
	if a.State != a.Segments[len(a.Segments)-1].State {
		return errors.New("response assembly state does not match last segment")
	}
	return nil
}

func (a *ResponseAssembly) ValidateExtension(previous *ResponseAssembly) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if previous == nil {
		return nil
	}
	if err := previous.Validate(); err != nil {
		return fmt.Errorf("previous response assembly: %w", err)
	}
	if a.Version != previous.Version ||
		a.LogicalRequestID != previous.LogicalRequestID ||
		len(a.Segments) < len(previous.Segments) {
		return errors.New("response assembly does not extend durable identity")
	}
	if len(a.ToolArgumentRepairs) < len(previous.ToolArgumentRepairs) ||
		!slices.Equal(a.ToolArgumentRepairs[:len(previous.ToolArgumentRepairs)], previous.ToolArgumentRepairs) {
		return errors.New("tool argument repair authorizations changed")
	}
	for index, before := range previous.Segments {
		after := a.Segments[index]
		if err := validateSegmentExtension(before, after); err != nil {
			return fmt.Errorf(
				"response segment %d regressed durable progress: %w",
				index,
				err,
			)
		}
	}
	return nil
}

func validateSegmentExtension(before, after ResponseSegment) error {
	if before.RejectedToolArguments && !after.RejectedToolArguments ||
		before.State != ResponseStreaming && before.RejectedToolArguments != after.RejectedToolArguments {
		return errors.New("tool argument rejection changed")
	}
	if !reflect.DeepEqual(before.Transport, after.Transport) {
		return errors.New("transport metadata changed")
	}
	if before.EventCount > after.EventCount ||
		before.Usage.InputTokens > after.Usage.InputTokens ||
		before.Usage.OutputTokens > after.Usage.OutputTokens ||
		before.Usage.ReasoningTokens > after.Usage.ReasoningTokens ||
		before.Usage.CachedTokens > after.Usage.CachedTokens {
		return errors.New("counters moved backward")
	}
	if before.Meaningful && !after.Meaningful ||
		before.HasSequence && !after.HasSequence ||
		before.HasSequence &&
			(after.LastSequence < before.LastSequence ||
				after.LastSequence == before.LastSequence &&
					after.LastOrdinal < before.LastOrdinal) {
		return errors.New("stream progress moved backward")
	}
	if before.StopReason != "" && before.StopReason != after.StopReason ||
		before.Error != "" && before.Error != after.Error {
		return errors.New("terminal metadata changed")
	}
	if before.State != ResponseStreaming && before.State != after.State {
		return errors.New("settled response state changed")
	}
	if err := validateBlockPrefix(before.Blocks, after.Blocks); err != nil {
		return err
	}
	if err := validateToolFragmentPrefix(
		before.ToolFragments,
		after.ToolFragments,
	); err != nil {
		return err
	}
	if before.Replay != nil && !reflect.DeepEqual(before.Replay, after.Replay) {
		return errors.New("replay state changed")
	}
	if err := validateResponseStatePrefix(before.Response, after.Response); err != nil {
		return err
	}
	for key, digest := range before.Seen {
		if after.Seen[key] != digest {
			return fmt.Errorf("event %q changed", key)
		}
	}
	return nil
}

func validateBlockPrefix(before, after []ContentBlock) error {
	if len(before) > len(after) {
		return errors.New("response blocks were removed")
	}
	for index := range before {
		oldBlock, newBlock := before[index], after[index]
		oldText, newText := oldBlock.Text, newBlock.Text
		oldBlock.Text, newBlock.Text = "", ""
		if oldBlock.ID == "" {
			newBlock.ID = ""
		}
		if !reflect.DeepEqual(oldBlock, newBlock) ||
			!strings.HasPrefix(newText, oldText) {
			return fmt.Errorf("response block %d changed", index)
		}
	}
	return nil
}

func validateToolFragmentPrefix(
	before, after []ToolCallFragment,
) error {
	if len(before) > len(after) {
		return errors.New("tool fragments were removed")
	}
	for index := range before {
		oldFragment, newFragment := before[index], after[index]
		if oldFragment.Index != newFragment.Index ||
			oldFragment.ID != "" && oldFragment.ID != newFragment.ID ||
			oldFragment.Name != "" && oldFragment.Name != newFragment.Name ||
			!strings.HasPrefix(newFragment.Arguments, oldFragment.Arguments) {
			return fmt.Errorf("tool fragment %d changed", index)
		}
	}
	return nil
}

func validateResponseStatePrefix(
	before, after *ResponseState,
) error {
	if before == nil {
		return nil
	}
	if after == nil || before.ID != after.ID ||
		len(before.Output) > len(after.Output) {
		return errors.New("response state changed")
	}
	for index := range before.Output {
		if !bytes.Equal(before.Output[index], after.Output[index]) {
			return fmt.Errorf("response output %d changed", index)
		}
	}
	return nil
}

func (a *ResponseAssembly) current() (*ResponseSegment, error) {
	if a == nil || len(a.Segments) == 0 {
		return nil, errors.New("response transport has not started")
	}
	return &a.Segments[len(a.Segments)-1], nil
}

func (a *ResponseAssembly) currentOrNil() *ResponseSegment {
	if a == nil || len(a.Segments) == 0 {
		return nil
	}
	return &a.Segments[len(a.Segments)-1]
}

func responseEventIdentity(event StreamEvent) (string, string, error) {
	key := event.EventID
	if event.Sequenced {
		key = fmt.Sprintf("sequence:%d:%d", event.Sequence, event.Ordinal)
	}
	if key == "" {
		return "", "", nil
	}
	data, err := json.Marshal(event)
	if err != nil {
		return "", "", err
	}
	return key, digestBytes(data), nil
}

func appendToolFragment(
	segment *ResponseSegment,
	fragment ToolCallFragment,
) error {
	for index := range segment.ToolFragments {
		current := &segment.ToolFragments[index]
		if current.Index != fragment.Index {
			continue
		}
		if fragment.ID != "" {
			if current.ID != "" && current.ID != fragment.ID {
				return fmt.Errorf(
					"tool call %d changed id from %q to %q",
					fragment.Index,
					current.ID,
					fragment.ID,
				)
			}
			current.ID = fragment.ID
		}
		if fragment.Name != "" {
			if current.Name != "" && current.Name != fragment.Name {
				return fmt.Errorf(
					"tool call %d changed name from %q to %q",
					fragment.Index,
					current.Name,
					fragment.Name,
				)
			}
			current.Name = fragment.Name
		}
		current.Arguments += fragment.Arguments
		return nil
	}
	segment.ToolFragments = append(
		segment.ToolFragments,
		fragment,
	)
	return nil
}

func responseEventBlock(
	event StreamEvent,
	fallback ContentType,
) ContentBlock {
	if event.Block != nil {
		return cloneContentBlocks([]ContentBlock{*event.Block})[0]
	}
	switch event.Type {
	case EventTextDelta:
		return ContentBlock{Type: ContentText, Text: event.Text}
	case EventReasoningDelta:
		return ContentBlock{Type: ContentReasoning, Text: event.Text}
	case EventSearchResult:
		return ContentBlock{Type: ContentSearch, Search: event.Search}
	case EventCitation:
		return ContentBlock{Type: ContentCitation, Citation: event.Citation}
	default:
		return ContentBlock{Type: fallback, Text: event.Text}
	}
}

func appendResponseBlock(
	blocks []ContentBlock,
	block ContentBlock,
) []ContentBlock {
	if len(blocks) != 0 && block.Type == blocks[len(blocks)-1].Type {
		last := &blocks[len(blocks)-1]
		if block.Type == ContentText {
			last.Text += block.Text
			return blocks
		}
		if block.Type == ContentReasoning &&
			(last.ID == "" || block.ID == "" || last.ID == block.ID) {
			last.Text += block.Text
			if last.ID == "" {
				last.ID = block.ID
			}
			return blocks
		}
	}
	return append(blocks, block)
}

func cloneContentBlocks(source []ContentBlock) []ContentBlock {
	if len(source) == 0 {
		return nil
	}
	data, err := json.Marshal(source)
	if err != nil {
		return nil
	}
	var cloned []ContentBlock
	if json.Unmarshal(data, &cloned) != nil {
		return nil
	}
	return cloned
}

func cloneToolFragments(source []ToolCallFragment) []ToolCallFragment {
	return append([]ToolCallFragment(nil), source...)
}

func cloneRawMessages(source []json.RawMessage) []json.RawMessage {
	result := make([]json.RawMessage, len(source))
	for index := range source {
		result[index] = append(json.RawMessage(nil), source[index]...)
	}
	return result
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum[:])
}
