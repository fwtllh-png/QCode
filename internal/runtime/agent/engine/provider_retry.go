package engine

import (
	"errors"
	"fmt"
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

func exhaustedToolArgumentRepair(cause error, used, limit int) error {
	message := fmt.Sprintf("provider tool argument regeneration budget exhausted (%d/%d); rejected tool calls were not executed", used, max(limit, 0))
	return protocol.NewFault(protocol.CodeUnavailable, message, false, protocol.FaultMetadata{
		Origin: protocol.FaultOriginProvider, Stage: protocol.FaultStageModelSample,
		Reason:      protocol.ProblemReasonToolArgumentRepair,
		Disposition: protocol.FaultResumeTurn, SideEffects: protocol.SideEffectUnchanged,
		RetryOwner: protocol.FaultRetryOwnerHost, ResumeHint: protocol.FaultResumeResumeTurn,
		RecoveryAction: "continue from the retained work after checking the provider or selecting another model; completed tools must not be repeated",
	}, errors.Join(cause, &provider.Failure{Code: provider.FailureMalformedResponse, Message: message}))
}

func kernelProviderRetry(retry ProviderRetry) turnkernel.ProviderRetryRequested {
	return turnkernel.ProviderRetryRequested{
		Retry:          retry.Retry,
		Failure:        retry.Failure,
		EffectiveDelay: retry.EffectiveDelay,
		RetryAt:        retry.RetryAt,
		PolicyRevision: retry.PolicyRevision,
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
		MaxRetries:                e.options.MaxRetries,
		MaxDelay:                  e.options.MaxRetryDelay,
		InfrastructureRetryLimit:  e.options.InfrastructureRetryLimit,
		RateLimitMaxRetries:       e.options.RateLimitMaxRetries,
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
	code, retryable := protocol.CodeUnavailable, true
	metadata := protocol.FaultMetadata{
		Origin: protocol.FaultOriginProvider, Stage: protocol.FaultStageModelSample,
		Disposition: protocol.FaultRetryTurn, SideEffects: protocol.SideEffectUnchanged,
		RetryOwner: protocol.FaultRetryOwnerHost, ResumeHint: protocol.FaultResumeRetryTurn,
		RecoveryAction: "check provider availability or select another model, then retry from the retained work",
	}
	if original := protocol.ProblemOf(err); original != nil {
		code, retryable = original.Code, original.Retryable
	}
	switch failure.Code {
	case provider.FailureServer, provider.FailureTransport, provider.FailureStreamClosed,
		provider.FailureTimeout, provider.FailureEmptyResponse:
		metadata.Reason = protocol.ProblemReasonProviderRetry
	case provider.FailureQuota:
		code, retryable = protocol.CodeResourceExhausted, false
		metadata.Reason = protocol.ProblemReasonProviderQuota
		metadata.Disposition, metadata.ResumeHint = protocol.FaultResumeTurn, protocol.FaultResumeResumeTurn
		metadata.RecoveryAction = "wait for the provider quota reset or restore the subscription/balance, then continue from the durable checkpoint"
	case provider.FailureAuth:
		retryable = false
		metadata.Reason = protocol.ProblemReasonProviderAuth
		metadata.RecoveryAction = "check the API key and access permissions in Connection settings, then retry"
	case provider.FailureInvalidRequest:
		retryable = false
		metadata.Reason = protocol.ProblemReasonProviderRequest
		metadata.RecoveryAction = "correct the model or provider request configuration in Connection settings, then retry"
	case provider.FailureUnsupportedContent:
		retryable = false
		metadata.Reason = protocol.ProblemReasonProviderContent
		metadata.RecoveryAction = "review the provider's content restrictions and supported input types before retrying"
	case provider.FailureMalformedResponse:
		retryable = false
		metadata.Reason = protocol.ProblemReasonProviderResponse
		metadata.RecoveryAction = "inspect the provider response failure or select another model before retrying; rejected tool calls were not executed"
	case provider.FailureContextWindowExceeded:
		retryable = false
		metadata.Reason = protocol.ProblemReasonContextWindow
		metadata.RecoveryAction = "reduce required context or select a model with a larger context window, then continue"
	case provider.FailureRateLimit:
		metadata.Reason = protocol.ProblemReasonProviderRateLimited
		metadata.RecoveryAction = "wait for the shared provider cooldown, then continue from the durable checkpoint"
	}
	problem := protocol.NewFault(code, failure.Message, retryable, metadata, err)
	if original := protocol.ProblemOf(err); original != nil {
		problem.HTTPStatus, problem.RateLimit, problem.Details = original.HTTPStatus, original.RateLimit, original.Details
	}
	return problem
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
	projectHistory agentcontext.HistoryProjector,
) (bool, error) {
	if meaningful || e.viewFold.folded ||
		providerwire.ClassifyFailure(err, meaningful).Code !=
			provider.FailureContextWindowExceeded {
		return false, nil
	}
	before := e.projectGateHistory(*history, projectHistory)
	beforeWindow, measureErr := e.measureTokenWindow(
		input.WithHistory(before), outputReserve, 0,
	)
	if measureErr != nil {
		return false, nil
	}
	after, afterWindow, folded, measureErr := e.foldForNetReduction(
		*history, input, outputReserve, 0, agentcontext.OmittedProviderOverflow,
		projectHistory, before, beforeWindow,
	)
	if measureErr != nil || !folded {
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
