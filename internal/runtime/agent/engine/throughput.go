package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerratelimit "github.com/fwtllh-png/QCode/internal/adapter/provider/ratelimit"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type throughputGovernor interface {
	DecideThroughput(model.ReadyRoute, uint64, uint64) providerratelimit.Decision
	ReserveThroughput(model.ReadyRoute, uint64)
}

type atomicThroughputGovernor interface {
	TryReserveThroughput(model.ReadyRoute, uint64, uint64) providerratelimit.Decision
}

type throughputShrink func() (uint64, bool, error)

func (e *Engine) admitProviderThroughput(
	ctx context.Context,
	route model.ReadyRoute,
	required uint64,
	waited *time.Duration,
	shrink throughputShrink,
	reserveWait func(time.Duration, time.Time) error,
) error {
	governor, ok := e.options.Provider.(throughputGovernor)
	if !ok {
		return nil
	}
	decide := governor.DecideThroughput
	atomicGovernor, atomic := e.options.Provider.(atomicThroughputGovernor)
	if atomic {
		decide = atomicGovernor.TryReserveThroughput
	}
	decision := decide(
		route, required, e.options.TokensPerMinute,
	)
	if shrink != nil &&
		throughputShouldShrink(decision, e.options.RateLimitMaxWait, waited) {
		next, folded, err := shrink()
		if err != nil {
			return err
		}
		if folded {
			required = next
			decision = decide(
				route, required, e.options.TokensPerMinute,
			)
		}
	}
	switch decision.Status {
	case providerratelimit.StatusAdmit:
		if !atomic && decision.Source != providerratelimit.SourceUnknown {
			governor.ReserveThroughput(route, required)
		}
		return nil
	case providerratelimit.StatusWait:
		already := time.Duration(0)
		if waited != nil {
			already = *waited
		}
		if e.options.RateLimitMaxWait > 0 &&
			already+decision.Wait > e.options.RateLimitMaxWait {
			return throughputRefusal(
				decision,
				providerratelimit.ReasonWaitExceedsBudget,
				true,
			)
		}
		if reserveWait != nil {
			if err := reserveWait(decision.Wait, time.Now().Add(decision.Wait)); err != nil {
				return err
			}
		}
		if waited != nil {
			*waited += decision.Wait
		}
		if err := waitRetryDelay(ctx, decision.Wait); err != nil {
			return err
		}
		retried := decide(
			route, required, e.options.TokensPerMinute,
		)
		if retried.Status != providerratelimit.StatusAdmit {
			return throughputRefusal(retried, retried.Reason, false)
		}
		if !atomic {
			governor.ReserveThroughput(route, required)
		}
		return nil
	default:
		return throughputRefusal(decision, decision.Reason, false)
	}
}

func (e *Engine) abortOversizedRateLimitRetry(
	ctx context.Context,
	route model.ReadyRoute,
	required uint64,
	retry ProviderRetry,
	shrink throughputShrink,
) error {
	if retry.Failure.Code != provider.FailureRateLimit ||
		!e.throughputRefuses(route, required) {
		return nil
	}
	if shrink != nil {
		next, folded, err := shrink()
		if err != nil {
			return err
		}
		if folded {
			required = next
			if !e.throughputRefuses(route, required) {
				return nil
			}
		}
	}
	return e.admitProviderThroughput(ctx, route, required, nil, nil, nil)
}

func (e *Engine) throughputRefuses(
	route model.ReadyRoute,
	required uint64,
) bool {
	governor, ok := e.options.Provider.(throughputGovernor)
	if !ok {
		return false
	}
	decision := governor.DecideThroughput(
		route, required, e.options.TokensPerMinute,
	)
	return decision.Status == providerratelimit.StatusRefuse
}

func throughputShouldShrink(
	decision providerratelimit.Decision,
	maxWait time.Duration,
	waited *time.Duration,
) bool {
	if decision.Status == providerratelimit.StatusRefuse {
		return decision.Reason == providerratelimit.ReasonExceedsBurst
	}
	if decision.Status != providerratelimit.StatusWait || maxWait <= 0 {
		return false
	}
	already := time.Duration(0)
	if waited != nil {
		already = *waited
	}
	return already+decision.Wait > maxWait
}

func (e *Engine) foldWorkingSetForThroughput(
	history *[]provider.Message,
	projectHistory agentcontext.HistoryProjector,
	input agentcontext.MessageSnapshot,
	outputReserve uint64,
	phase string,
	send func(State, Event) error,
) (uint64, bool, error) {
	if history == nil {
		return 0, false, nil
	}
	before := e.projectGateHistory(*history, projectHistory)
	beforeWindow, err := e.measureTokenWindow(
		input.WithHistory(before), outputReserve, 0,
	)
	if err != nil {
		return 0, false, err
	}
	after, afterWindow, folded, err := e.foldForNetReduction(
		*history, input, outputReserve, 0, agentcontext.OmittedThroughput,
		projectHistory, before, beforeWindow,
	)
	if err != nil {
		return 0, false, err
	}
	if !folded {
		return 0, false, nil
	}
	receipt := viewFoldReceipt(
		phase, before, after, beforeWindow, afterWindow,
	)
	receipt.TruncationReason = "throughput_tail_fold"
	if send != nil {
		_ = send(Compacting, Event{Compaction: receipt})
	}
	return afterWindow.accounting.FullActiveTokens + outputReserve, true, nil
}

func throughputRefusal(
	decision providerratelimit.Decision,
	reason string,
	retryable bool,
) error {
	if reason == "" {
		reason = decision.Reason
	}
	problem := protocol.NewProblem(
		protocol.CodeResourceExhausted,
		fmt.Sprintf(
			"provider throughput admission refused: required %d tokens, available %d, limit %d",
			decision.Required,
			decision.Available,
			decision.Limit,
		),
		retryable,
		nil,
	)
	problem.Details = &protocol.ProblemDetails{
		Reason:     protocol.ProblemReasonProviderThroughput,
		ResourceID: reason,
	}
	problem.Fault.Origin = protocol.FaultOriginProvider
	problem.Fault.Stage = protocol.FaultStageAdmission
	problem.Fault.Reason = protocol.ProblemReasonProviderThroughput
	problem.Fault.RetryOwner = protocol.FaultRetryOwnerHost
	problem.Fault.RecoveryAction = "reduce the request or adjust the configured throughput limit before continuing"
	if retryable {
		problem.Fault.RecoveryAction = "wait for the provider throughput window to reset, then continue"
	}
	return problem
}
