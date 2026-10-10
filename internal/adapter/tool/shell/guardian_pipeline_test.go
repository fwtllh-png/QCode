package shell

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/security/guardian"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

type pipelineReviewer struct {
	mu          sync.Mutex
	auth        guardian.AuthorizationSnapshot
	reviews     atomic.Int32
	prompt      bool
	failure     bool
	reviewHook  func(guardian.ReviewCandidate)
	startHook   func()
	captureHook func() error
}

func (*pipelineReviewer) Report(context.Context, toolguard.GuardianFact) error { return nil }

func digestPipeline(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }
func newPipelineReviewer(root string) *pipelineReviewer {
	return &pipelineReviewer{auth: guardian.AuthorizationSnapshot{WorkspaceID: digestPipeline(root), SessionID: "session", ThreadID: "thread", Revision: digestPipeline("user"), Complete: true,
		Sources: []guardian.AuthorizationSource{{ID: "user", ThreadID: "thread", TurnID: "turn", Role: "user", Version: 1, Digest: digestPipeline("create generated output")}}}}
}
func (*pipelineReviewer) ID() string { return "review" }
func (*pipelineReviewer) Versions() guardian.ReviewVersions {
	return guardian.ReviewVersions{ConfigurationDigest: digestPipeline("config"), RouteDigest: digestPipeline("route"), PromptVersion: "prompt", SchemaVersion: "schema"}
}
func (*pipelineReviewer) MaxContentBytes() int64 { return 4096 }
func (r *pipelineReviewer) Capture(context.Context) (guardian.AuthorizationSnapshot, error) {
	if r.captureHook != nil {
		if err := r.captureHook(); err != nil {
			return guardian.AuthorizationSnapshot{}, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.auth, nil
}

func TestGuardianPipelineResamplesPolicyAfterSourceFailure(t *testing.T) {
	root, g, ctx, raw := guardianShellFixture(t)
	r := newPipelineReviewer(root)
	r.captureHook = func() error {
		g.Policy().SetPermission(policy.PermissionNever)
		return errors.New("source unavailable")
	}
	g.SetApprovalHandler(func(context.Context, toolguard.ApprovalRequest) error {
		t.Error("asked using policy from before source wait")
		return errors.New("unexpected approval")
	})
	result, err := g.Execute(pipelineContext(ctx, r), "call", "exec_command", raw)
	if err == nil && !result.IsError {
		t.Fatal("source failure retained old permission")
	}
	if r.reviews.Load() != 0 {
		t.Fatal("revoked command was sent to model")
	}
}
func (r *pipelineReviewer) revoke() {
	r.mu.Lock()
	r.auth.Revision = digestPipeline("revoked")
	r.mu.Unlock()
}
func (r *pipelineReviewer) Review(_ context.Context, c guardian.ReviewCandidate, content map[string][]byte) (*guardian.ReviewEvidence, error) {
	r.reviews.Add(1)
	if r.reviewHook != nil {
		r.reviewHook(c)
	}
	if r.failure {
		return nil, errors.New("provider unavailable")
	}
	a := guardian.Assessment{RiskLevel: guardian.RiskLow, Authorization: guardian.AuthSupported, AuthorizationSourceIDs: []string{"user"}, Recommendation: guardian.RecommendAllow, Rationale: "Requested bounded output."}
	if r.prompt {
		a.Recommendation = guardian.RecommendPrompt
	}
	body, _ := json.Marshal(a)
	return guardian.BindAssessment(c, body)
}
func (r *pipelineReviewer) WithAuthorization(_ context.Context, expected guardian.AuthorizationSnapshot, start func() error) error {
	if r.startHook != nil {
		r.startHook()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.auth.Digest() != expected.Digest() {
		return errors.New("user authorization revoked")
	}
	return start()
}
func pipelineContext(ctx context.Context, r *pipelineReviewer) context.Context {
	return toolguard.WithGuardian(ctx, func(context.Context) (toolguard.GuardianReviewer, error) { return r, nil })
}

func TestGuardianPipelinePolicyAndHumanRecovery(t *testing.T) {
	for _, kind := range []string{"allow", "reviewed_copy", "prompt", "failure", "source_changed", "readonly", "kill_switch", "bypass", "explicit_ask"} {
		t.Run(kind, func(t *testing.T) {
			root, g, ctx, raw := guardianShellFixture(t)
			r := newPipelineReviewer(root)
			var approvals int
			g.SetApprovalHandler(func(_ context.Context, req toolguard.ApprovalRequest) error {
				approvals++
				if kind != "explicit_ask" && req.GuardianReason == "" {
					t.Error("missing Guardian reason on real approval")
				}
				return g.Decide(toolguard.ApprovalDecision{RequestID: req.RequestID, Approved: true, Scope: policy.ApprovalOnce})
			})
			switch kind {
			case "prompt":
				r.prompt = true
			case "failure":
				r.failure = true
			case "source_changed":
				r.reviewHook = func(guardian.ReviewCandidate) { r.revoke() }
			case "readonly":
				r.reviewHook = func(guardian.ReviewCandidate) { g.Policy().SetPermission(policy.PermissionNever) }
			case "kill_switch":
				r.reviewHook = func(guardian.ReviewCandidate) { g.Policy().SetDisableAutoReview(true) }
			case "bypass":
				r.reviewHook = func(guardian.ReviewCandidate) { g.Policy().SetPermission(policy.PermissionBypass) }
			case "explicit_ask":
				if _, err := g.Policy().AppendManagedRule(policy.Rule{Tool: "exec_command", Action: policy.ActionAsk}); err != nil {
					t.Fatal(err)
				}
			case "reviewed_copy":
				r.reviewHook = func(guardian.ReviewCandidate) {
					if err := os.WriteFile(filepath.Join(root, "build.sh"), []byte("#!/bin/sh\nprintf unreviewed > generated/out.txt\n"), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			result, err := g.Execute(pipelineContext(ctx, r), "call", "exec_command", raw)
			if kind == "readonly" {
				if err == nil {
					t.Fatalf("revoked invocation executed: %+v", result)
				}
				if _, err := os.Stat(filepath.Join(root, "generated/out.txt")); !os.IsNotExist(err) {
					t.Fatal("revoked process wrote output")
				}
			} else if err != nil || result.IsError {
				t.Fatalf("execution: %+v %v", result, err)
			}
			wantApprovals := 1
			if kind == "allow" || kind == "reviewed_copy" || kind == "readonly" || kind == "bypass" {
				wantApprovals = 0
			}
			if approvals != wantApprovals {
				t.Fatalf("approvals=%d want=%d", approvals, wantApprovals)
			}
			wantReviews := int32(1)
			if kind == "explicit_ask" {
				wantReviews = 0
			}
			if r.reviews.Load() != wantReviews {
				t.Fatalf("reviews=%d want=%d", r.reviews.Load(), wantReviews)
			}
			if kind == "allow" || kind == "reviewed_copy" {
				body, err := os.ReadFile(filepath.Join(root, "generated/out.txt"))
				if err != nil || string(body) != "reviewed\n" {
					t.Fatalf("unreviewed output: %q %v", body, err)
				}
			}
		})
	}
}

func TestGuardianPipelineRejectsChangesAtPhysicalStart(t *testing.T) {
	for _, kind := range []string{"permission", "policy_replaced", "source", "copy", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			root, g, ctx, raw := guardianShellFixture(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			r := newPipelineReviewer(root)
			var copyRoot string
			r.reviewHook = func(c guardian.ReviewCandidate) { copyRoot = c.Execution.Root }
			r.startHook = func() {
				switch kind {
				case "permission":
					g.Policy().SetPermission(policy.PermissionNever)
				case "policy_replaced":
					g.SwapPolicy(g.Policy().CloneSampling())
				case "source":
					r.revoke()
				case "copy":
					if err := os.WriteFile(filepath.Join(copyRoot, "build.sh"), []byte("#!/bin/sh\nprintf corrupt > generated/out.txt\n"), 0700); err != nil {
						t.Fatal(err)
					}
				case "cancel":
					cancel()
				}
			}
			result, err := g.Execute(pipelineContext(ctx, r), "call", "exec_command", raw)
			if err == nil && !result.IsError {
				t.Fatalf("changed invocation started: %+v", result)
			}
			if r.reviews.Load() != 1 {
				t.Fatalf("reviews=%d", r.reviews.Load())
			}
			if _, err := os.Stat(filepath.Join(root, "generated/out.txt")); !os.IsNotExist(err) {
				t.Fatal("revoked process wrote output")
			}
			if _, err := os.Stat(copyRoot); !os.IsNotExist(err) {
				t.Fatal("review copy leaked")
			}
		})
	}
}

func TestGuardianPipelineDeduplicatesConcurrentCall(t *testing.T) {
	root, g, ctx, raw := guardianShellFixture(t)
	r := newPipelineReviewer(root)
	entered, release := make(chan struct{}), make(chan struct{})
	r.reviewHook = func(guardian.ReviewCandidate) { close(entered); <-release }
	ctx = pipelineContext(ctx, r)
	done := make(chan error, 1)
	go func() {
		result, err := g.Execute(ctx, "call", "exec_command", raw)
		if err == nil && result.IsError {
			err = errors.New(result.Content)
		}
		done <- err
	}()
	<-entered
	_, duplicateErr := g.Execute(ctx, "call", "exec_command", raw)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if duplicateErr == nil || r.reviews.Load() != 1 {
		t.Fatalf("duplicate executed: err=%v reviews=%d", duplicateErr, r.reviews.Load())
	}
}

func TestGuardianPipelineRevocationWhileQueued(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(fmt.Sprint(manual), func(t *testing.T) {
			root, g, ctx, raw := guardianShellFixture(t)
			r := newPipelineReviewer(root)
			r.prompt = manual
			ctx = tool.WithExecutionAdmission(pipelineContext(ctx, r), func(context.Context, tool.ParallelPolicy) (func(), error) {
				g.Policy().SetPermission(policy.PermissionNever)
				return func() {}, nil
			})
			result, err := g.Execute(ctx, "call", "exec_command", raw)
			if err == nil && !result.IsError {
				t.Fatalf("queued revoked invocation executed: %+v", result)
			}
			if _, err := os.Stat(filepath.Join(root, "generated/out.txt")); !os.IsNotExist(err) {
				t.Fatal("revoked process wrote output")
			}
		})
	}
}

func TestGuardianPipelineReusesHumanGrantBeforeModel(t *testing.T) {
	root, g, ctx, raw := guardianShellFixture(t)
	var approvals int
	g.SetApprovalHandler(func(_ context.Context, req toolguard.ApprovalRequest) error {
		approvals++
		return g.Decide(toolguard.ApprovalDecision{RequestID: req.RequestID, Approved: true, Scope: policy.ApprovalSession})
	})
	if result, err := g.Execute(ctx, "call", "exec_command", raw); err != nil || result.IsError {
		t.Fatalf("human grant: %+v %v", result, err)
	}
	r := newPipelineReviewer(root)
	if result, err := g.Execute(pipelineContext(ctx, r), "next-call", "exec_command", raw); err != nil || result.IsError {
		t.Fatalf("reuse: %+v %v", result, err)
	}
	if approvals != 1 || r.reviews.Load() != 0 {
		t.Fatalf("approvals=%d reviews=%d", approvals, r.reviews.Load())
	}
}

func TestGuardianPipelineHumanReplacementAndRejection(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint(replace), func(t *testing.T) {
			root, g, ctx, raw := guardianShellFixture(t)
			r := newPipelineReviewer(root)
			r.prompt = true
			var approvals int
			g.SetApprovalHandler(func(_ context.Context, req toolguard.ApprovalRequest) error {
				approvals++
				d := toolguard.ApprovalDecision{RequestID: req.RequestID, Approved: replace, Scope: policy.ApprovalOnce}
				if replace {
					if !req.ReplacementAllowed {
						t.Fatal("Guardian removed argument replacement")
					}
					d.ReplacementArguments = json.RawMessage(`{"command":"printf human > generated/out.txt","write_paths":["generated"],"yield_time_ms":30000}`)
				}
				return g.Decide(d)
			})
			result, err := g.Execute(pipelineContext(ctx, r), "call", "exec_command", raw)
			if replace {
				if err != nil || result.IsError {
					t.Fatalf("replacement failed: %+v %v", result, err)
				}
				body, err := os.ReadFile(filepath.Join(root, "generated/out.txt"))
				if err != nil || string(body) != "human" {
					t.Fatalf("replacement not executed: %q %v", body, err)
				}
			} else {
				if err == nil && !result.IsError {
					t.Fatal("human rejection was ignored")
				}
				if _, err := os.Stat(filepath.Join(root, "generated/out.txt")); !os.IsNotExist(err) {
					t.Fatal("rejected process wrote output")
				}
			}
			if approvals != 1 || r.reviews.Load() != 1 {
				t.Fatalf("repeated review or approval: approvals=%d reviews=%d", approvals, r.reviews.Load())
			}
		})
	}
}
