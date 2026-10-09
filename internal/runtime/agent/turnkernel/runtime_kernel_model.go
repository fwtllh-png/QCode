package turnkernel

import (
	"context"
	"math"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerassembly "github.com/fwtllh-png/QCode/internal/adapter/provider/assembly"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (s *RuntimeKernel) BeginModelSample(
	ctx context.Context,
	sampleID string,
) error {
	s.mu.Lock()
	if _, exists := s.state.SampleLedger[sampleID]; !exists {
		if err := s.applyAuthoritativeLocked(
			ModelSampleRequested{SampleID: sampleID},
		); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	sample := s.state.SampleLedger[sampleID]
	if sample.ProviderRetries > 0 && sample.RetryBudget == (ProviderRetryBudget{}) {
		s.mu.Unlock()
		return protocol.NewFault(protocol.CodeConflict, "retained model sample has no classified retry budget", false,
			protocol.FaultMetadata{Origin: protocol.FaultOriginKernel, Disposition: protocol.FaultResumeTurn,
				RecoveryAction: "continue in a new recovery turn using the retained work; this older sample cannot safely resume its retry budget"}, nil)
	}
	retryAt := sample.RetryBudget.WaitUntil
	s.mu.Unlock()
	if delay := time.Until(retryAt); !retryAt.IsZero() && delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	from := s.state.Phase
	effect, err := s.dispatcher.Start(
		EffectSampleProvider,
		sampleID,
	)
	if err != nil {
		return err
	}
	s.recordAcceptedLocked(EffectStarted{
		EffectID: effect.ID,
		Attempt:  effect.Attempt,
	}, from)
	return nil
}

func (s *RuntimeKernel) FinishModelSample(
	sampleID string,
	text string,
	calls []provider.ToolCall,
	usage provider.Usage,
	cost float64,
	costKnown bool,
	continued bool,
	sampleErr error,
	failure *provider.Failure,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	effect, _, err := s.dispatcher.Routed(
		EffectSampleProvider,
		sampleID,
	)
	if err != nil {
		return err
	}
	states := make([]ToolCallState, 0, len(calls))
	for _, call := range calls {
		states = append(states, ToolCallState{
			ID: call.ID, Name: call.Name,
			Arguments:         call.Arguments,
			CatalogID:         call.CatalogID,
			CatalogGeneration: call.CatalogGeneration,
			CatalogRevision:   call.CatalogRevision,
			CatalogAuthority:  call.CatalogAuthority,
		})
	}
	command := ModelSampleResultReceived{
		EffectID: effect.ID,
		SampleID: sampleID,
		Usage: UsageState{
			InputTokens:     usage.InputTokens,
			OutputTokens:    usage.OutputTokens,
			ReasoningTokens: usage.ReasoningTokens,
			CachedTokens:    usage.CachedTokens,
			CostMicrounits:  uint64(math.Round(cost * 1_000_000)),
			CostKnown:       costKnown,
		},
		Text:      text,
		Calls:     states,
		Continued: continued,
	}
	if sampleErr != nil {
		command.Error = sampleErr.Error()
		if failure == nil {
			failure = &provider.Failure{
				Code: provider.FailureUnknown, Message: sampleErr.Error(),
			}
		}
		copy := *failure
		command.Failure = &copy
	}
	from := s.state.Phase
	if err := s.dispatcher.Resolve(command); err != nil {
		return err
	}
	s.recordAcceptedLocked(command, from)
	return nil
}

func (s *RuntimeKernel) RecordSupplementalUsage(
	source, sampleID string,
	usage provider.Usage,
	cost float64,
	costKnown bool,
) error {
	return s.applyAuthoritative(
		SupplementalUsageRecorded{
			Source: source, SampleID: sampleID,
			Usage: UsageState{
				InputTokens:     usage.InputTokens,
				OutputTokens:    usage.OutputTokens,
				ReasoningTokens: usage.ReasoningTokens,
				CachedTokens:    usage.CachedTokens,
				CostMicrounits: uint64(
					math.Round(cost * 1_000_000),
				),
				CostKnown: costKnown,
			},
		},
	)
}

func (s *RuntimeKernel) ScheduleProviderRetry(
	sampleID string,
	command ProviderRetryRequested,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	effect, _, err := s.dispatcher.Routed(
		EffectSampleProvider,
		sampleID,
	)
	if err != nil {
		return err
	}
	from := s.state.Phase
	command.EffectID = effect.ID
	command.SampleID = sampleID
	command.Attempt = effect.Attempt
	if err := s.dispatcher.ScheduleRetry(
		EffectSampleProvider,
		sampleID,
		command,
	); err != nil {
		return err
	}
	s.recordAcceptedLocked(command, from)
	return nil
}

func (s *RuntimeKernel) ProviderRetryBudget(sampleID string) ProviderRetryBudget {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.SampleLedger[sampleID].RetryBudget
}

func (s *RuntimeKernel) ReserveProviderWait(sampleID string, delay time.Duration, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.state.SampleLedger[sampleID]; !exists {
		if err := s.applyAuthoritativeLocked(ModelSampleRequested{SampleID: sampleID}); err != nil {
			return err
		}
	}
	return s.applyAuthoritativeLocked(ProviderWaitReserved{SampleID: sampleID, Delay: delay, Until: until})
}

func (s *RuntimeKernel) FinishModelTransport(sampleID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	from := s.state.Phase
	effect, _, err := s.dispatcher.Routed(
		EffectSampleProvider,
		sampleID,
	)
	if err != nil {
		return err
	}
	if err := s.dispatcher.Requeue(
		EffectSampleProvider,
		sampleID,
	); err != nil {
		return err
	}
	s.recordAcceptedLocked(
		EffectRequeued{EffectID: effect.ID},
		from,
	)
	return nil
}

func (s *RuntimeKernel) SampleAssembly(
	sampleID string,
) *providerassembly.ResponseAssembly {
	s.mu.Lock()
	defer s.mu.Unlock()
	return providerassembly.CloneResponseAssembly(
		s.state.SampleLedger[sampleID].Assembly,
	)
}

// RecordModelSampleProgress borrows assembly for the synchronous submission.
// The caller may mutate it after this method returns; the reducer owns the
// defensive copy retained by authoritative state.
func (s *RuntimeKernel) RecordModelSampleProgress(
	sampleID string,
	assembly *providerassembly.ResponseAssembly,
) error {
	if assembly == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	effect, _, err := s.dispatcher.Routed(
		EffectSampleProvider,
		sampleID,
	)
	if err != nil {
		return err
	}
	command := ModelSampleProgressRecorded{
		EffectID: effect.ID,
		SampleID: sampleID,
		Attempt:  effect.Attempt,
		Assembly: *assembly,
	}
	return s.applyAuthoritativeLocked(command)
}

func (s *RuntimeKernel) EvaluateTurnStep(
	progressKey string,
) (StepAction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.applyAuthoritativeLocked(EvaluateTurnStep{
		ProgressKey: progressKey,
	})
	return s.state.NextAction, err
}
