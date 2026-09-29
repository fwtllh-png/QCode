package contract_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/config"
	contract "github.com/fwtllh-png/QCode/internal/host/runtimeapi/runtimecontract"
	threadstate "github.com/fwtllh-png/QCode/internal/persist/thread"
	runtimeview "github.com/fwtllh-png/QCode/internal/host/runtimeapi/view"
	usagestate "github.com/fwtllh-png/QCode/internal/observability/usage"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	apppersistence "github.com/fwtllh-png/QCode/internal/runtime/app/persistence"
	"github.com/fwtllh-png/QCode/internal/runtime/app/wire"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestRuntimeApplicationMeetsTheHostContract(t *testing.T) {
	contract.Run(t, newRuntimeContractHost)
}

type runtimeContractHost struct {
	runtime      *app.Runtime
	repositories apppersistence.PersistentRepositories
	agents       *subagent.AgentControl
	workspace    string
	identity     protocol.WorkspaceIdentity
	sessionID    string
	threadID     protocol.ThreadID
}

func newRuntimeContractHost(
	t *testing.T,
	setup contract.Setup,
) contract.Host {
	t.Helper()
	workspace := setup.Workspace
	if workspace == "" {
		workspace = t.TempDir()
	}
	identity := protocol.WorkspaceIdentity{}
	if setup.WorkspaceIdentity != nil {
		identity = *setup.WorkspaceIdentity
	} else {
		var err error
		identity, err = protocol.NewWorkspaceIdentity(
			(&url.URL{Scheme: "file", Path: workspace}).String(),
			workspace,
			"",
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	store, err := state.Open(t.Context(), state.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	overrides := config.Overrides{Workspace: &workspace}
	if setup.Tools {
		enabled := true
		overrides.Tools = &enabled
	}
	if setup.MaxSteps > 0 {
		overrides.MaxSteps = &setup.MaxSteps
	}
	session, err := wire.NewExec(t.Context(), wire.ExecOptions{
		FixturePath: setup.Fixture, Permission: "bypass",
		RepositoryRulesPath: setup.RepositoryRules,
		MCPConfigPath:       setup.WriteMCPConfig(t, store.Root()),
		ConfigOverrides:     overrides, PersistentStore: store,
		WorkspaceIdentity: identity,
	})
	if err != nil {
		_ = store.CloseAll(context.Background())
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := session.Close(ctx); err != nil {
			t.Errorf("close Runtime contract session: %v", err)
		}
	})
	repositories, err := apppersistence.NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := session.Runtime.CreateSession(
		t.Context(),
		app.CreateSessionRequest{
			Title:          "contract",
			WorkspaceRoot:  workspace,
			WorkspaceLabel: "contract",
			Provider:       session.DefaultProfile().Provider,
			Model:          session.DefaultProfile().Model,
			Isolation:      "shared",
			IdempotencyKey: "runtime-contract",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	host := &runtimeContractHost{
		runtime: session.Runtime, repositories: repositories,
		agents:    session.Subagents(),
		workspace: workspace, identity: identity,
		sessionID: binding.SessionID, threadID: binding.ThreadID,
	}
	return host
}

func (h *runtimeContractHost) Transport() string { return "runtime-app" }

func (h *runtimeContractHost) StartTurn(
	ctx context.Context,
	prompt string,
) (contract.Receipt, error) {
	return h.submit(ctx, protocol.OperationStartTurn, &protocol.StartTurnPayload{
		Prompt: prompt,
	})
}

func (h *runtimeContractHost) StartTurnWithContext(
	ctx context.Context,
	prompt string,
	workspaceIdentity *protocol.WorkspaceIdentity,
	editorContext []protocol.EditorContextReference,
) (contract.Receipt, error) {
	return h.submit(ctx, protocol.OperationStartTurn, &protocol.StartTurnPayload{
		Prompt: prompt, WorkspaceIdentity: workspaceIdentity, Context: editorContext,
	})
}

func (h *runtimeContractHost) Cancel(
	ctx context.Context,
	turn contract.Receipt,
	reason string,
) (contract.Receipt, error) {
	return h.submit(ctx, protocol.OperationCancelTurn, &protocol.CancelTurnPayload{
		ThreadID: turn.ThreadID, TurnID: turn.TurnID, Reason: reason,
	})
}

func (h *runtimeContractHost) Decide(
	ctx context.Context,
	turn contract.Receipt,
	requestID string,
	decision protocol.ApprovalDecision,
	planID string,
) (contract.Receipt, error) {
	return h.submit(
		ctx,
		protocol.OperationApprovalDecision,
		&protocol.ApprovalDecisionPayload{
			ThreadID: turn.ThreadID, TurnID: turn.TurnID,
			RequestID: requestID, Decision: decision,
			Scope: protocol.ApprovalScopeOnce, PlanID: planID,
		},
	)
}

func (h *runtimeContractHost) ReplyInput(
	ctx context.Context,
	turn contract.Receipt,
	requestID, answer string,
	values map[string]string,
) (contract.Receipt, error) {
	return h.submit(ctx, protocol.OperationInputReply, &protocol.InputReplyPayload{
		ThreadID: turn.ThreadID, TurnID: turn.TurnID,
		RequestID: requestID, Answer: answer, Values: values,
	})
}

func (h *runtimeContractHost) RecoverTurn(
	ctx context.Context,
	sourceTurnID protocol.TurnID,
	action protocol.TurnRecoveryAction,
	prompt string,
) (contract.Receipt, error) {
	request := protocol.TurnRecoveryRequest{
		Version: protocol.WorkflowIntentVersion, SessionID: h.sessionID,
		SourceTurnID: sourceTurnID, Action: action, Prompt: prompt,
		IdempotencyKey: "recover-" + string(sourceTurnID),
	}
	prepared, err := h.runtime.PrepareTurnRecovery(ctx, request)
	if err != nil {
		return contract.Receipt{}, contractError(err)
	}
	receipt, err := h.runtime.SubmitForSession(ctx, app.SubmitSessionOperation{
		SessionID: h.sessionID, Kind: protocol.OperationStartTurn,
		IdempotencyKey:    prepared.IdempotencyKey,
		WorkspaceIdentity: &h.identity,
		Payload: &protocol.StartTurnPayload{
			Prompt: prepared.Prompt, DisplayPrompt: prepared.DisplayPrompt,
			Intent: prepared.Intent, Recovery: &prepared.Recovery,
		},
	})
	if err != nil {
		return contract.Receipt{}, contractError(err)
	}
	return contract.Receipt{
		ThreadID: receipt.ThreadID, TurnID: receipt.TurnID, ItemID: receipt.ItemID,
	}, nil
}

func (h *runtimeContractHost) submit(
	ctx context.Context,
	kind protocol.OperationKind,
	payload protocol.OperationPayload,
) (contract.Receipt, error) {
	receipt, err := h.runtime.SubmitForSession(ctx, app.SubmitSessionOperation{
		SessionID: h.sessionID, Kind: kind, Payload: payload,
		IdempotencyKey:    string(kind) + "-" + time.Now().UTC().Format(time.RFC3339Nano),
		WorkspaceIdentity: &h.identity,
	})
	if err != nil {
		return contract.Receipt{}, contractError(err)
	}
	return contract.Receipt{
		ThreadID: receipt.ThreadID, TurnID: receipt.TurnID, ItemID: receipt.ItemID,
	}, nil
}

func (h *runtimeContractHost) Live(
	ctx context.Context,
	since protocol.Cursor,
) (<-chan protocol.Event, error) {
	events, err := h.runtime.EventsLimited(ctx, since, 10_000)
	return events, contractError(err)
}

func (h *runtimeContractHost) History(
	ctx context.Context,
	since protocol.Cursor,
	limit int,
) ([]protocol.Event, error) {
	page, err := h.runtime.History(ctx, app.SessionHistoryQuery{
		SessionID: h.sessionID, Since: since, Limit: limit,
	})
	return page.Events, contractError(err)
}

func (h *runtimeContractHost) ReadState(
	ctx context.Context,
) (contract.ReadState, error) {
	var result contract.ReadState
	threads, err := h.repositories.Threads.List(ctx, threadstate.Filter{
		SessionID: h.sessionID, WorkspaceRoot: h.workspace,
	}, 10)
	if err != nil {
		return result, err
	}
	result.Threads = make([]runtimeview.Thread, 0, len(threads))
	for _, value := range threads {
		result.Threads = append(result.Threads, runtimeview.ThreadFrom(value, nil))
	}
	current, err := h.repositories.Threads.Get(ctx, h.threadID)
	if err != nil {
		return result, err
	}
	turns, err := h.repositories.Threads.ListTurns(ctx, h.threadID)
	if err != nil {
		return result, err
	}
	result.Thread = runtimeview.ThreadFrom(current, turns)
	if h.agents != nil {
		for _, value := range h.agents.List(subagent.ListFilter{
			SessionID: h.sessionID, IncludeClosed: true,
		}) {
			result.Agents = append(result.Agents, runtimeview.AgentFrom(value))
		}
	}
	usage, err := h.repositories.Usage.QueryAggregates(ctx, usagestate.Query{
		SessionID: h.sessionID, WorkspaceRoot: h.workspace, Limit: 10,
	})
	if err != nil {
		return result, err
	}
	for _, value := range usage {
		result.Usage = append(result.Usage, runtimeview.UsageFrom(value))
	}
	result.Rollup = runtimeview.UsageRollupFrom(usagestate.Fold(
		usagestate.Scope{SessionID: h.sessionID},
		usage,
	))
	return result, nil
}

func (h *runtimeContractHost) SessionProfile(
	ctx context.Context,
) (protocol.SessionProfileSnapshot, error) {
	result, err := h.runtime.SessionProfile(ctx, h.sessionID)
	return result, contractError(err)
}

func (h *runtimeContractHost) SessionToolCatalog(
	ctx context.Context,
) (protocol.SessionToolCatalog, error) {
	result, err := h.runtime.SessionToolCatalog(ctx, h.sessionID)
	return result, contractError(err)
}

func (h *runtimeContractHost) ListSessions(
	ctx context.Context,
	query protocol.SessionListQuery,
) (protocol.SessionList, error) {
	query.WorkspaceRoot = h.workspace
	result, err := h.runtime.ListSessions(ctx, query)
	return result, contractError(err)
}

func (h *runtimeContractHost) UpdateSessionLifecycle(
	ctx context.Context,
	expectedRevision uint64,
	patch protocol.SessionLifecyclePatch,
) (protocol.SessionLifecycleUpdate, error) {
	result, err := h.runtime.UpdateSessionLifecycle(
		ctx,
		h.sessionID,
		expectedRevision,
		patch,
	)
	return result, contractError(err)
}

func (h *runtimeContractHost) DeleteSession(
	ctx context.Context,
	expectedRevision uint64,
) (protocol.SessionDeleteResult, error) {
	result, err := h.runtime.DeleteSession(ctx, h.sessionID, expectedRevision)
	return result, contractError(err)
}

func (h *runtimeContractHost) ListCheckpoints(
	ctx context.Context,
	limit int,
) (protocol.CheckpointList, error) {
	result, err := h.runtime.Checkpoints(ctx, h.sessionID, limit)
	return result, contractError(err)
}

func (h *runtimeContractHost) RestoreCheckpoint(
	ctx context.Context,
	checkpointID string,
) (protocol.CheckpointRestoreResult, error) {
	result, err := h.runtime.RestoreCheckpoint(ctx, h.sessionID, checkpointID)
	return result, contractError(err)
}

func (h *runtimeContractHost) ForkCheckpoint(
	ctx context.Context,
	checkpointID, title string,
) (protocol.CheckpointForkResult, error) {
	result, err := h.runtime.ForkCheckpoint(
		ctx,
		h.sessionID,
		checkpointID,
		title,
	)
	if err == nil {
		h.threadID = result.ThreadID
	}
	return result, contractError(err)
}

func (h *runtimeContractHost) UpdateSessionProfile(
	ctx context.Context,
	expectedRevision uint64,
	patch protocol.SessionProfilePatch,
) (protocol.SessionProfileUpdateResult, error) {
	result, err := h.runtime.UpdateSessionProfile(
		ctx,
		h.sessionID,
		h.threadID,
		expectedRevision,
		patch,
	)
	return result, contractError(err)
}

func contractError(err error) error {
	if err == nil {
		return nil
	}
	return &contract.Refusal{
		Code: protocol.CodeOf(err), Message: err.Error(),
		Retryable: protocol.IsRetryable(err),
	}
}
