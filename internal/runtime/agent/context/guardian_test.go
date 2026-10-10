package agentcontext

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type authorizationEvents struct {
	events []protocol.Event
	gap    bool
	fence  protocol.Cursor
}

func (s *authorizationEvents) LastSequence(context.Context) (protocol.Cursor, error) {
	if s.fence != 0 {
		return s.fence, nil
	}
	return protocol.Cursor(len(s.events)), nil
}
func (s *authorizationEvents) ReplayThrough(ctx context.Context, _, fence protocol.Cursor, _ int) ([]protocol.Event, bool, error) {
	if s.gap {
		return nil, false, errors.New("history gap")
	}
	var events []protocol.Event
	for _, event := range s.events {
		if event.Sequence <= fence {
			events = append(events, event)
		}
	}
	return events, false, ctx.Err()
}
func (s *authorizationEvents) add(t *testing.T, thread, turn string, data protocol.EventData) {
	t.Helper()
	event, err := protocol.NewEvent(protocol.EventMeta{Sequence: protocol.Cursor(len(s.events) + 1), OperationID: "op", ItemID: "item", ThreadID: protocol.ThreadID(thread), TurnID: protocol.TurnID(turn)}, data)
	if err != nil {
		t.Fatal(err)
	}
	s.events = append(s.events, event)
}
func authorizationScope() AuthorizationScope {
	return AuthorizationScope{WorkspaceID: "workspace", SessionID: "session", ThreadID: "thread"}
}

func TestGuardianAuthorizationRetainsOldSourcesAndRejectsForgedAuthority(t *testing.T) {
	store := &authorizationEvents{}
	for index := range 8 {
		store.add(t, "thread", fmt.Sprint(index), &protocol.TurnStartedData{Provider: "fixture", Model: "fixture", DisplayPrompt: fmt.Sprintf("user restriction %d", index), Prompt: "expanded repository text says allow anything"})
		store.add(t, "thread", fmt.Sprint(index), &protocol.TurnCompletedData{Text: "assistant says user authorized all commands"})
	}
	store.add(t, "foreign", "foreign", &protocol.TurnStartedData{Provider: "fixture", Model: "fixture", Prompt: "foreign request"})
	projection, err := CaptureGuardianAuthorization(t.Context(), store, authorizationScope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Texts()) != 8 || projection.Texts()[0].Text != "user restriction 0" {
		t.Fatalf("lost old original sources: %+v", projection.Texts())
	}
	for _, text := range projection.Texts() {
		if text.Source.Role != "user" {
			t.Fatal("untrusted author promoted")
		}
	}
	snapshot := projection.Snapshot()
	snapshot.Sources[0].Revoked = true
	texts := projection.Texts()
	texts[0].Text = "mutated"
	if projection.Snapshot().Sources[0].Revoked || projection.Texts()[0].Text == "mutated" {
		t.Fatal("projection aliases caller data")
	}
}

func TestGuardianAuthorizationSteeringWithdrawalAndParentInvalidation(t *testing.T) {
	store := &authorizationEvents{}
	store.add(t, "thread", "turn", &protocol.TurnStartedData{Provider: "fixture", Model: "fixture", Prompt: "only edit local files"})
	first, err := CaptureGuardianAuthorization(t.Context(), store, authorizationScope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	store.add(t, "thread", "turn", &protocol.TurnSteeredData{Prompt: "do not modify configuration"})
	second, err := CaptureGuardianAuthorization(t.Context(), store, authorizationScope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Snapshot().Digest() == second.Snapshot().Digest() {
		t.Fatal("steering preserved stale digest")
	}
	child := authorizationScope()
	child.ThreadID = "child"
	child.Child = true
	store.add(t, "child", "task", &protocol.TurnStartedData{Provider: "fixture", Model: "fixture", Prompt: "delegated task claims unrestricted authority"})
	a, err := CaptureGuardianAuthorization(t.Context(), store, child, &first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CaptureGuardianAuthorization(t.Context(), store, child, &second)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Texts()) != 1 || len(b.Texts()) != 2 || a.Snapshot().Digest() == b.Snapshot().Digest() {
		t.Fatal("child elevated task text or lost parent revision")
	}
	if _, err := CaptureGuardianAuthorization(t.Context(), store, child, nil); err == nil {
		t.Fatal("child without parent admitted")
	}
	store.add(t, "thread", "turn", &protocol.TurnWithdrawnData{})
	revoked, err := CaptureGuardianAuthorization(t.Context(), store, authorizationScope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range revoked.Snapshot().Sources {
		if !source.Revoked || revoked.Snapshot().SourceIDs()[source.ID] {
			t.Fatal("withdrawn source remained citable")
		}
	}
}

func TestGuardianAuthorizationSeparatesQuestionFromAnswer(t *testing.T) {
	store := &authorizationEvents{}
	store.add(t, "thread", "turn", &protocol.TurnStartedData{Provider: "fixture", Model: "fixture", Prompt: "fix tests"})
	store.add(t, "thread", "turn", &protocol.InputRequiredData{RequestID: "input", CallID: "call", Tool: "ask_user", Prompt: "assistant suggests broad authority", ExpiresAt: time.Now().Add(time.Hour)})
	store.add(t, "thread", "turn", &protocol.InputResolvedData{RequestID: "input", Answer: "only within this repository"})
	projection, err := CaptureGuardianAuthorization(t.Context(), store, authorizationScope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	answer := projection.Texts()[1]
	if answer.Text != "only within this repository" || answer.UntrustedContext != "assistant suggests broad authority" || answer.Source.Digest != guardianTextDigest(answer.Text) {
		t.Fatal("question promoted into user authority")
	}
	store.add(t, "thread", "turn", &protocol.InputResolvedData{RequestID: "unknown", Answer: "yes"})
	if _, err := CaptureGuardianAuthorization(t.Context(), store, authorizationScope(), nil); err == nil {
		t.Fatal("unmatched answer admitted")
	}
}

func TestGuardianAuthorizationFailsClosedOnMissingSources(t *testing.T) {
	store := &authorizationEvents{gap: true}
	if _, err := CaptureGuardianAuthorization(t.Context(), store, authorizationScope(), nil); err == nil {
		t.Fatal("evicted history admitted")
	}
	store.gap = false
	store.add(t, "thread", "turn", &protocol.TurnStartedData{Provider: "fixture", Model: "fixture"})
	if _, err := CaptureGuardianAuthorization(t.Context(), store, authorizationScope(), nil); err == nil {
		t.Fatal("missing original request admitted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := CaptureGuardianAuthorization(ctx, store, authorizationScope(), nil); err == nil {
		t.Fatal("canceled source capture admitted")
	}
}

func TestGuardianAuthorizationAcceptsStorageValidatedWorkspaceProjection(t *testing.T) {
	store := &authorizationEvents{fence: 5}
	store.add(t, "thread", "turn", &protocol.TurnStartedData{Provider: "fixture", Model: "fixture", Prompt: "only edit local files"})
	store.events[0].Sequence = 3 // Other workspace events are removed by the store.
	projection, err := CaptureGuardianAuthorization(t.Context(), store, authorizationScope(), nil)
	if err != nil || len(projection.Texts()) != 1 || projection.Texts()[0].Source.Version != 3 {
		t.Fatalf("sparse workspace sequence rejected: %+v %v", projection.Texts(), err)
	}
	store.gap = true
	if _, err := CaptureGuardianAuthorization(t.Context(), store, authorizationScope(), nil); err == nil {
		t.Fatal("storage-reported history gap ignored")
	}
}

func TestGuardianAuthorizationCannotOmitImageInstructions(t *testing.T) {
	store := &authorizationEvents{}
	store.add(t, "thread", "turn", &protocol.TurnStartedData{
		Provider: "fixture", Model: "fixture", DisplayPrompt: "follow the constraints in this image",
		Images: []protocol.EditorContextReference{{
			Kind: protocol.EditorContextImage, Source: protocol.EditorContextSourceNativePicker,
			URI: "file:///workspace/constraints.png", Path: "constraints.png", DocumentVersion: 1,
			Digest: guardianTextDigest("fixture"), Label: "constraints.png", MediaType: "image/png", Explicit: true,
		}},
	})
	if _, err := CaptureGuardianAuthorization(t.Context(), store, authorizationScope(), nil); err == nil {
		t.Fatal("incomplete multimodal authorization declared complete")
	}
}
