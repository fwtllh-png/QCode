package agentcontext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/guardian"
)

// AuthorizationEventStore is the existing durable event replay contract. A
// fixed inclusive fence and storage-level gap checks are required: a retained
// tail or model transcript cannot prove old restrictions were included. A
// workspace-filtered store may retain sparse process-wide sequence numbers.
type AuthorizationEventStore interface {
	LastSequence(context.Context) (protocol.Cursor, error)
	ReplayThrough(context.Context, protocol.Cursor, protocol.Cursor, int) ([]protocol.Event, bool, error)
}

type AuthorizationScope struct {
	WorkspaceID, SessionID, ThreadID string
	Child                            bool
}

// UserAuthorizationText separates original user text from untrusted context
// such as the model's question. Context is never a citable user source.
type UserAuthorizationText struct {
	Source           guardian.AuthorizationSource
	Text             string
	UntrustedContext string
}

// GuardianAuthorization owns a frozen, complete source projection for one
// invocation. It is independent of history compaction and prompt tail limits.
type GuardianAuthorization struct {
	snapshot guardian.AuthorizationSnapshot
	texts    []UserAuthorizationText
}

func (a GuardianAuthorization) Snapshot() guardian.AuthorizationSnapshot {
	s := a.snapshot
	s.Sources = append([]guardian.AuthorizationSource(nil), s.Sources...)
	return s
}

func (a GuardianAuthorization) Texts() []UserAuthorizationText {
	return append([]UserAuthorizationText(nil), a.texts...)
}

// CaptureGuardianAuthorization reads only committed Runtime events. Parent is
// captured by the parent's Runtime; a delegated task description supplies no
// additional user authority. Provider messages, plans and tool output are never
// inputs to this projection.
func CaptureGuardianAuthorization(ctx context.Context, store AuthorizationEventStore, scope AuthorizationScope, parent *GuardianAuthorization) (GuardianAuthorization, error) {
	if store == nil || scope.WorkspaceID == "" || scope.SessionID == "" || scope.ThreadID == "" {
		return GuardianAuthorization{}, errors.New("guardian authorization requires a durable source and scope")
	}
	if scope.Child && (parent == nil || parent.snapshot.Validate() != nil) {
		return GuardianAuthorization{}, errors.New("guardian child authorization requires the parent snapshot")
	}
	if parent != nil && (parent.snapshot.Validate() != nil || parent.snapshot.SessionID != scope.SessionID) {
		return GuardianAuthorization{}, errors.New("guardian parent authorization scope is invalid")
	}
	fence, err := store.LastSequence(ctx)
	if err != nil {
		return GuardianAuthorization{}, err
	}
	events, more, err := store.ReplayThrough(ctx, 0, fence, 0)
	if err != nil {
		return GuardianAuthorization{}, err
	}
	if more {
		return GuardianAuthorization{}, errors.New("guardian authorization history is incomplete")
	}
	result := GuardianAuthorization{snapshot: guardian.AuthorizationSnapshot{
		WorkspaceID: scope.WorkspaceID, SessionID: scope.SessionID, ThreadID: scope.ThreadID, Complete: true,
	}}
	if parent != nil {
		result.snapshot.ParentDigest = parent.snapshot.Digest()
		result.texts = parent.Texts()
	}
	withdrawn := make(map[protocol.TurnID]bool)
	questions := make(map[string]struct {
		turn protocol.TurnID
		text string
	})
	seen := make(map[protocol.EventID]bool)
	var previous protocol.Cursor
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return GuardianAuthorization{}, err
		}
		if event.Sequence <= previous || event.Sequence > fence || event.ID == "" || seen[event.ID] {
			return GuardianAuthorization{}, errors.New("guardian authorization event identity is invalid")
		}
		previous = event.Sequence
		seen[event.ID] = true
		if string(event.ThreadID) != scope.ThreadID {
			continue
		}
		if err := event.Validate(); err != nil {
			return GuardianAuthorization{}, err
		}
		var text, untrusted string
		switch data := event.Data.(type) {
		case *protocol.TurnStartedData:
			if scope.Child || data.PlanID != "" {
				continue
			}
			if len(data.Images) != 0 {
				return GuardianAuthorization{}, errors.New("guardian authorization does not cover image input")
			}
			text = data.DisplayPrompt
			if text == "" && len(data.EditorContext) == 0 && len(data.Images) == 0 {
				text = data.Prompt
			}
			if text == "" {
				return GuardianAuthorization{}, errors.New("guardian original user request is unavailable")
			}
		case *protocol.TurnSteeredData:
			if scope.Child {
				continue
			}
			text = data.Prompt
		case *protocol.InputRequiredData:
			if _, exists := questions[data.RequestID]; exists {
				return GuardianAuthorization{}, errors.New("duplicate guardian input request")
			}
			questions[data.RequestID] = struct {
				turn protocol.TurnID
				text string
			}{event.TurnID, data.Prompt}
		case *protocol.InputResolvedData:
			if scope.Child || data.Answer == "" {
				continue
			}
			question, found := questions[data.RequestID]
			if !found || question.turn != event.TurnID {
				return GuardianAuthorization{}, errors.New("guardian user answer has no matching question")
			}
			text, untrusted = data.Answer, question.text
			delete(questions, data.RequestID)
		case *protocol.TurnWithdrawnData:
			withdrawn[event.TurnID] = true
		}
		if text != "" {
			source := guardian.AuthorizationSource{ID: string(event.ID), ThreadID: scope.ThreadID, TurnID: string(event.TurnID), Role: "user", Version: uint64(event.Sequence), Digest: guardianTextDigest(text)}
			result.texts = append(result.texts, UserAuthorizationText{Source: source, Text: text, UntrustedContext: untrusted})
		}
	}
	for index := range result.texts {
		text := &result.texts[index]
		if text.Source.ThreadID == scope.ThreadID && withdrawn[protocol.TurnID(text.Source.TurnID)] {
			text.Source.Revoked = true
		}
		result.snapshot.Sources = append(result.snapshot.Sources, text.Source)
	}
	// Bind questions as well as answers; replacing context cannot retain the
	// old authorization digest. Revoked facts remain as explicit tombstones.
	encoded, err := json.Marshal(struct {
		Scope  AuthorizationScope
		Parent string
		Texts  []UserAuthorizationText
	}{scope, result.snapshot.ParentDigest, result.texts})
	if err != nil {
		return GuardianAuthorization{}, err
	}
	result.snapshot.Revision = guardianTextDigest(string(encoded))
	if err := result.snapshot.Validate(); err != nil {
		return GuardianAuthorization{}, fmt.Errorf("guardian authorization: %w", err)
	}
	return result, nil
}

func guardianTextDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
