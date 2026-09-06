package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	defaultSessionTitle     = "New Chat"
	sessionTitleMaxWords    = 5
	sessionTitleMaxUTF8Byte = 48
)

var sessionTitleEscapeSequence = regexp.MustCompile(
	`(?:\x1b\][^\x07]*(?:\x07|\x1b\\)|\x1b\[[0-?]*[ -/]*[@-~]|\x1b[@-_])`,
)

type sessionCreateStore interface {
	CreateLifecycle(
		context.Context,
		protocol.SessionCreateSeed,
	) (protocol.SessionSummary, error)
}

type sessionPresentationStore interface {
	PresentationReadFence(
		context.Context,
		string,
	) (protocol.SessionReadFence, error)
}

type CreateSessionRequest struct {
	SessionID      string
	IdempotencyKey string
	WorkspaceRoot  string
	WorkspaceLabel string
	Title          string
	Provider       string
	Model          string
	Isolation      string
}

type ActivateSessionRequest struct {
	SessionID     string
	ThreadID      protocol.ThreadID
	WorkspaceRoot string
}

type SessionBinding struct {
	SessionID     string            `json:"session_id"`
	ThreadID      protocol.ThreadID `json:"thread_id"`
	WorkspaceRoot string            `json:"workspace_root"`
	Provider      string            `json:"provider"`
	Model         string            `json:"model"`
	Isolation     string            `json:"isolation"`
}

type SubmitSessionOperation struct {
	SessionID         string
	Kind              protocol.OperationKind
	Payload           protocol.OperationPayload
	IdempotencyKey    string
	WorkspaceIdentity *protocol.WorkspaceIdentity
}

type OperationReceipt struct {
	OperationID protocol.OperationID   `json:"operation_id"`
	Kind        protocol.OperationKind `json:"kind"`
	ThreadID    protocol.ThreadID      `json:"thread_id"`
	TurnID      protocol.TurnID        `json:"turn_id"`
	ItemID      protocol.ItemID        `json:"item_id"`
	Accepted    bool                   `json:"accepted"`
}

func (r *SessionService) CreateSession(
	ctx context.Context,
	request CreateSessionRequest,
) (SessionBinding, error) {
	store, ok := r.sessionLifecycle.(sessionCreateStore)
	if !ok {
		return SessionBinding{}, runtimeProblem(
			protocol.CodeUnavailable,
			"session creation is unavailable",
			nil,
		)
	}
	request.Title = strings.TrimSpace(request.Title)
	if request.Title == "" {
		request.Title = defaultSessionTitle
	}
	request.WorkspaceRoot = strings.TrimSpace(request.WorkspaceRoot)
	if request.WorkspaceRoot == "" {
		return SessionBinding{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"workspace root is required",
			nil,
		)
	}
	if r.workspaceRoot != "" &&
		!sameWorkspaceRoot(r.workspaceRoot, request.WorkspaceRoot) {
		return SessionBinding{}, runtimeProblem(
			protocol.CodeConflict,
			"session workspace does not match the Runtime binding",
			nil,
		)
	}
	if request.WorkspaceLabel == "" {
		request.WorkspaceLabel = filepath.Base(request.WorkspaceRoot)
	}
	if request.Isolation == "" {
		request.Isolation = "shared"
	}
	if request.Provider == "" {
		request.Provider = r.defaultProfile.Provider
	}
	if request.Model == "" {
		request.Model = r.defaultProfile.Model
	}
	if request.Provider != r.defaultProfile.Provider ||
		request.Model != r.defaultProfile.Model {
		return SessionBinding{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"requested provider or model is unavailable in this Runtime",
			nil,
		)
	}
	if len(request.IdempotencyKey) > 256 {
		return SessionBinding{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"idempotency key exceeds 256 bytes",
			nil,
		)
	}
	var err error
	if request.SessionID == "" {
		if request.IdempotencyKey != "" {
			request.SessionID = sessionDerivedID(
				"session",
				request.IdempotencyKey,
				"session:create:"+request.WorkspaceRoot,
			)
		} else {
			request.SessionID, err = protocol.NewSessionID()
			if err != nil {
				return SessionBinding{}, err
			}
		}
	}
	if request.IdempotencyKey != "" {
		if binding, found := r.existingCreateBinding(ctx, request); found {
			return binding, nil
		}
	}
	var workspaceID string
	var threadID protocol.ThreadID
	if request.IdempotencyKey != "" {
		workspaceID = sessionDerivedID(
			"workspace",
			request.IdempotencyKey,
			"session-workspace:"+request.WorkspaceRoot,
		)
		threadID = protocol.ThreadID(sessionDerivedID(
			"thread",
			request.IdempotencyKey,
			"session-thread:"+request.SessionID,
		))
	} else {
		workspaceID, err = protocol.NewWorkspaceID()
		if err != nil {
			return SessionBinding{}, err
		}
		threadID, err = protocol.NewThreadID()
		if err != nil {
			return SessionBinding{}, err
		}
	}
	seed := protocol.SessionCreateSeed{
		Version:        protocol.SessionLifecycleVersion,
		SessionID:      request.SessionID,
		WorkspaceID:    workspaceID,
		WorkspaceRoot:  request.WorkspaceRoot,
		WorkspaceLabel: request.WorkspaceLabel,
		ThreadID:       threadID,
		Title:          request.Title,
		Provider:       request.Provider,
		Model:          request.Model,
		Isolation:      request.Isolation,
	}
	if err := seed.Validate(); err != nil {
		return SessionBinding{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			err.Error(),
			err,
		)
	}
	provisioned := false
	if seed.Isolation == SessionIsolationWorktree {
		if r.sessionWorkspaces == nil {
			return SessionBinding{}, runtimeProblem(
				protocol.CodeUnavailable,
				"isolated session workspaces are unavailable",
				nil,
			)
		}
		if _, err := r.sessionWorkspaces.Provision(
			ctx,
			seed.SessionID,
			seed.ThreadID,
		); err != nil {
			return SessionBinding{}, err
		}
		provisioned = true
	}
	if _, err := store.CreateLifecycle(ctx, seed); err != nil {
		if provisioned {
			_ = r.sessionWorkspaces.Discard(
				context.Background(),
				seed.SessionID,
				seed.ThreadID,
			)
		}
		if request.IdempotencyKey != "" {
			if binding, found := r.existingCreateBinding(ctx, request); found {
				return binding, nil
			}
		}
		return SessionBinding{}, err
	}
	if err := r.BindThreadSession(seed.ThreadID, seed.SessionID); err != nil {
		return SessionBinding{}, err
	}
	if r.SessionProfilesAvailable() {
		if _, err := r.RestoreSessionProfile(
			ctx,
			seed.SessionID,
			seed.ThreadID,
		); err != nil {
			return SessionBinding{}, err
		}
	}
	return bindingFromSeed(seed), nil
}

func (r *SessionService) existingCreateBinding(
	ctx context.Context,
	request CreateSessionRequest,
) (SessionBinding, bool) {
	summary, err := r.sessionLifecycle.GetLifecycle(ctx, request.SessionID)
	if err != nil {
		return SessionBinding{}, false
	}
	if !sameWorkspaceRoot(summary.WorkspaceRoot, request.WorkspaceRoot) ||
		summary.Title != request.Title ||
		summary.Isolation != request.Isolation ||
		summary.Provider != request.Provider ||
		summary.Model != request.Model {
		return SessionBinding{}, false
	}
	return bindingFromSummary(summary), true
}

func (r *SessionService) ActivateSession(
	ctx context.Context,
	request ActivateSessionRequest,
) (SessionBinding, error) {
	if strings.TrimSpace(request.SessionID) == "" {
		return SessionBinding{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"session id is required",
			nil,
		)
	}
	summary, err := r.SessionStatus(ctx, request.SessionID)
	if err != nil {
		return SessionBinding{}, err
	}
	if request.WorkspaceRoot != "" &&
		request.WorkspaceRoot != summary.WorkspaceRoot {
		return SessionBinding{}, runtimeProblem(
			protocol.CodeConflict,
			"session does not belong to this workspace",
			nil,
		)
	}
	threadID := request.ThreadID
	if threadID == "" {
		threadID = summary.ThreadID
	}
	owner, err := r.sessionLifecycle.SessionForThread(ctx, threadID)
	if err != nil {
		return SessionBinding{}, err
	}
	if owner != request.SessionID {
		return SessionBinding{}, runtimeProblem(
			protocol.CodeConflict,
			"thread does not belong to the requested session",
			nil,
		)
	}
	if summary.Isolation == SessionIsolationWorktree {
		if r.sessionWorkspaces == nil {
			return SessionBinding{}, runtimeProblem(
				protocol.CodeUnavailable,
				"isolated session workspaces are unavailable",
				nil,
			)
		}
		if _, err := r.sessionWorkspaces.Restore(
			ctx,
			request.SessionID,
			threadID,
		); err != nil {
			return SessionBinding{}, err
		}
	}
	summary, err = r.sessionLifecycle.ActivateThread(
		ctx,
		request.SessionID,
		threadID,
	)
	if err != nil {
		return SessionBinding{}, err
	}
	if err := r.BindThreadSession(threadID, request.SessionID); err != nil {
		return SessionBinding{}, err
	}
	if r.SessionProfilesAvailable() {
		if _, err := r.RestoreSessionProfile(
			ctx,
			request.SessionID,
			threadID,
		); err != nil {
			return SessionBinding{}, err
		}
	}
	return bindingFromSummary(summary), nil
}

func (r *SessionService) SessionForThread(
	ctx context.Context,
	threadID protocol.ThreadID,
) (string, error) {
	if r.sessionLifecycle == nil {
		return "", runtimeProblem(
			protocol.CodeUnavailable,
			"session lifecycle is unavailable",
			nil,
		)
	}
	return r.sessionLifecycle.SessionForThread(ctx, threadID)
}

func (r *OperationService) SubmitForSession(
	ctx context.Context,
	request SubmitSessionOperation,
) (OperationReceipt, error) {
	r.SessionService.mutationMu.Lock()
	defer r.SessionService.mutationMu.Unlock()
	if request.Payload == nil || request.Kind == "" {
		return OperationReceipt{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"operation kind and payload are required",
			nil,
		)
	}
	if len(request.IdempotencyKey) > 256 {
		return OperationReceipt{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"idempotency key exceeds 256 bytes",
			nil,
		)
	}
	summary, err := r.SessionStatus(ctx, request.SessionID)
	if err != nil {
		return OperationReceipt{}, err
	}
	if request.WorkspaceIdentity != nil &&
		!sameWorkspaceRoot(
			summary.WorkspaceRoot,
			request.WorkspaceIdentity.RuntimePath,
		) {
		return OperationReceipt{}, runtimeProblem(
			protocol.CodeConflict,
			"session workspace does not match the Runtime binding",
			nil,
		)
	}
	if summary.Archived {
		return OperationReceipt{}, runtimeProblem(
			protocol.CodeConflict,
			"archived session does not accept operations",
			nil,
		)
	}
	if start, ok := request.Payload.(*protocol.StartTurnPayload); ok {
		if request.WorkspaceIdentity == nil {
			return OperationReceipt{}, runtimeProblem(
				protocol.CodeInvalidArgument,
				"workspace identity is required for a new turn",
				nil,
			)
		}
		if start.WorkspaceIdentity == nil {
			identity := *request.WorkspaceIdentity
			start.WorkspaceIdentity = &identity
		} else if *start.WorkspaceIdentity != *request.WorkspaceIdentity {
			return OperationReceipt{}, runtimeProblem(
				protocol.CodeConflict,
				"turn workspace identity does not match the Runtime binding",
				nil,
			)
		}
	}
	if enqueue, ok := request.Payload.(*protocol.EnqueueTurnPayload); ok {
		if request.WorkspaceIdentity == nil {
			return OperationReceipt{}, runtimeProblem(
				protocol.CodeInvalidArgument,
				"workspace identity is required for a queued turn",
				nil,
			)
		}
		if enqueue.WorkspaceIdentity == nil {
			identity := *request.WorkspaceIdentity
			enqueue.WorkspaceIdentity = &identity
		} else if *enqueue.WorkspaceIdentity != *request.WorkspaceIdentity {
			return OperationReceipt{}, runtimeProblem(
				protocol.CodeConflict,
				"queued turn workspace identity does not match the Runtime binding",
				nil,
			)
		}
		if enqueue.QueueID == "" {
			if request.IdempotencyKey == "" {
				return OperationReceipt{}, runtimeProblem(
					protocol.CodeInvalidArgument,
					"queued turn requires an idempotency key",
					nil,
				)
			}
			enqueue.QueueID = sessionDerivedID(
				"queue",
				request.IdempotencyKey,
				request.SessionID,
			)
		}
	}
	if cancel, ok := request.Payload.(*protocol.CancelTurnPayload); ok {
		active, found := r.active.LookupTurn(cancel.TurnID)
		if !found {
			return OperationReceipt{}, turnNotActiveProblem()
		}
		if cancel.ThreadID != "" && cancel.ThreadID != active.ThreadID {
			return OperationReceipt{}, runtimeProblem(
				protocol.CodeConflict,
				"turn does not belong to the requested thread",
				nil,
			)
		}
	}
	if err := r.bindPendingSessionRequest(
		ctx,
		request.SessionID,
		request.Payload,
	); err != nil {
		return OperationReceipt{}, err
	}
	threadID, turnID, _ := protocol.PayloadReferences(request.Payload)
	if threadID == "" {
		threadID = summary.ThreadID
	} else if err := r.requireSessionThread(
		ctx,
		request.SessionID,
		threadID,
	); err != nil {
		return OperationReceipt{}, err
	}
	if turnID == "" {
		if request.Kind != protocol.OperationStartTurn {
			return OperationReceipt{}, runtimeProblem(
				protocol.CodeInvalidArgument,
				fmt.Sprintf("%s requires turn_id", request.Kind),
				nil,
			)
		}
		turnID, err = sessionTurnID(
			request.IdempotencyKey,
			threadID,
		)
		if err != nil {
			return OperationReceipt{}, err
		}
	}
	itemID, err := sessionItemID(
		request.IdempotencyKey,
		request.Kind,
		turnID,
	)
	if err != nil {
		return OperationReceipt{}, err
	}
	protocol.FillOperationReferences(
		request.Payload,
		threadID,
		turnID,
		itemID,
	)
	if fork, ok := request.Payload.(*protocol.ForkThreadPayload); ok &&
		fork.NewThreadID == "" {
		value, err := sessionDerivedOrRandomID(
			"thread",
			request.IdempotencyKey,
			"fork:"+string(turnID),
		)
		if err != nil {
			return OperationReceipt{}, err
		}
		fork.NewThreadID = protocol.ThreadID(value)
	}
	operation, err := protocol.NewOperation(request.Payload)
	if err != nil {
		return OperationReceipt{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			err.Error(),
			err,
		)
	}
	if request.IdempotencyKey != "" {
		operation.ID = protocol.OperationID(sessionDerivedID(
			"op",
			request.IdempotencyKey,
			string(request.Kind)+":"+string(threadID),
		))
	}
	if err := r.SubmitWithKey(
		ctx,
		operation,
		request.IdempotencyKey,
	); err != nil {
		return OperationReceipt{}, err
	}
	if start, ok := request.Payload.(*protocol.StartTurnPayload); ok &&
		summary.Title == defaultSessionTitle &&
		summary.LatestSequence == 0 {
		prompt := start.DisplayPrompt
		if prompt == "" {
			prompt = start.Prompt
		}
		if title := promptSessionTitle(prompt); title != "" {
			titlePatch := protocol.SessionLifecyclePatch{Title: &title}
			if _, titleErr := r.sessionLifecycle.UpdateLifecycle(
				ctx,
				request.SessionID,
				summary.Revision,
				titlePatch,
			); titleErr != nil && r.logger != nil {
				r.logger.Warn(
					"automatic session title update failed",
					"session_id",
					request.SessionID,
					"error",
					titleErr,
				)
			}
		}
	}
	if fork, ok := request.Payload.(*protocol.ForkThreadPayload); ok {
		if err := r.BindThreadSession(
			fork.NewThreadID,
			request.SessionID,
		); err != nil {
			return OperationReceipt{}, err
		}
	}
	return OperationReceipt{
		OperationID: operation.ID,
		Kind:        operation.Kind,
		ThreadID:    threadID,
		TurnID:      turnID,
		ItemID:      itemID,
		Accepted:    true,
	}, nil
}

func promptSessionTitle(prompt string) string {
	prompt = sessionTitleEscapeSequence.ReplaceAllString(prompt, "")
	clean := strings.Map(func(value rune) rune {
		if unicode.IsSpace(value) {
			return ' '
		}
		if unicode.IsControl(value) || unicode.Is(unicode.Cf, value) {
			return -1
		}
		return value
	}, prompt)
	clean = strings.Join(strings.Fields(clean), " ")
	for _, prefix := range []string{"请帮我", "请你", "帮我", "请"} {
		if strings.HasPrefix(clean, prefix) {
			clean = strings.TrimSpace(strings.TrimPrefix(clean, prefix))
			break
		}
	}
	if end := strings.IndexAny(clean, "。！？!?；;"); end >= 0 {
		clean = strings.TrimSpace(clean[:end])
	}
	words := strings.Fields(clean)
	if len(words) > sessionTitleMaxWords {
		clean = strings.Join(words[:sessionTitleMaxWords], " ")
	}
	if len(clean) <= sessionTitleMaxUTF8Byte {
		return clean
	}
	var title strings.Builder
	title.Grow(sessionTitleMaxUTF8Byte)
	for _, value := range clean {
		if title.Len()+utf8.RuneLen(value) > sessionTitleMaxUTF8Byte {
			break
		}
		title.WriteRune(value)
	}
	return strings.TrimSpace(title.String())
}

func bindingFromSeed(seed protocol.SessionCreateSeed) SessionBinding {
	return SessionBinding{
		SessionID: seed.SessionID, ThreadID: seed.ThreadID,
		WorkspaceRoot: seed.WorkspaceRoot,
		Provider:      seed.Provider, Model: seed.Model,
		Isolation: seed.Isolation,
	}
}

func bindingFromSummary(summary protocol.SessionSummary) SessionBinding {
	return SessionBinding{
		SessionID: summary.SessionID, ThreadID: summary.ThreadID,
		WorkspaceRoot: summary.WorkspaceRoot,
		Provider:      summary.Provider, Model: summary.Model,
		Isolation: summary.Isolation,
	}
}

func (r *OperationService) bindPendingSessionRequest(
	ctx context.Context,
	sessionID string,
	payload protocol.OperationPayload,
) error {
	var threadID protocol.ThreadID
	switch value := payload.(type) {
	case *protocol.ApprovalDecisionPayload:
		pending, ok := r.PendingApproval(value.RequestID)
		if !ok {
			return nil
		}
		threadID = pending.ThreadID
		if pending.Data.Source != nil &&
			pending.Data.Source.SessionID != "" &&
			pending.Data.Source.SessionID != sessionID {
			return runtimeProblem(
				protocol.CodeConflict,
				"approval request belongs to another session",
				nil,
			)
		}
		value.ThreadID, value.TurnID = pending.ThreadID, pending.TurnID
	case *protocol.InputReplyPayload:
		pending, ok := r.PendingInput(value.RequestID)
		if !ok {
			return nil
		}
		threadID = pending.ThreadID
		value.ThreadID, value.TurnID = pending.ThreadID, pending.TurnID
	default:
		return nil
	}
	return r.requireSessionThread(ctx, sessionID, threadID)
}

func (r *OperationService) requireSessionThread(
	ctx context.Context,
	sessionID string,
	threadID protocol.ThreadID,
) error {
	owner, err := r.sessionLifecycle.SessionForThread(ctx, threadID)
	if err != nil {
		return err
	}
	if owner != sessionID {
		return runtimeProblem(
			protocol.CodeConflict,
			"thread belongs to another session",
			nil,
		)
	}
	return nil
}

func sessionTurnID(
	key string,
	threadID protocol.ThreadID,
) (protocol.TurnID, error) {
	value, err := sessionDerivedOrRandomID(
		"turn",
		key,
		"turn:"+string(threadID),
	)
	return protocol.TurnID(value), err
}

func sessionItemID(
	key string,
	kind protocol.OperationKind,
	turnID protocol.TurnID,
) (protocol.ItemID, error) {
	value, err := sessionDerivedOrRandomID(
		"item",
		key,
		string(kind)+":"+string(turnID),
	)
	return protocol.ItemID(value), err
}

func sessionDerivedOrRandomID(
	prefix, key, namespace string,
) (string, error) {
	if key != "" {
		return sessionDerivedID(prefix, key, namespace), nil
	}
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate %s id: %w", prefix, err)
	}
	return prefix + "_" + hex.EncodeToString(value[:]), nil
}

func sessionDerivedID(prefix, key, namespace string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + key))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}

func sameWorkspaceRoot(left, right string) bool {
	leftAbsolute, leftErr := filepath.Abs(strings.TrimSpace(left))
	rightAbsolute, rightErr := filepath.Abs(strings.TrimSpace(right))
	leftPhysical, leftPhysicalErr := filepath.EvalSymlinks(leftAbsolute)
	rightPhysical, rightPhysicalErr := filepath.EvalSymlinks(rightAbsolute)
	return leftErr == nil && rightErr == nil && (filepath.Clean(leftAbsolute) == filepath.Clean(rightAbsolute) ||
		leftPhysicalErr == nil && rightPhysicalErr == nil && filepath.Clean(leftPhysical) == filepath.Clean(rightPhysical))
}

func (r *SessionService) UpdateSessionLifecycle(
	ctx context.Context,
	sessionID string,
	expectedRevision uint64,
	patch protocol.SessionLifecyclePatch,
) (protocol.SessionLifecycleUpdate, error) {
	r.mutationMu.Lock()
	defer r.mutationMu.Unlock()
	if r.sessionLifecycle == nil {
		return protocol.SessionLifecycleUpdate{}, runtimeProblem(protocol.CodeUnavailable, "session lifecycle is unavailable", nil)
	}
	current, err := r.SessionStatus(ctx, sessionID)
	if err != nil {
		return protocol.SessionLifecycleUpdate{}, err
	}
	if patch.Archived != nil && *patch.Archived {
		if err := ensureSessionQuiescent(current, "archive"); err != nil {
			return protocol.SessionLifecycleUpdate{}, err
		}
	}
	updated, err := r.sessionLifecycle.UpdateLifecycle(
		ctx,
		sessionID,
		expectedRevision,
		patch,
	)
	if err != nil {
		return protocol.SessionLifecycleUpdate{}, err
	}
	updated, err = r.projectSessionActivity(ctx, updated)
	if err != nil {
		return protocol.SessionLifecycleUpdate{}, err
	}
	return protocol.SessionLifecycleUpdate{
		Session: updated,
	}, nil
}

func (r *SessionService) DeleteSession(
	ctx context.Context,
	sessionID string,
	expectedRevision uint64,
) (protocol.SessionDeleteResult, error) {
	return r.deleteSession(ctx, sessionID, expectedRevision, false)
}

func (r *SessionService) DiscardSession(
	ctx context.Context,
	sessionID string,
	expectedRevision uint64,
) (protocol.SessionDeleteResult, error) {
	return r.deleteSession(ctx, sessionID, expectedRevision, true)
}

func (r *SessionService) deleteSession(
	ctx context.Context,
	sessionID string,
	expectedRevision uint64,
	discard bool,
) (protocol.SessionDeleteResult, error) {
	r.mutationMu.Lock()
	defer r.mutationMu.Unlock()
	if r.sessionLifecycle == nil {
		return protocol.SessionDeleteResult{}, runtimeProblem(protocol.CodeUnavailable, "session lifecycle is unavailable", nil)
	}
	current, err := r.SessionStatus(ctx, sessionID)
	if err != nil {
		return protocol.SessionDeleteResult{}, err
	}
	threadIDs, err := r.sessionLifecycle.ThreadIDs(ctx, sessionID)
	if err != nil {
		return protocol.SessionDeleteResult{}, err
	}
	if discard {
		for _, threadID := range threadIDs {
			if _, active := r.active.LookupThread(threadID); active {
				return protocol.SessionDeleteResult{}, sessionBusyProblem(
					"cannot discard session while a turn is active",
					current,
				)
			}
		}
		if r.OperationService.hasPendingSession(sessionID) {
			return protocol.SessionDeleteResult{}, sessionBusyProblem(
				"cannot discard session while a turn is recovering",
				current,
			)
		}
	} else if err := ensureSessionQuiescent(current, "delete"); err != nil {
		return protocol.SessionDeleteResult{}, err
	}
	if err := r.reclaimSessionWorkspaceDrafts(ctx, threadIDs); err != nil {
		return protocol.SessionDeleteResult{}, err
	}
	if current.Isolation == SessionIsolationWorktree {
		if r.sessionWorkspaces == nil {
			return protocol.SessionDeleteResult{}, runtimeProblem(protocol.CodeUnavailable, "isolated Chat workspaces are unavailable", nil)
		}
		if _, err := r.sessionWorkspaces.Restore(
			ctx,
			current.SessionID,
			current.ThreadID,
		); err != nil {
			return protocol.SessionDeleteResult{}, err
		}
		if !discard {
			plan, err := r.sessionWorkspaces.PlanMerge(
				ctx,
				current.SessionID,
				current.ThreadID,
			)
			if err != nil && !errors.Is(err, ErrSessionWorkspaceClean) {
				return protocol.SessionDeleteResult{}, runtimeProblem(
					protocol.CodeConflict,
					"cannot delete session while its isolated worktree has unresolved changes",
					err,
				)
			}
			if err == nil && len(plan.Files) != 0 {
				return protocol.SessionDeleteResult{}, runtimeProblem(protocol.CodeConflict, "cannot delete session with unmerged worktree changes", nil)
			}
		}
	}
	var result protocol.SessionDeleteResult
	if discard {
		store, ok := r.sessionLifecycle.(sessionDiscardStore)
		if !ok {
			return protocol.SessionDeleteResult{}, runtimeProblem(
				protocol.CodeUnavailable,
				"discarding a session is unavailable",
				nil,
			)
		}
		result, err = store.DiscardLifecycle(ctx, sessionID, expectedRevision)
	} else {
		result, err = r.sessionLifecycle.DeleteLifecycle(
			ctx,
			sessionID,
			expectedRevision,
		)
	}
	if err != nil {
		return protocol.SessionDeleteResult{}, err
	}
	if manager, ok := r.engine.(*ThreadManager); ok && manager != nil {
		for _, threadID := range threadIDs {
			manager.Release(threadID)
		}
	}
	if discard {
		r.clearSessionInteractions(threadIDs)
	}
	r.TurnQueueService.clearThreads(threadIDs)
	if current.Isolation == SessionIsolationWorktree {
		if discardErr := r.sessionWorkspaces.Discard(
			ctx,
			current.SessionID,
			current.ThreadID,
		); discardErr != nil {
			r.logger.Error(
				"discard deleted Session worktree",
				"session_id", current.SessionID,
				"thread_id", current.ThreadID,
				"error", discardErr,
			)
		}
	}
	return result, nil
}

func (r *SessionService) clearSessionInteractions(threadIDs []protocol.ThreadID) {
	threads := make(map[protocol.ThreadID]struct{}, len(threadIDs))
	for _, threadID := range threadIDs {
		threads[threadID] = struct{}{}
	}
	r.EventService.mu.Lock()
	defer r.EventService.mu.Unlock()
	for requestID, approval := range r.approvals {
		if _, ok := threads[approval.ThreadID]; ok {
			delete(r.approvals, requestID)
			delete(r.approvalItems, eventItemOwner(approval.TurnID, requestID))
		}
	}
	for requestID, input := range r.inputs {
		if _, ok := threads[input.ThreadID]; ok {
			delete(r.inputs, requestID)
			delete(r.inputItems, eventItemOwner(input.TurnID, requestID))
		}
	}
}
