package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func normalizeEngineOptions(options *Options) error {
	summaryBytes := options.SummaryMaxBytes
	capacity := agentcontext.ResolveCapacity(
		options.Route,
		options.MaxOutputTokens,
		options.Budget.MaxTurnTokens,
		options.Budget.MaxTokens,
	)
	if options.Context.TruthRetention.TruthMaxBytes <= 0 {
		options.Context.TruthRetention.TruthMaxBytes =
			truthMaxBytesForCapacity(
				capacity.HardInputTokens,
				summaryBytes,
			)
	}
	options.Context.TruthRetention =
		options.Context.TruthRetention.Normalized()
	if err := options.Context.TruthRetention.Validate(summaryBytes); err != nil {
		return fmt.Errorf("truth retention: %w", err)
	}
	switch options.Context.SemanticNarrative {
	case "", "off":
		options.Context.SemanticNarrative = "off"
	case "post_turn":
	default:
		return errors.New("semantic narrative mode is invalid")
	}
	switch options.Context.Digest {
	case "", "ledger":
		options.Context.Digest = "ledger"
	case "ledger+narrative":
	default:
		return errors.New("context view digest is invalid")
	}
	if options.Context.RecentTailTurns < 0 {
		return errors.New("recent tail turns cannot be negative")
	}
	if options.Context.CheckpointMaxBytes < 0 {
		return errors.New("checkpoint max bytes cannot be negative")
	}
	if options.Context.NarrativeTimeout < 0 {
		return errors.New("semantic narrative timeout cannot be negative")
	}
	if options.Context.NarrativeTimeout == 0 {
		options.Context.NarrativeTimeout = 30 * time.Second
	}
	if options.Context.NarrativeRetryLimit < 0 {
		return errors.New("semantic narrative retry limit cannot be negative")
	}
	manifestDefaults := agentcontext.DefaultManifestLimits()
	if options.Context.OwnerDeltaMaxSegments <= 0 {
		options.Context.OwnerDeltaMaxSegments =
			manifestDefaults.OwnerDeltaMaxSegments
	}
	if options.Context.OwnerDeltaMaxBytes <= 0 {
		options.Context.OwnerDeltaMaxBytes =
			manifestDefaults.OwnerDeltaMaxBytes
	}
	prepareLimit, compactLimit, emergencyLimit := agentcontext.WindowThresholds(
		options.Context.Window,
		capacity.HardInputTokens,
	)
	if prepareLimit > compactLimit || compactLimit > emergencyLimit {
		return errors.New(
			"context compaction thresholds must satisfy prepare <= compact <= emergency",
		)
	}
	if err := validateReasoningEffort(
		options.Routes,
		options.ReasoningEffort,
	); err != nil {
		return err
	}
	if options.MaxRetries < 0 {
		return errors.New("provider retry limit cannot be negative")
	}
	if options.InfrastructureRetryLimit < -1 {
		return errors.New("infrastructure retry limit cannot be less than -1")
	}
	if options.MaxRetryDelay < 0 {
		return errors.New("max retry delay cannot be negative")
	}
	if options.MaxRetryDelay == 0 {
		options.MaxRetryDelay = 2 * time.Minute
	}
	if options.RateLimitMaxRetries < 0 {
		return errors.New("rate limit max retries cannot be negative")
	}
	if options.RateLimitMaxWait < 0 {
		return errors.New("rate limit max wait cannot be negative")
	}
	return nil
}

func truthMaxBytesForCapacity(hardInputTokens uint64, summaryMaxBytes int) int {
	limit := int(min(hardInputTokens, uint64(1<<20)))
	if summaryMaxBytes > 0 {
		limit = min(limit, summaryMaxBytes-256)
	}
	return limit
}

func waitRetryDelay(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func validateReasoningEffort(
	routes model.RouteSet,
	effort string,
) error {
	if effort == "" {
		return nil
	}
	entries := []model.ReadyRoute{routes.Act()}
	for _, purpose := range routes.Slots() {
		route, err := routes.For(purpose)
		if err != nil {
			return err
		}
		entries = append(entries, route)
	}
	for _, route := range entries {
		if !route.Model().Capabilities.SupportsReasoningEffort(effort) {
			return fmt.Errorf(
				"model %q does not support reasoning effort %q",
				route.Model().ID,
				effort,
			)
		}
	}
	return nil
}
