package intergration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/fwtllh-png/QCode/internal/config"
	webhost "github.com/fwtllh-png/QCode/internal/host"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	threadstate "github.com/fwtllh-png/QCode/internal/persist/thread"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	apppersistence "github.com/fwtllh-png/QCode/internal/runtime/app/persistence"
	"github.com/fwtllh-png/QCode/internal/runtime/app/wire"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestWebHostMeetsTheRuntimeContract(t *testing.T) {
	runContract(t, newWebContractHost)
}

type webContractHost struct {
	origin      string
	token       string
	client      *http.Client
	sessionID   string
	threadID    protocol.ThreadID
	workspaceID string
}

func newWebContractHost(t *testing.T, setup hostSetup) hostDriver {
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
		MCPConfigPath:       setup.writeMCPConfig(t, store.Root()),
		ConfigOverrides:     overrides, PersistentStore: store,
		WorkspaceIdentity: identity,
	})
	if err != nil {
		_ = store.CloseAll(context.Background())
		t.Fatal(err)
	}
	repositories, err := apppersistence.NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hostPort := listener.Addr().String()
	server, err := webhost.New(webhost.Options{
		Assets: fstest.MapFS{
			"index.html": &fstest.MapFile{Data: []byte("<main>contract</main>")},
		},
		ExpectedHost: hostPort,
		Origin:       "http://" + hostPort,
		Token:        "contract-token",
		Build:        "contract",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Activate(webhost.Dependencies{
		Runtime: session.Runtime, WorkspaceRoot: workspace,
		WorkspaceIdentity: identity, DefaultProfile: session.DefaultProfile(),
		ProviderCatalog: session.ProviderCatalog(), ModelCatalog: session.ModelCatalog(),
		MCPHealth: session.MCPHealth,
		Usage:     repositories.Usage, Agents: session.Subagents(),
		SessionWorkspaces: session.SessionWorkspaces(),
		Workspace:         session.WorkspaceQuery(),
		RepositoryIndex:   session.RepositoryIndex(),
	}); err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: server.Handler()}
	served := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		served <- err
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
		<-served
		if err := session.Close(ctx); err != nil {
			t.Errorf("close Web contract session: %v", err)
		}
	})
	host := &webContractHost{
		origin: "http://" + hostPort, token: "contract-token",
		client:      &http.Client{Timeout: 20 * time.Second},
		workspaceID: identity.RootID,
	}
	var binding app.SessionBinding
	if err := host.call(
		t.Context(),
		"session/create",
		map[string]any{
			"session_id": "session_contract",
			"title":      "contract", "isolation": "shared",
			"provider": session.DefaultProfile().Provider,
			"model":    session.DefaultProfile().Model,
		},
		&binding,
		"runtime-contract",
	); err != nil {
		t.Fatal(err)
	}
	host.sessionID, host.threadID = binding.SessionID, binding.ThreadID
	return host
}

func (h *webContractHost) Transport() string { return "web" }

func (h *webContractHost) StartTurn(
	ctx context.Context,
	prompt string,
) (contractReceipt, error) {
	return h.submit(ctx, protocol.OperationStartTurn, map[string]any{"prompt": prompt})
}

func (h *webContractHost) StartTurnWithContext(
	ctx context.Context,
	prompt string,
	workspaceIdentity *protocol.WorkspaceIdentity,
	editorContext []protocol.EditorContextReference,
) (contractReceipt, error) {
	return h.submit(ctx, protocol.OperationStartTurn, map[string]any{
		"prompt": prompt, "workspace_identity": workspaceIdentity,
		"context": editorContext,
	})
}

func (h *webContractHost) Cancel(
	ctx context.Context,
	turn contractReceipt,
	reason string,
) (contractReceipt, error) {
	return h.submit(ctx, protocol.OperationCancelTurn, map[string]any{
		"thread_id": turn.ThreadID, "turn_id": turn.TurnID, "reason": reason,
	})
}

func (h *webContractHost) Decide(
	ctx context.Context,
	turn contractReceipt,
	requestID string,
	decision protocol.ApprovalDecision,
	planID string,
) (contractReceipt, error) {
	return h.submit(ctx, protocol.OperationApprovalDecision, map[string]any{
		"thread_id": turn.ThreadID, "turn_id": turn.TurnID,
		"request_id": requestID, "decision": decision,
		"scope": protocol.ApprovalScopeOnce, "plan_id": planID,
	})
}

func (h *webContractHost) ReplyInput(
	ctx context.Context,
	turn contractReceipt,
	requestID, answer string,
	values map[string]string,
) (contractReceipt, error) {
	return h.submit(ctx, protocol.OperationInputReply, map[string]any{
		"thread_id": turn.ThreadID, "turn_id": turn.TurnID,
		"request_id": requestID, "answer": answer, "values": values,
	})
}

func (h *webContractHost) RecoverTurn(
	ctx context.Context,
	sourceTurnID protocol.TurnID,
	action protocol.TurnRecoveryAction,
	prompt string,
) (contractReceipt, error) {
	var result app.OperationReceipt
	key := "recover-" + string(sourceTurnID)
	err := h.call(ctx, "turn/recover", protocol.TurnRecoveryRequest{
		Version: protocol.WorkflowIntentVersion, SessionID: h.sessionID,
		SourceTurnID: sourceTurnID, Action: action, Prompt: prompt,
		IdempotencyKey: key,
	}, &result, key)
	return contractReceipt{
		ThreadID: result.ThreadID, TurnID: result.TurnID, ItemID: result.ItemID,
	}, err
}

func (h *webContractHost) submit(
	ctx context.Context,
	kind protocol.OperationKind,
	payload any,
) (contractReceipt, error) {
	var result app.OperationReceipt
	key := string(kind) + "-" + time.Now().UTC().Format(time.RFC3339Nano)
	err := h.call(ctx, "operation/submit", map[string]any{
		"session_id": h.sessionID, "kind": kind,
		"payload": payload, "idempotency_key": key,
	}, &result, key)
	return contractReceipt{
		ThreadID: result.ThreadID, TurnID: result.TurnID, ItemID: result.ItemID,
	}, err
}

func (h *webContractHost) Live(
	ctx context.Context,
	since protocol.Cursor,
) (<-chan protocol.Event, error) {
	socketURL := "ws" + h.origin[len("http"):] + "/api/v1/events"
	connection, _, err := websocket.Dial(ctx, socketURL, nil)
	if err != nil {
		return nil, err
	}
	if err := wsjson.Write(ctx, connection, map[string]any{
		"type": "authenticate", "token": h.token,
		"workspace_id": h.workspaceID, "cursor": since,
	}); err != nil {
		_ = connection.CloseNow()
		return nil, err
	}
	var hello struct {
		Type string `json:"type"`
	}
	if err := wsjson.Read(ctx, connection, &hello); err != nil {
		_ = connection.CloseNow()
		return nil, err
	}
	if hello.Type != "hello" {
		_ = connection.CloseNow()
		return nil, errors.New("Web event stream did not send hello")
	}
	output := make(chan protocol.Event, 256)
	go func() {
		defer close(output)
		defer connection.CloseNow()
		for {
			var frame struct {
				Type  string         `json:"type"`
				Event protocol.Event `json:"event"`
			}
			if err := wsjson.Read(ctx, connection, &frame); err != nil {
				return
			}
			if frame.Type != "event" {
				continue
			}
			select {
			case output <- frame.Event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return output, nil
}

func (h *webContractHost) History(
	ctx context.Context,
	since protocol.Cursor,
	limit int,
) ([]protocol.Event, error) {
	var page app.SessionHistoryPage
	err := h.call(ctx, "session/history", map[string]any{
		"session_id": h.sessionID, "since_sequence": since, "limit": limit,
	}, &page, "")
	return page.Events, err
}

func (h *webContractHost) ReadState(
	ctx context.Context,
) (contractReadState, error) {
	var result contractReadState
	var summary protocol.SessionSummary
	if err := h.call(ctx, "session/status", map[string]any{
		"session_id": h.sessionID,
	}, &summary, ""); err != nil {
		return result, err
	}
	var snapshot app.SessionPresentationSnapshot
	if err := h.call(ctx, "session/snapshot", map[string]any{
		"session_id": h.sessionID,
	}, &snapshot, ""); err != nil {
		return result, err
	}
	thread := contractThread{
		ID: summary.ThreadID, SessionID: summary.SessionID,
		LatestSequence: summary.LatestSequence,
		CreatedAt:      summary.CreatedAt, UpdatedAt: summary.UpdatedAt,
	}
	for _, event := range snapshot.Events {
		if event.Kind != protocol.EventTurnCompleted &&
			event.Kind != protocol.EventTurnFailed &&
			event.Kind != protocol.EventTurnCanceled {
			continue
		}
		status := "completed"
		if event.Kind == protocol.EventTurnFailed {
			status = "failed"
		} else if event.Kind == protocol.EventTurnCanceled {
			status = "canceled"
		}
		thread.Turns = append(thread.Turns, contractTurn{
			ID: event.TurnID, ThreadID: event.ThreadID,
			Status: threadStatus(status), CompletedAt: &event.CreatedAt,
		})
	}
	result.Threads = []contractThread{thread}
	result.Thread = thread
	var agents struct {
		Agents []webhost.Agent `json:"agents"`
	}
	if err := h.call(ctx, "agent/list", map[string]any{
		"session_id": h.sessionID, "limit": 10,
	}, &agents, ""); err != nil {
		return result, err
	}
	result.Agents = agents.Agents
	var usage struct {
		Usage  []webhost.Usage     `json:"usage"`
		Rollup webhost.UsageRollup `json:"rollup"`
	}
	if err := h.call(ctx, "usage/query", map[string]any{
		"session_id": h.sessionID, "thread_id": h.threadID, "limit": 10,
	}, &usage, ""); err != nil {
		return result, err
	}
	result.Usage, result.Rollup = usage.Usage, usage.Rollup
	return result, nil
}

func (h *webContractHost) SessionProfile(
	ctx context.Context,
) (protocol.SessionProfileSnapshot, error) {
	var result protocol.SessionProfileSnapshot
	err := h.call(ctx, "profile/get", map[string]any{
		"session_id": h.sessionID,
	}, &result, "")
	return result, err
}

func (h *webContractHost) SessionToolCatalog(
	ctx context.Context,
) (protocol.SessionToolCatalog, error) {
	var result protocol.SessionToolCatalog
	err := h.call(ctx, "tool/catalog", map[string]any{
		"session_id": h.sessionID,
	}, &result, "")
	return result, err
}

func (h *webContractHost) ListSessions(
	ctx context.Context,
	query protocol.SessionListQuery,
) (protocol.SessionList, error) {
	var result protocol.SessionList
	err := h.call(ctx, "session/list", query, &result, "")
	return result, err
}

func (h *webContractHost) UpdateSessionLifecycle(
	ctx context.Context,
	expectedRevision uint64,
	patch protocol.SessionLifecyclePatch,
) (protocol.SessionLifecycleUpdate, error) {
	var result protocol.SessionLifecycleUpdate
	err := h.call(ctx, "session/update", map[string]any{
		"session_id": h.sessionID, "expected_revision": expectedRevision,
		"patch": patch,
	}, &result, "session-update")
	return result, err
}

func (h *webContractHost) DeleteSession(
	ctx context.Context,
	expectedRevision uint64,
) (protocol.SessionDeleteResult, error) {
	var result protocol.SessionDeleteResult
	err := h.call(ctx, "session/delete", map[string]any{
		"session_id": h.sessionID, "expected_revision": expectedRevision,
	}, &result, "session-delete")
	return result, err
}

func (h *webContractHost) ListCheckpoints(
	ctx context.Context,
	limit int,
) (protocol.CheckpointList, error) {
	var result protocol.CheckpointList
	err := h.call(ctx, "checkpoint/list", map[string]any{
		"session_id": h.sessionID, "limit": limit,
	}, &result, "")
	return result, err
}

func (h *webContractHost) RestoreCheckpoint(
	ctx context.Context,
	checkpointID string,
) (protocol.CheckpointRestoreResult, error) {
	var result protocol.CheckpointRestoreResult
	err := h.call(ctx, "checkpoint/restore", map[string]any{
		"session_id": h.sessionID, "checkpoint_id": checkpointID,
	}, &result, "checkpoint-restore")
	return result, err
}

func (h *webContractHost) ForkCheckpoint(
	ctx context.Context,
	checkpointID, title string,
) (protocol.CheckpointForkResult, error) {
	var result protocol.CheckpointForkResult
	err := h.call(ctx, "checkpoint/fork", map[string]any{
		"session_id": h.sessionID, "checkpoint_id": checkpointID,
		"title": title,
	}, &result, "checkpoint-fork")
	if err == nil {
		h.threadID = result.ThreadID
	}
	return result, err
}

func (h *webContractHost) UpdateSessionProfile(
	ctx context.Context,
	expectedRevision uint64,
	patch protocol.SessionProfilePatch,
) (protocol.SessionProfileUpdateResult, error) {
	var result protocol.SessionProfileUpdateResult
	err := h.call(ctx, "profile/update", map[string]any{
		"session_id": h.sessionID, "thread_id": h.threadID,
		"expected_revision": expectedRevision, "patch": patch,
	}, &result, "profile-update")
	return result, err
}

func (h *webContractHost) call(
	ctx context.Context,
	route string,
	request any,
	result any,
	idempotencyKey string,
) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		h.origin+"/api/v1/"+route,
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+h.token)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("X-QCode-Request-ID", "contract")
	httpRequest.Header.Set("X-QCode-Workspace-ID", h.workspaceID)
	if idempotencyKey != "" {
		httpRequest.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := h.client.Do(httpRequest)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var envelope struct {
		Result  json.RawMessage   `json:"result"`
		Problem *protocol.Problem `json:"problem"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 3<<20)).
		Decode(&envelope); err != nil {
		return err
	}
	if envelope.Problem != nil {
		return &contractRefusal{
			Code: envelope.Problem.Code, Message: envelope.Problem.Message,
			Retryable: envelope.Problem.Retryable,
		}
	}
	if response.StatusCode != http.StatusOK {
		return errors.New(response.Status)
	}
	return json.Unmarshal(envelope.Result, result)
}

func threadStatus(value string) threadstate.TurnStatus {
	return threadstate.TurnStatus(value)
}

func TestWebWithdrawsLatestRunningTurn(t *testing.T) {
	fixture, err := filepath.Abs("../../../testdata/providers/slow")
	if err != nil {
		t.Fatal(err)
	}
	host := newWebContractHost(t, hostSetup{Fixture: fixture}).(*webContractHost)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	live, err := host.Live(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := host.StartTurn(ctx, "mistaken request")
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-live:
			if event.TurnID == turn.TurnID && event.Kind == protocol.EventTurnStarted {
				goto started
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
started:
	var result struct{}
	request := protocol.TurnWithdrawRequest{SessionID: host.sessionID, TurnID: turn.TurnID}
	if err := host.call(ctx, "turn/withdraw", request, &result, "withdraw"); err != nil {
		t.Fatal(err)
	}
	if err := host.call(ctx, "turn/withdraw", request, &result, "withdraw-again"); err != nil {
		t.Fatal(err)
	}
	var summary protocol.SessionSummary
	if err := host.call(ctx, "session/status", map[string]string{"session_id": host.sessionID}, &summary, ""); err != nil {
		t.Fatal(err)
	}
	if !summary.LatestTurnWithdrawn || summary.Status != protocol.SessionStatusIdle {
		t.Fatalf("summary: %+v", summary)
	}
	if _, err := host.RecoverTurn(ctx, turn.TurnID, protocol.TurnRecoveryContinue, ""); err == nil {
		t.Fatal("withdrawn Turn accepted Continue")
	}
	next, err := host.StartTurn(ctx, "a new request after withdrawal")
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-live:
			if event.TurnID != next.TurnID {
				continue
			}
			if event.Kind == protocol.EventTurnStarted {
				return
			}
			if event.Kind == protocol.EventOperationRejected || event.Kind == protocol.EventTurnFailed {
				t.Fatalf("new Turn rejected after withdrawal: %+v", event.Data)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestWebImmediateStopPreservesBaselineAndRetry(t *testing.T) {
	fixture, err := filepath.Abs("../../../testdata/providers/slow")
	if err != nil {
		t.Fatal(err)
	}
	host := newWebContractHost(t, hostSetup{Fixture: fixture}).(*webContractHost)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	live, err := host.Live(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := host.StartTurn(ctx, "stop immediately")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Cancel(ctx, turn, protocol.CancelReasonUserInterrupted); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-live:
			if event.TurnID == turn.TurnID && protocol.IsTerminalEvent(event.Kind) {
				goto stopped
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
stopped:
	retried, err := host.RecoverTurn(ctx, turn.TurnID, protocol.TurnRecoveryRetry, "")
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-live:
			if event.TurnID != retried.TurnID {
				continue
			}
			if event.Kind == protocol.EventTurnStarted {
				return
			}
			if event.Kind == protocol.EventOperationRejected || event.Kind == protocol.EventTurnFailed {
				t.Fatalf("Retry rejected: %+v", event.Data)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
