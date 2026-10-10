package engine

import (
	"context"
	"errors"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/common/tokenestimate"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/guardian"
)

type guardianCall struct {
	emit   func(Event) error
	record protocol.GuardianReviewData
	*GuardianReview
	source        agentcontext.GuardianSource
	scope         agentcontext.AuthorizationScope
	authorization agentcontext.GuardianAuthorization
	identity      tool.InvocationIdentity
}

func (e *Engine) guardianContext(ctx context.Context, emit func(Event) error) context.Context {
	if !e.options.Guardian.Enabled {
		return ctx
	}
	// This captures only this call's immutable identity and source dependency.
	identity := tool.InvocationIdentityFrom(ctx)
	source := e.options.GuardianSource
	workspace := e.guard.GuardianWorkspaceID()
	child := e.options.Security != nil && e.options.Security.CloneSampling().DisableHostExecution
	return toolguard.WithGuardian(ctx, func(ctx context.Context) (toolguard.GuardianReviewer, error) {
		if source == nil {
			return nil, errors.New("Guardian durable authorization source is unavailable")
		}
		review, err := e.prepareGuardianReview(ctx)
		if err != nil {
			return nil, err
		}
		review.toolSpend = true
		return &guardianCall{GuardianReview: review, source: source, identity: identity, emit: emit,
			scope: agentcontext.AuthorizationScope{WorkspaceID: workspace, SessionID: identity.SessionID, ThreadID: identity.ThreadID, Child: child}}, nil
	})
}

// Durable facts and current user authority are prerequisites in addition to
// the explicit configuration switch checked by PrepareGuardianReview.
func (r *guardianCall) AutomaticApprovalReady() bool { return r.emit != nil && r.source != nil }

func (r *guardianCall) MaxContentBytes() int64 {
	return int64(tokenestimate.BytesForTokens(r.route.Model().Limits.ContextTokens))
}

func (r *guardianCall) Capture(ctx context.Context) (guardian.AuthorizationSnapshot, error) {
	if living := r.options.SessionForTurn; living != nil {
		if session, ok := living(ctx, r.identity.TurnID); !ok || session != r.identity.SessionID {
			return guardian.AuthorizationSnapshot{}, errors.New("Guardian turn no longer belongs to the session")
		}
	}
	a, err := r.source.Capture(ctx, r.scope)
	if err != nil {
		return guardian.AuthorizationSnapshot{}, err
	}
	if r.authorization.Snapshot().Revision == "" {
		r.authorization = a
	}
	return a.Snapshot(), nil
}

func (r *guardianCall) Review(ctx context.Context, candidate guardian.ReviewCandidate, content map[string][]byte) (*guardian.ReviewEvidence, error) {
	result, err := r.GuardianReview.Review(ctx, candidate, r.authorization, content)
	if result.Attempted {
		r.engine.addToolSpend(result.Usage, result.CostUSD, result.CostKnown, r.engine.nextSample())
	}
	if r.emit != nil {
		if auditErr := r.reportAssessment(ctx, result, err); auditErr != nil {
			return nil, errors.Join(err, auditErr)
		}
	}
	return result.Evidence, err
}

// Lock order: Engine accounting, durable source fence, review copy, Guard,
// catalog, Policy/user rules, LeaseAuthority. No callback may reenter a holder.
func (r *guardianCall) WithAuthorization(ctx context.Context, expected guardian.AuthorizationSnapshot, start func() error) error {
	accounting := &r.engine.titleState
	accounting.mu.Lock()
	defer accounting.mu.Unlock()
	if guardianReviewDigest(accounting.options.Guardian) != r.versions.ConfigurationDigest {
		return errors.New("Guardian configuration changed before process start")
	}
	route, err := accounting.options.Routes.For(model.PurposeJudge)
	if err != nil {
		return err
	}
	descriptor, err := route.Describe()
	if err != nil {
		return err
	}
	if guardianReviewDigest(descriptor) != r.versions.RouteDigest {
		return errors.New("Guardian route changed before process start")
	}
	return r.source.WithCurrent(ctx, r.scope, expected, start)
}
