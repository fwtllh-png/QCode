package extension

import (
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestTurnImageAttachmentsPreserveValidatedModelInput(t *testing.T) {
	images := turnImageAttachments([]provider.Attachment{{
		Name: "lake.png", MediaType: "image/png", Data: []byte("image"),
	}})
	if len(images) != 1 ||
		images[0].Label != "lake.png" ||
		images[0].MediaType != "image/png" ||
		images[0].Content != "aW1hZ2U=" {
		t.Fatalf("turn images = %+v", images)
	}
}

func TestTurnPlanExecutionPreservesApprovedRecovery(t *testing.T) {
	payload := &protocol.StartTurnPayload{
		Recovery: &protocol.TurnRecoveryContext{
			Action: protocol.TurnRecoveryContinue, SourceTurnID: "turn-source",
			PlanID: "plan-source", PlanTransition: protocol.PlanTransitionImplement,
			ProfileRevision: 2,
		},
	}
	planID, transition, approved := turnPlanExecution(payload)
	if planID != "plan-source" ||
		transition != protocol.PlanTransitionImplement ||
		!approved {
		t.Fatalf(
			"recovery plan execution = (%q, %q, %t)",
			planID,
			transition,
			approved,
		)
	}
}

func TestProviderAttemptDataMapsRetryAndCompletionFacts(t *testing.T) {
	started := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	retryAt := started.Add(2 * time.Second)
	retry := providerAttemptData(agentengine.Event{
		ProviderRetry: &providerwire.RetryDecision{
			Code: protocol.CodeUnavailable, PolicyRevision: "retry-v1",
			RetryAt: retryAt, EffectiveDelay: 2 * time.Second,
			Failure: provider.Failure{
				Code: provider.FailureRateLimit, HTTPStatus: 429, RetryAfterMS: 2500,
			},
		},
		ModelExecution: &agentengine.ModelExecution{
			Kind: "provider_attempt", SampleID: "sample-1", Attempt: 2,
			Status:               protocol.ProviderAttemptRetryWait,
			ProjectedInputTokens: 690_000,
			RateLimitRetries:     1,
			RateLimitRetryLimit:  3,
			RateLimitWaited:      1500 * time.Millisecond,
			RateLimitWaitBudget:  2 * time.Minute,
			Transport: provider.TransportMetadata{
				RequestBytes: 2_669_546, RouteCooldownWaitMS: 1200,
				Projection: provider.ProjectionReceipt{
					Mode:                       provider.ProjectionModeFullHTTP,
					FallbackReason:             provider.ProjectionFallbackCapabilityDisabled,
					LogicalTransportEquivalent: true,
				},
			},
			StartedAt: started,
		},
	})
	if retry == nil || retry.FailureCode != "rate_limit" || retry.HTTPStatus != 429 ||
		retry.ProviderRetryAfterMS != 2500 || retry.EffectiveDelayMS != 2000 ||
		retry.RouteCooldownWaitMS != 1200 || retry.RequestBytes != 2_669_546 ||
		retry.RateLimitRetries != 1 || retry.RateLimitRetryLimit != 3 ||
		retry.RateLimitWaitedMS != 1500 || retry.RateLimitWaitBudgetMS != 120000 ||
		retry.Projection == nil || retry.Projection.Mode != "complete_http_sse" ||
		retry.StartedAt == nil || !retry.StartedAt.Equal(started) {
		t.Fatalf("retry attempt = %+v", retry)
	}
	finished := started.Add(3 * time.Second)
	completed := providerAttemptData(agentengine.Event{
		ModelExecution: &agentengine.ModelExecution{
			Kind: "provider_attempt", SampleID: "sample-1", Attempt: 2,
			Status: protocol.ProviderAttemptCompleted, StopReason: provider.StopReasonToolUse,
			StartedAt: started, FinishedAt: finished,
		},
	})
	if completed == nil || completed.StopReason != "tool_use" ||
		completed.FinishedAt == nil || !completed.FinishedAt.Equal(finished) {
		t.Fatalf("completed attempt = %+v", completed)
	}
}

type recordingSink struct {
	events []protocol.EventData
}

func (s *recordingSink) Emit(data protocol.EventData) error {
	s.events = append(s.events, data)
	return nil
}

func TestStreamingBodyTextProjectsAsDraftWithSampleIdentity(t *testing.T) {
	sink := &recordingSink{}
	err := emitRichEngineEvent(sink, agentengine.Event{
		Block:    &provider.ContentBlock{Type: provider.ContentText, Text: "answer"},
		SampleID: "turn-1-step-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = emitRichEngineEvent(sink, agentengine.Event{
		OutputDiscarded: &agentengine.ModelOutputDiscarded{
			SampleID: "turn-1-step-1",
			Reason:   "narration",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 2 {
		t.Fatalf("events = %+v", sink.events)
	}
	draft, ok := sink.events[0].(*protocol.OutputDraftData)
	if !ok || draft.Text != "answer" || draft.SampleID != "turn-1-step-1" {
		t.Fatalf("draft = %+v", sink.events[0])
	}
	discarded, ok := sink.events[1].(*protocol.OutputDiscardedData)
	if !ok || discarded.SampleID != "turn-1-step-1" ||
		discarded.Reason != "narration" {
		t.Fatalf("discard = %+v", sink.events[1])
	}
}
