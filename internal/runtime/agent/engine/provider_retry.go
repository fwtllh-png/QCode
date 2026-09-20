package engine

import (
	"errors"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const providerRetryPolicyRevision = providerwire.RetryPolicyRevision

func kernelProviderRetry(retry ProviderRetry) turnkernel.ProviderRetryRequested {
	return turnkernel.ProviderRetryRequested{
		Retry:            retry.Retry,
		Failure:          retry.Failure,
		EffectiveDelayMS: uint64(retry.EffectiveDelay / time.Millisecond),
		RetryAt:          retry.RetryAt,
		PolicyRevision:   retry.PolicyRevision,
	}
}

func kernelProviderFailure(err error) *provider.Failure {
	if err == nil {
		return nil
	}
	failure := providerwire.ClassifyFailure(err, false)
	return &failure
}

type rateLimitBudget struct {
	retries  uint32
	waited   time.Duration
	cooldown time.Duration
}

func (e *Engine) providerRetry(
	err error,
	meaningful bool,
	retries uint32,
	contextChanged bool,
	budget rateLimitBudget,
	sampleID string,
) (ProviderRetry, bool) {
	policy := providerwire.RetryPolicy{
		MaxRetries:          e.options.MaxRetries,
		MaxDelay:            e.options.MaxRetryDelay,
		RateLimitMaxRetries: e.options.RateLimitMaxRetries,
		RateLimitMaxWait:    e.options.RateLimitMaxWait,
		RateLimitRetries:    budget.retries,
		RateLimitWaited:     budget.waited,
		RouteCooldown:       budget.cooldown,
		JitterSeed:          e.retryJitterSeed(sampleID),
		Now:                 e.options.Observability.Now,
	}
	if shared := e.options.SharedRateLimit; shared != nil {
		policy.SharedRateLimitRetries, policy.SharedRateLimitWaited = shared.Load()
	}
	return policy.Decide(err, meaningful, retries, contextChanged)
}

func (e *Engine) retryJitterSeed(sampleID string) string {
	route := e.activeRoute()
	credential := route.Credential()
	return strings.Join([]string{
		e.options.SessionID,
		route.ConnectionID(),
		credential.Kind,
		credential.Name,
		route.Model().ID,
		strings.TrimSpace(sampleID),
	}, "\x00")
}

func exhaustedProviderRetry(err error) error {
	failure := providerwire.ClassifyFailure(err, false)
	if failure.Code == provider.FailureQuota {
		problem := protocol.NewFault(
			protocol.CodeResourceExhausted, failure.Message, false,
			protocol.FaultMetadata{
				Origin: protocol.FaultOriginProvider, Stage: protocol.FaultStageModelSample,
				Disposition: protocol.FaultResumeTurn, SideEffects: protocol.SideEffectUnchanged,
				RetryOwner: protocol.FaultRetryOwnerHost, ResumeHint: protocol.FaultResumeResumeTurn,
				RecoveryAction: "wait for the provider quota reset or restore the subscription/balance, then continue from the durable checkpoint",
			}, err,
		)
		if original := protocol.ProblemOf(err); original != nil {
			problem.HTTPStatus, problem.RateLimit = original.HTTPStatus, original.RateLimit
		}
		return problem
	}
	if problem, ok := errors.AsType[*protocol.Problem](err); ok &&
		!problem.Retryable {
		problem.Fault = &protocol.FaultMetadata{
			Origin:         protocol.FaultOriginProvider,
			Stage:          protocol.FaultStageModelSample,
			Disposition:    protocol.FaultRetryTurn,
			SideEffects:    protocol.SideEffectUnchanged,
			RetryOwner:     protocol.FaultRetryOwnerHost,
			ResumeHint:     protocol.FaultResumeRetryTurn,
			RecoveryAction: "correct the provider request or configuration, then retry from the durable checkpoint",
		}
		return err
	}
	fault := protocol.FaultMetadata{
		Origin: protocol.FaultOriginProvider, Disposition: protocol.FaultRetryTurn, SideEffects: protocol.SideEffectUnchanged,
		RecoveryAction: "retry the turn from its durable checkpoint",
	}
	if problem, ok := errors.AsType[*protocol.Problem](err); ok {
		problem.Retryable, problem.Fault = true, &fault
		return err
	}
	return protocol.NewFault(protocol.CodeUnavailable, "provider could not complete the model sample: "+errorText(err), true, fault, err)
}

func exhaustedRateLimitRetry(err error) error {
	recovered := exhaustedProviderRetry(err)
	var problem *protocol.Problem
	if errors.As(recovered, &problem) && problem != nil {
		problem.Message = "provider rate limit retry budget exhausted: " +
			providerwire.ClassifyFailure(err, false).Message
		problem.Fault.Reason = protocol.ProblemReasonProviderRateLimited
		problem.Fault.RecoveryAction = "wait for the shared provider cooldown, then continue from the durable checkpoint"
	}
	return recovered
}

func (e *Engine) routeCooldown(route model.ReadyRoute) time.Duration {
	source, ok := e.options.Provider.(interface {
		RouteCooldown(model.ReadyRoute) time.Duration
	})
	if !ok {
		return 0
	}
	return source.RouteCooldown(route)
}

func (e *Engine) recoverContextOverflow(
	err error,
	meaningful bool,
	history *[]provider.Message,
	input agentcontext.MessageSnapshot,
	outputReserve uint64,
	send func(State, Event) error,
) (bool, error) {
	if meaningful ||
		providerwire.ClassifyFailure(err, meaningful).Code !=
			provider.FailureContextWindowExceeded {
		return false, nil
	}
	before := e.projectGateHistory(*history, e.contextViewProject(nil))
	beforeWindow, measureErr := e.measureTokenWindow(
		input.WithHistory(before), outputReserve, 0,
	)
	if measureErr != nil || !e.foldOldestVisibleTail(*history, true) {
		return false, nil
	}
	after := e.projectGateHistory(*history, e.contextViewProject(nil))
	afterWindow, measureErr := e.measureTokenWindow(
		input.WithHistory(after), outputReserve, 0,
	)
	if measureErr != nil || agentcontext.HistoryBytes(after) >= agentcontext.HistoryBytes(before) {
		return false, nil
	}
	receipt := viewFoldReceipt(
		CompactionPhaseMidTurn, before, after, beforeWindow, afterWindow,
	)
	if err := send(Compacting, Event{Compaction: receipt}); err != nil {
		return false, err
	}
	return true, nil
}
