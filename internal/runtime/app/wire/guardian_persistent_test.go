package wire

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/guardian"
)

// This fixture crosses the production HTTP provider, Wire, ThreadManager,
// durable session, Guard and real sandbox boundaries. Only model output is
// simulated; no approval or execution decision is injected into the Runtime.
func TestGuardianProductionPersistentSessionAndCheckpointFork(t *testing.T) {
	t.Setenv("QCODE_DISABLE_APPROVAL_AUTO_REVIEW", "")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, "generated"), 0700); err != nil {
		t.Fatal(err)
	}
	var actCalls atomic.Int32
	reviews := make(chan guardian.ReviewCandidate, 4)
	sourceTexts := make(chan []agentcontext.UserAuthorizationText, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string            `json:"model"`
			Tools    []json.RawMessage `json:"tools"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", 400)
			return
		}
		var delta map[string]any
		finish := "stop"
		if request.Model == "guardian-judge" {
			var input struct {
				Sources   []agentcontext.UserAuthorizationText `json:"user_sources"`
				Operation guardian.ReviewCandidate             `json:"untrusted_operation"`
			}
			for _, message := range request.Messages {
				if message.Role != "user" {
					continue
				}
				var content string
				if err := json.Unmarshal(message.Content, &content); err != nil {
					t.Error(err)
					continue
				}
				if err := json.Unmarshal([]byte(content), &input); err != nil {
					t.Error(err)
				}
			}
			if len(input.Sources) == 0 {
				t.Error("production judge lost user sources")
				http.Error(w, "no sources", 400)
				return
			}
			select {
			case reviews <- input.Operation:
			case <-r.Context().Done():
				return
			}
			select {
			case sourceTexts <- input.Sources:
			case <-r.Context().Done():
				return
			}
			assessment := guardian.Assessment{RiskLevel: guardian.RiskLow, Authorization: guardian.AuthSupported,
				AuthorizationSourceIDs: []string{input.Sources[0].Source.ID}, Recommendation: guardian.RecommendAllow, Rationale: "bounded requested write"}
			for _, source := range input.Sources {
				if strings.Contains(source.Text, "禁止修改") {
					assessment.Authorization = guardian.AuthConflicting
					assessment.AuthorizationSourceIDs = []string{}
					assessment.Recommendation = guardian.RecommendPrompt
				}
			}
			body, _ := json.Marshal(assessment)
			delta = map[string]any{"content": string(body)}
		} else if len(request.Tools) == 0 {
			// Auxiliary title/summary samples must not consume act steps.
			delta = map[string]any{"content": "Guardian test"}
		} else {
			call := actCalls.Add(1)
			name, arguments := "exec_command", `{"command":"printf reviewed > generated/out.txt","write_paths":["generated/out.txt"],"yield_time_ms":30000}`
			switch call {
			case 1, 5:
				name, arguments = "submit_plan", `{"steps":[{"id":"write","title":"Create the requested output","status":"pending"}]}`
			case 3:
				name, arguments = "update_plan", `{"steps":[{"id":"write","title":"Create the requested output","status":"done"}]}`
			case 4:
				name, arguments = "turn_complete", `{"status":"complete","summary":"done","pending_actions":[]}`
			}
			delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("call-%d", call), "type": "function", "function": map[string]any{"name": name, "arguments": arguments}}}}
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	t.Cleanup(server.Close)
	dataDir := t.TempDir()
	store, err := state.Open(ctx, state.Options{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("[security.guardian]\nenabled = true\ntimeout = \"5s\"\n[route.judge]\nprovider = \"judge\"\nmodel = \"guardian-judge\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	providerID, modelID, wireProtocol, tools := "fixture", "guardian-act", string(model.ProtocolOpenAIChat), true
	s, err := NewExec(ctx, ExecOptions{ConfigPath: configPath, BaseURL: server.URL, Permission: "auto", PersistentStore: store,
		Skills:           SkillOptions{UserHome: t.TempDir(), DataDir: dataDir},
		ModelMetadata:    ModelMetadataOptions{Descriptor: fixtureModel(modelID)},
		ExtraConnections: []ExtraConnectionSpec{{ProviderID: "judge", BaseURL: server.URL, Protocol: model.ProtocolOpenAIChat, Model: fixtureModel("guardian-judge")}},
		ConfigOverrides:  config.Overrides{Workspace: &workspace, Provider: &providerID, Model: &modelID, Protocol: &wireProtocol, Tools: &tools}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	binding, err := s.Runtime.CreateSession(ctx, app.CreateSessionRequest{SessionID: "guardian-durable", WorkspaceRoot: workspace, Title: "Guardian test"})
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.Runtime.Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	submit := func(payload protocol.OperationPayload) {
		t.Helper()
		op, err := protocol.NewOperation(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Runtime.Submit(ctx, op); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(kind protocol.EventKind, allowApproval bool) protocol.Event {
		t.Helper()
		for {
			select {
			case event, ok := <-events:
				if !ok {
					t.Fatal("event stream closed")
				}
				if event.Kind == protocol.EventTurnFailed {
					t.Fatalf("turn failed: %+v", event.Data)
				}
				if event.Kind == protocol.EventApprovalRequired && !allowApproval {
					t.Fatalf("unexpected manual approval: %+v", event.Data)
				}
				if event.Kind == kind {
					return event
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	submit(&protocol.StartTurnPayload{ThreadID: binding.ThreadID, TurnID: "guardian-turn", ItemID: "prompt", Prompt: "创建 generated/out.txt，写入 reviewed"})
	wait(protocol.EventTurnCompleted, false)
	wait(protocol.EventCheckpointCreated, false)
	var candidate guardian.ReviewCandidate
	select {
	case candidate = <-reviews:
	default:
		t.Fatal("real session never reached the judge")
	}
	<-sourceTexts
	if candidate.Identity.SessionID != binding.SessionID || candidate.Identity.ThreadID != string(binding.ThreadID) {
		t.Fatalf("wrong candidate identity: %+v", candidate.Identity)
	}
	if body, err := os.ReadFile(filepath.Join(workspace, "generated/out.txt")); err != nil || string(body) != "reviewed" {
		t.Fatalf("sandbox output=%q err=%v", body, err)
	}
	fence, err := store.LastSequence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	persisted, more, err := store.ReplayThrough(ctx, 0, fence, 0)
	if err != nil || more {
		t.Fatal("audit replay incomplete", err)
	}
	decided := false
	for _, event := range persisted {
		if data, ok := event.Data.(*protocol.GuardianReviewData); ok && data.Phase == "decided" && data.Decision != nil && data.Decision.Authority == "guardian" && data.Candidate != nil {
			decided = data.ReviewID == candidate.Identity.ReviewID && data.CallID == candidate.Identity.CallID && event.ThreadID == binding.ThreadID && data.Candidate.CandidateDigest == candidate.Digest()
		}
	}
	if !decided {
		t.Fatal("automatic decision is not linked to the durable candidate")
	}
	checkpoints, err := s.Runtime.Checkpoints(ctx, binding.SessionID, 10)
	if err != nil || len(checkpoints.Checkpoints) == 0 {
		t.Fatal("terminal checkpoint missing", err)
	}
	fork, err := s.Runtime.ForkCheckpoint(ctx, binding.SessionID, checkpoints.Checkpoints[0].ID, "Guardian fork")
	if err != nil {
		t.Fatal(err)
	}
	submit(&protocol.StartTurnPayload{ThreadID: fork.ThreadID, TurnID: "fork-turn", ItemID: "fork-prompt", Prompt: "仅审查，禁止修改任何文件"})
	wait(protocol.EventApprovalRequired, true)
	select {
	case <-reviews:
	default:
		t.Fatal("fork never reached the judge")
	}
	texts := <-sourceTexts
	foundParent, foundFork := false, false
	for _, source := range texts {
		foundParent = foundParent || strings.Contains(source.Text, "创建 generated/out.txt")
		foundFork = foundFork || strings.Contains(source.Text, "禁止修改")
	}
	if !foundParent || !foundFork {
		t.Fatalf("fork lost user authority: %+v", texts)
	}
	source := guardianSource(store)
	scope := agentcontext.AuthorizationScope{WorkspaceID: candidate.Identity.WorkspaceID, SessionID: binding.SessionID, ThreadID: string(fork.ThreadID)}
	before, err := source.Capture(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	submit(&protocol.SteerTurnPayload{ThreadID: fork.ThreadID, TurnID: "fork-turn", ItemID: "steer", Prompt: "停止修改，撤销此前写入授权"})
	wait(protocol.EventTurnSteered, true)
	started := false
	if err := source.WithCurrent(ctx, scope, before.Snapshot(), func() error { started = true; return nil }); err == nil || started {
		t.Fatal("fork steering did not invalidate prepared authority")
	}
	submit(&protocol.CancelTurnPayload{ThreadID: fork.ThreadID, TurnID: "fork-turn", ItemID: "cancel"})
	wait(protocol.EventTurnCanceled, true)
}
