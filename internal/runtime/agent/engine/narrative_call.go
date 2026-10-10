package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

// One physical provider attempt. Observed usage survives every failure path.
func (e *Engine) callNarrative(ctx context.Context, options Options, request provider.ModelRequest, input agentcontext.NarrativeInputArtifact, turn uint64, outputBytes int, background bool) (result NarrativeGenerationResult, err error) {
	var release func()
	if background {
		ctx, release, err = options.SharedRateLimit.AcquireBackground(ctx)
	} else {
		release, err = options.SharedRateLimit.Acquire(ctx)
	}
	if err != nil {
		return result, err
	}
	defer release()
	route := request.Route
	result.Provider, result.Model = route.ProviderID(), route.Model().ID
	result.ModelMetadata = *modelMetadataProvenance(route.Model().MetadataProvenance)
	result.RouteDigest = input.RouteDigest
	estimated, err := options.TokenEstimator.Estimate(request.Messages)
	if err != nil {
		return result, err
	}
	var settle func(provider.Usage)
	request.MaxOutputTokens, settle, err = e.reserveAuxiliaryBudget(options, route, estimated, request.MaxOutputTokens, "")
	if err != nil {
		return result, err
	}
	defer func() { settle(result.Usage) }()
	if err = ctx.Err(); err != nil {
		return result, err
	}
	result.Attempt = 1
	defer func() {
		result.CostUSD = provider.EstimateCost(route.Model().Pricing, result.Usage)
		result.CostKnown = provider.PricingKnown(route.Model().Pricing, result.Usage)
	}()
	stream, err := options.Provider.Stream(ctx, request)
	if err != nil {
		return result, err
	}
	defer stream.Close()
	var body strings.Builder
	complete := false
	for {
		event, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return result, recvErr
		}
		switch event.Type {
		case provider.EventTextDelta:
			if outputBytes > 0 && len(event.Text) > outputBytes-body.Len() {
				return result, errors.New("narrative output exceeds byte limit")
			}
			body.WriteString(event.Text)
			outputTokens, estimateErr := options.TokenEstimator.Estimate([]provider.Message{provider.TextMessage(provider.RoleAssistant, body.String())})
			if estimateErr != nil {
				return result, estimateErr
			}
			if outputTokens > request.MaxOutputTokens {
				return result, errors.New("narrative output exceeds token budget")
			}

		case provider.EventUsage:
			if event.Usage != nil {
				result.Usage = provider.MergeCumulative(result.Usage, *event.Usage)
			}
		case provider.EventMessageStop:
			if event.StopReason.Incomplete() || event.StopReason == provider.StopReasonToolUse {
				return result, errors.New("narrative provider output is incomplete")
			}
			complete = true
		case provider.EventMessageStart, provider.EventReasoningDelta, provider.EventReasoningSignature, provider.EventTransportProgress, provider.EventReplayState, provider.EventResponseState:
		default:
			return result, fmt.Errorf("narrative provider emitted forbidden event %q", event.Type)
		}
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if !complete {
		return result, errors.New("narrative provider omitted message_stop")
	}
	result.Artifact, err = agentcontext.ValidateNarrativeJSON([]byte(body.String()), input, options.Context.NarrativeLimits, turn, time.Now().UTC())
	return result, err
}
