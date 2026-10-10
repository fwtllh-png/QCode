package shell

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/guardian"
)

type guardianRuntimeProvider struct {
	arguments  string
	mode       string
	act, judge atomic.Int32
}

func (p *guardianRuntimeProvider) Stream(ctx context.Context, request provider.ModelRequest) (provider.Stream, error) {
	if request.Purpose == model.PurposeJudge {
		p.judge.Add(1)
		if p.mode == "failure" {
			return nil, errors.New("provider failed with SECRET_SENTINEL")
		}
		if p.mode == "timeout" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		var input struct {
			Sources []agentcontext.UserAuthorizationText `json:"user_sources"`
		}
		if len(request.Messages) != 2 {
			return nil, errors.New("missing judge input")
		}
		if err := json.Unmarshal([]byte(request.Messages[1].Text()), &input); err != nil {
			return nil, err
		}
		if len(input.Sources) == 0 {
			return nil, errors.New("missing user source")
		}
		assessment := guardian.Assessment{RiskLevel: guardian.RiskLow, Authorization: guardian.AuthSupported,
			AuthorizationSourceIDs: []string{input.Sources[0].Source.ID}, Recommendation: guardian.RecommendAllow, Rationale: "SECRET_SENTINEL"}
		if p.mode == "approve" || p.mode == "deny" || p.mode == "cancel" {
			assessment.Recommendation = guardian.RecommendPrompt
		}
		body, _ := json.Marshal(assessment)
		return &providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventMessageStart}, {Type: provider.EventTextDelta, Text: string(body)},
			{Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 10, OutputTokens: 5}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn},
		}}, nil
	}
	if p.act.Add(1) == 1 {
		return &providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{Index: 0, ID: "call", Name: "exec_command", Arguments: p.arguments}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
		}}, nil
	}
	return &providerfixture.SliceStream{Events: []provider.StreamEvent{{Type: provider.EventTextDelta, Text: "done"}, {Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn}}}, nil
}

type guardianAuditStore struct {
	*state.Store
	failPhase string
}

func (s guardianAuditStore) Append(ctx context.Context, event protocol.Event) error {
	if d, ok := event.Data.(*protocol.GuardianReviewData); ok && d.Phase == s.failPhase {
		return errors.New("injected durable audit failure")
	}
	return s.Store.Append(ctx, event)
}

func guardianRuntimeRoute(t *testing.T) model.ReadyRoute {
	t.Helper()
	catalog, err := model.NewCatalog(model.Provider{ID: "test", Adapter: model.AdapterOpenAICompatible,
		Endpoint: "http://127.0.0.1:1", Protocol: model.ProtocolOpenAIChat, Provenance: model.ProvenanceFixture,
		Models: map[string]model.Model{"model": {ID: "model", CanonicalID: "model", WireID: "model",
			Limits: model.Limits{ContextTokens: 32768, MaxOutputTokens: 1024}, Capabilities: model.Capabilities{Streaming: true, ToolCalls: true},
			Pricing: model.Pricing{InputPerMillion: 1, OutputPerMillion: 1, Currency: "USD", Known: true, Provenance: model.ProvenanceFixture}, Provenance: model.ProvenanceFixture}}})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := model.NewResolver(catalog)
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(model.RouteRequest{ProviderID: "test", ModelID: "model", Provenance: model.ProvenanceFixture})
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func TestGuardianRuntimeDurableAuditAndApprovalE2E(t *testing.T) {
	type observation struct {
		Mode               string   `json:"mode"`
		Approvals          int      `json:"approval_events"`
		JudgeCalls         int32    `json:"judge_calls"`
		CancelToTerminalMS *float64 `json:"approval_cancel_to_terminal_ms"`
	}
	var observations []observation
	for _, mode := range []string{"allow", "approve", "deny", "cancel", "failure", "timeout", "audit_started", "audit_assessed", "audit_decided"} {
		t.Run(mode, func(t *testing.T) {
			root, guard, registry, _, raw := guardianShellRegistryFixture(t)
			dataDir := t.TempDir()
			store, err := state.Open(t.Context(), state.Options{DataDir: dataDir})
			if err != nil {
				t.Fatal(err)
			}
			backend := &guardianRuntimeProvider{arguments: string(raw), mode: mode}
			journal, err := workspacejournal.New(root, contentstore.NewMemory(contentstore.Options{}))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = journal.Close(context.Background()) })
			worker, err := agentengine.New(agentengine.Options{
				ProviderConfig: agentengine.ProviderConfig{Provider: backend, Route: guardianRuntimeRoute(t), MaxOutputTokens: 1024, SharedRateLimit: agentengine.NewSharedRateLimit(2)},
				ToolConfig:     agentengine.ToolConfig{Tools: registry, Guard: guard},
				SecurityConfig: agentengine.SecurityConfig{Security: guard.Policy(), Workspace: root, WorkspaceIdentity: digestPipeline(root), Journal: journal,
					Guardian: agentengine.GuardianConfig{Enabled: true, Timeout: time.Second}, GuardianSource: agentcontext.DurableGuardianSource{Store: store}},
				LifecycleConfig: agentengine.LifecycleConfig{SessionID: "session", TurnCoordinatorRuntime: turnkernel.NewEphemeralCoordinatorRuntime()},
			})
			if err != nil {
				t.Fatal(err)
			}
			failPhase := ""
			if strings.HasPrefix(mode, "audit_") {
				failPhase = strings.TrimPrefix(mode, "audit_")
			}
			runtime := app.NewRuntime(app.Options{Engine: app.AdaptEngine(worker), EventStore: guardianAuditStore{Store: store, failPhase: failPhase}})
			t.Cleanup(func() { _ = runtime.Close(context.Background()) })
			events, err := runtime.Events(t.Context(), 0)
			if err != nil {
				t.Fatal(err)
			}
			op, err := protocol.NewOperation(&protocol.StartTurnPayload{ThreadID: "thread", TurnID: "turn", ItemID: "prompt", Prompt: "Run build.sh to create generated/out.txt."})
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.Submit(t.Context(), op); err != nil {
				t.Fatal(err)
			}
			var observed []protocol.Event
			var approvals int
			var cancelRequested time.Time
			var cancelToTerminal *float64
			deadline := time.After(20 * time.Second)
		loop:
			for {
				select {
				case event := <-events:
					observed = append(observed, event)
					if request, ok := event.Data.(*protocol.ApprovalRequiredData); ok {
						approvals++
						if mode == "allow" {
							t.Fatal("automatic approval fell back", request.GuardianReasonCode, observed)
						}
						if _, err := os.Stat(filepath.Join(root, "generated/out.txt")); !os.IsNotExist(err) {
							t.Fatal("executed before human approval")
						}
						if request.GuardianReviewID == "" || request.GuardianReasonCode == "" || request.BindingDigest == "" {
							t.Fatal("approval lost review/binding", request)
						}
						decision := protocol.ApprovalApprove
						if mode == "deny" {
							decision = protocol.ApprovalDeny
						}
						if mode == "cancel" {
							decision = protocol.ApprovalCancel
						}
						approve, err := protocol.NewOperation(&protocol.ApprovalDecisionPayload{ThreadID: "thread", TurnID: "turn", ItemID: "approval", RequestID: request.RequestID, Decision: decision, Scope: protocol.ApprovalScopeOnce})
						if err != nil {
							t.Fatal(err)
						}
						if mode == "cancel" {
							cancelRequested = time.Now()
						}
						if err := runtime.Submit(t.Context(), approve); err != nil {
							t.Fatal(err)
						}
					}
					if protocol.IsTerminalEvent(event.Kind) {
						if !cancelRequested.IsZero() {
							ms := float64(time.Since(cancelRequested)) / float64(time.Millisecond)
							cancelToTerminal = &ms
						}
						break loop
					}
				case <-deadline:
					t.Fatal("Guardian runtime did not settle", observed)
				}
			}
			wantReviews := int32(1)
			if mode == "audit_started" {
				wantReviews = 0
			}
			if backend.judge.Load() != wantReviews {
				t.Fatalf("judge attempts=%d", backend.judge.Load())
			}
			if (mode == "allow" && approvals != 0) || (mode != "allow" && approvals != 1) {
				t.Fatalf("approvals=%d events=%v", approvals, observed)
			}
			_, outputErr := os.Stat(filepath.Join(root, "generated/out.txt"))
			if mode == "deny" || mode == "cancel" {
				if !os.IsNotExist(outputErr) {
					t.Fatal("declined operation executed")
				}
			} else if outputErr != nil {
				t.Fatal("approved operation did not execute", outputErr, observed)
			}
			if err := runtime.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			reopened, err := state.Open(t.Context(), state.Options{DataDir: dataDir})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close(context.Background())
			replay, err := reopened.Replay(t.Context(), 0)
			if err != nil {
				t.Fatal(err)
			}
			var reviewID string
			var receiptID string
			var allowed bool
			for _, event := range replay {
				if review, ok := event.Data.(*protocol.GuardianReviewData); ok {
					body, _ := json.Marshal(review)
					if strings.Contains(string(body), "SECRET_SENTINEL") || strings.Contains(string(body), "build.sh") {
						t.Fatal("raw text leaked into audit")
					}
					if reviewID != "" && reviewID != review.ReviewID {
						t.Fatal("review identity changed")
					}
					reviewID = review.ReviewID
					allowed = allowed || review.Decision != nil && review.Decision.Authority == "guardian"
				}
				if result, ok := event.Data.(*protocol.ToolResultData); ok && result.Execution != nil {
					receiptID = result.Execution.GuardianReviewID
				}
			}
			if mode == "allow" && (!allowed || reviewID == "" || receiptID != reviewID) {
				t.Fatalf("missing durable allow/receipt: allow=%v review=%s receipt=%s", allowed, reviewID, receiptID)
			}
			observations = append(observations, observation{mode, approvals, backend.judge.Load(), cancelToTerminal})
		})
	}
	if path := os.Getenv("QCODE_GUARDIAN_RUNTIME_REPORT"); path != "" {
		body, err := json.MarshalIndent(struct {
			Origin string        `json:"origin"`
			Cases  []observation `json:"cases"`
		}{"fixture_model_real_runtime_and_sandbox", observations}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(body, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
