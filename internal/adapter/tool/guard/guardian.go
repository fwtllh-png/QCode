package guard

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/common/startgate"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/guardian"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

// GuardianReviewer is a per-call Runtime service. Guard never calls providers
// or reads mutable Engine state. Capture must include current parent sources;
// WithAuthorization must fence their updates through physical process start.
type GuardianReviewer interface {
	Report(context.Context, GuardianFact) error
	ID() string
	Versions() guardian.ReviewVersions
	MaxContentBytes() int64
	Capture(context.Context) (guardian.AuthorizationSnapshot, error)
	Review(context.Context, guardian.ReviewCandidate, map[string][]byte) (*guardian.ReviewEvidence, error)
	WithAuthorization(context.Context, guardian.AuthorizationSnapshot, func() error) error
}

type GuardianFactory func(context.Context) (GuardianReviewer, error)
type guardianFactoryKey struct{}
type guardianAttemptKey struct{}

func WithGuardian(ctx context.Context, factory GuardianFactory) context.Context {
	return context.WithValue(ctx, guardianFactoryKey{}, factory)
}

type guardianAttempt struct {
	invocation                    Invocation
	reasonCode                    string
	policyRevision                uint64
	approvalID, recoveredReviewID string
	tried                         bool
	reviewer                      GuardianReviewer
	execution                     *ReviewExecution
	candidate                     guardian.ReviewCandidate
	evidence                      *guardian.ReviewEvidence
	reason                        string
}

func (a *guardianAttempt) close() {
	if a.execution != nil {
		_ = a.execution.Close()
		a.execution = nil
	}
	a.evidence = nil
}

func guardianAttemptFrom(ctx context.Context) *guardianAttempt {
	a, _ := ctx.Value(guardianAttemptKey{}).(*guardianAttempt)
	return a
}

func (g *Guard) enterGuardianCall(ctx context.Context, callID string) (func(), error) {
	if factory, _ := ctx.Value(guardianFactoryKey{}).(GuardianFactory); factory == nil {
		return func() {}, nil
	}
	identity := tool.InvocationIdentityFrom(ctx)
	identity.CallID = callID
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.guardianCalls[identity] {
		return nil, errors.New("Guardian call is already in progress")
	}
	if g.guardianCalls == nil {
		g.guardianCalls = make(map[tool.InvocationIdentity]bool)
	}
	g.guardianCalls[identity] = true
	return func() { g.mu.Lock(); delete(g.guardianCalls, identity); g.mu.Unlock() }, nil
}

func (g *Guard) reviewGuardian(ctx context.Context, p preparedExecution) bool {
	a := guardianAttemptFrom(ctx)
	factory, _ := ctx.Value(guardianFactoryKey{}).(GuardianFactory)
	if a == nil || a.tried || factory == nil || !p.decision.GuardianEligible {
		return false
	}
	a.tried = true
	a.invocation = p.invocation
	a.reasonCode = "review_unavailable"
	a.reason = "Guardian review unavailable; human approval is required"
	reviewer, err := factory(ctx)
	if err != nil || reviewer == nil {
		return true
	}
	a.reviewer = reviewer
	authorization, err := reviewer.Capture(ctx)
	if err != nil {
		_ = a.report(ctx, "failed", a.reasonCode, nil, "")
		return true
	}
	execution, err := g.PrepareGuardianExecution(ctx, p.invocation.CallID, p.invocation.Tool, p.arguments, p.invocation.Ref.Binding(), ReviewContentRequest{AttemptID: reviewer.ID(), MaxBytes: reviewer.MaxContentBytes()})
	if err != nil {
		_ = a.report(ctx, "failed", a.reasonCode, nil, "")
		return true
	}
	a.execution = execution
	candidate, err := execution.Candidate(ctx, reviewer.ID(), authorization, reviewer.Versions())
	if err != nil {
		_ = a.report(ctx, "failed", a.reasonCode, nil, "")
		a.close()
		return true
	}
	a.candidate = candidate
	if err := a.report(ctx, "started", "review_started", nil, ""); err != nil {
		a.reasonCode = "audit_unavailable"
		a.close()
		return true
	}
	content := make(map[string][]byte)
	for _, file := range candidate.Execution.Content {
		content[file.Path] = execution.Content(file.Path)
	}
	a.evidence, err = reviewer.Review(ctx, candidate, content)
	if err != nil {
		a.evidence = nil
		if errors.Is(err, context.DeadlineExceeded) {
			a.reasonCode = "review_timed_out"
		}
		if errors.Is(err, context.Canceled) {
			a.reasonCode = "review_canceled"
		}
	}
	if a.evidence != nil {
		a.reasonCode = "approval_required"
		assessment := a.evidence.Assessment()
		a.reason = guardian.OutcomeKeepAsk.Reason(&assessment)
	}
	// Always reenter the sole Policy entry after model or source waits.
	return true
}

func (a *guardianAttempt) input(ctx context.Context, invocation Invocation) *policy.GuardianInput {
	if a == nil || a.execution == nil || a.evidence == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		a.close()
		return nil
	}
	current, err := a.reviewer.Capture(ctx)
	if err != nil || current.Digest() != a.candidate.Authorization.Digest() || a.execution.ValidateInvocation(ctx, invocation) != nil {
		a.reasonCode = "evidence_invalidated"
		_ = a.report(ctx, "invalidated", a.reasonCode, nil, "")
		a.reason = "Guardian evidence changed; human approval is required"
		a.close()
		return nil
	}
	enabled := true
	if readiness, ok := a.reviewer.(interface{ AutomaticApprovalReady() bool }); ok {
		enabled = readiness.AutomaticApprovalReady()
	}
	if !enabled {
		a.reasonCode = "automatic_approval_disabled"
	}
	return &policy.GuardianInput{Enabled: enabled, Candidate: a.candidate, Evidence: a.evidence}
}

func (g *Guard) withProcessStart(ctx context.Context, p preparedExecution, lease authority.ExecutionLease) context.Context {
	var started atomic.Bool
	return startgate.With(ctx, func(start func() error) (startErr error) {
		defer func() {
			if startErr != nil && p.review != nil {
				_ = p.review.report(ctx, "invalidated", "execution_invalidated", nil, "")
			}
		}()
		if !started.CompareAndSwap(false, true) {
			return errors.New("process start admission may only be consumed once")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		admit := func() error {
			g.mu.Lock()
			defer g.mu.Unlock()
			if p.livePolicy != nil && g.policy != p.livePolicy {
				return errors.New("policy runtime changed before process start")
			}
			return g.registry.WithCurrentBinding(p.invocation.Ref, p.review != nil, func() error {
				return g.policy.WithRevision(p.runtime.Revision, func() error {
					if err := ctx.Err(); err != nil {
						return err
					}
					return g.leaseAuthority.Start(lease, start)
				})
			})
		}
		if p.review != nil {
			return p.review.reviewer.WithAuthorization(ctx, p.review.candidate.Authorization, func() error {
				return p.review.execution.withLaunch(ctx, admit)
			})
		}
		return admit()
	})
}
