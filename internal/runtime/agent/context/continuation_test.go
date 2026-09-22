package agentcontext

import (
	"context"
	"errors"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

type continuationBlobStore struct {
	data map[string][]byte
}

func newContinuationBlobStore() *continuationBlobStore {
	return &continuationBlobStore{data: map[string][]byte{}}
}

func (s *continuationBlobStore) Put(
	_ context.Context,
	handle string,
	data []byte,
) error {
	s.data[handle] = append([]byte(nil), data...)
	return nil
}

func (s *continuationBlobStore) Get(
	_ context.Context,
	handle string,
) ([]byte, error) {
	data, ok := s.data[handle]
	if !ok {
		return nil, errors.New("blob not found")
	}
	return append([]byte(nil), data...), nil
}

func continuationFixture() TurnContinuation {
	return TurnContinuation{
		Version:           ContinuationVersion,
		TurnID:            "turn-continue-1",
		Sequence:          2,
		TurnNumber:        3,
		SessionRevision:   7,
		StateEpoch:        2,
		SampleID:          "sample-2",
		Step:              2,
		WorkspaceIdentity: "workspace-a",
		ProfileRevision:   4,
		Provider:          "provider-a",
		Model:             "model-a",
		Messages: []provider.Message{
			provider.TextMessage(provider.RoleUser, "inspect the parser"),
			{
				Role: provider.RoleAssistant,
				Blocks: []provider.ContentBlock{{
					Type: provider.ContentToolCall,
					ToolCall: &provider.ToolCall{
						ID: "call-1", Name: "file_read", Arguments: `{}`,
					},
				}},
			},
			{
				Role: provider.RoleTool,
				Blocks: []provider.ContentBlock{{
					Type: provider.ContentToolResult,
					ToolResult: &provider.ToolResult{
						CallID: "call-1", Content: "package parser",
					},
				}},
			},
		},
	}
}

func TestTurnContinuationRoundtrip(t *testing.T) {
	store := newContinuationBlobStore()
	record := continuationFixture()
	ref, err := StoreTurnContinuation(t.Context(), store, record)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Handle == "" || ref.Digest == "" || ref.Handle != ref.Digest {
		t.Fatalf("continuation ref = %+v", ref)
	}
	loaded, err := LoadTurnContinuation(
		t.Context(), store, ref.Handle, ref.Digest,
	)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TurnID != record.TurnID ||
		loaded.Sequence != record.Sequence ||
		loaded.TurnNumber != record.TurnNumber ||
		len(loaded.Messages) != 3 ||
		loaded.Messages[2].Blocks[0].ToolResult.Content != "package parser" {
		t.Fatalf("loaded continuation = %+v", loaded)
	}
}

func TestContinuationReusesMessagesAndValidatesEveryChild(t *testing.T) {
	store := newContinuationBlobStore()
	record := continuationFixture()
	first, err := StoreTurnContinuation(t.Context(), store, record)
	if err != nil {
		t.Fatal(err)
	}
	originalCount := len(store.data)
	record.Sequence++
	record.Messages = append(record.Messages, provider.TextMessage(provider.RoleAssistant, "new answer"))
	second, err := StoreTurnContinuation(t.Context(), store, record)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.data) != originalCount+2 {
		t.Fatalf("new continuation stored %d new objects, want one message and one manifest", len(store.data)-originalCount)
	}
	var firstStored storedContinuation
	if err := readValue(t.Context(), store, first, &firstStored); err != nil {
		t.Fatal(err)
	}
	sharedID := firstStored.MessageRefs[0].Handle
	store.data[sharedID] = []byte("corrupt")
	if _, err := LoadTurnContinuation(t.Context(), store, second.Handle, second.Digest); !errors.Is(err, ErrTurnContinuationUnavailable) {
		t.Fatalf("corrupt shared message accepted: %v", err)
	}
}

func TestTurnContinuationRejectsMissingAndCorruptContent(t *testing.T) {
	store := newContinuationBlobStore()
	record := continuationFixture()
	ref, err := StoreTurnContinuation(t.Context(), store, record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTurnContinuation(
		t.Context(), store, ref.Handle, "not-the-digest",
	); !errors.Is(err, ErrTurnContinuationUnavailable) {
		t.Fatalf("digest mismatch error = %v", err)
	}
	if _, err := LoadTurnContinuation(
		t.Context(), store, "missing-handle", ref.Digest,
	); !errors.Is(err, ErrTurnContinuationUnavailable) {
		t.Fatalf("missing content error = %v", err)
	}
}

func TestTurnContinuationValidationAndDrift(t *testing.T) {
	record := continuationFixture()
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}
	broken := record
	broken.WorkspaceIdentity = ""
	if err := broken.Validate(); err == nil {
		t.Fatal("incomplete environment passed validation")
	}
	broken = record
	broken.Sequence = 0
	if err := broken.Validate(); err == nil {
		t.Fatal("missing sequence passed validation")
	}

	if drift := record.EnvironmentDrift(ContinuationEnvironment{
		WorkspaceIdentity: "workspace-a",
		ProfileRevision:   4,
		Provider:          "provider-a",
		Model:             "model-a",
	}); len(drift) != 0 {
		t.Fatalf("matching environment reported drift: %v", drift)
	}
	drift := record.EnvironmentDrift(ContinuationEnvironment{
		WorkspaceIdentity: "workspace-b",
		ProfileRevision:   5,
		Provider:          "provider-b",
		Model:             "model-b",
	})
	if len(drift) != 4 {
		t.Fatalf("drift reasons = %v", drift)
	}
}
