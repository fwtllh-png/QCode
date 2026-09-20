package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/mcp"
	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerassembly "github.com/fwtllh-png/QCode/internal/adapter/provider/assembly"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/toolsearch"
	"github.com/fwtllh-png/QCode/internal/observability/trace"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	contextview "github.com/fwtllh-png/QCode/internal/runtime/agent/contextview"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (e *Engine) reasoningEffort() string {
	// Keep reasoning effort stable for every sample in a turn. Provider support
	// is negotiated when the route is built; dynamically escalating an effort
	// from prompt keywords or repair attempts can exceed that provider contract.
	return e.options.ReasoningEffort
}

func finishOnlyReasoningEffort(
	capabilities model.Capabilities,
) string {
	efforts := capabilities.ReasoningEffortLevels()
	if len(efforts) != 0 {
		return efforts[0]
	}
	return ""
}

func (e *Engine) modelStep(
	ctx context.Context,
	history *[]provider.Message,
	turnUsage provider.Usage,
	sampleID string,
	reason string,
	providerRetries uint32,
	finishOnly bool,
	convergenceOnly bool,
	continued *bool,
	pendingInputInjected *bool,
	capturedReplay **provider.ReplayState,
	assembly *providerassembly.ResponseAssembly,
	checkpoint func(*providerassembly.ResponseAssembly) error,
	beginAttempt func() error,
	finishTransport func() error,
	send func(State, Event) error,
) ([]provider.ContentBlock, []provider.ToolCall, provider.Usage, uint64, error) {
	e.viewFold.folded = false
	var rateLimitRetries uint32
	var rateLimitWaited time.Duration
	if continued != nil {
		*continued = false
	}
	if pendingInputInjected != nil {
		*pendingInputInjected = false
	}
	if capturedReplay != nil {
		*capturedReplay = nil
	}
	if assembly == nil {
		assembly = providerassembly.NewResponseAssembly(sampleID)
	}
	if err := assembly.Validate(); err != nil {
		return nil, nil, provider.Usage{}, 0,
			fmt.Errorf("restore provider response assembly: %w", err)
	}
	scope := e.runningScope()
	if scope == nil {
		return nil, nil, provider.Usage{}, 0, errors.New("turn scope is not active")
	}
	catalog := e.scopeCatalog(scope)
	definitions, advertised, err := e.toolDefinitionsFromSnapshot(
		catalog,
		scope.spec.Request,
	)
	if err != nil {
		return nil, nil, provider.Usage{}, 0, err
	}
	if assembly.State == providerassembly.ResponseComplete {
		calls, err := assembly.ExecutableToolCalls()
		if err == nil {
			bindToolCalls(calls, catalog, advertised)
			if continued != nil {
				*continued = assembly.TransportCount() > 1
			}
			if capturedReplay != nil {
				*capturedReplay = assembly.CurrentReplay()
			}
			return assembly.ConfirmedBlocks(), calls,
				assembly.TotalUsage(), 0, nil
		}
	}
	admittedHistory, err := e.admitToolResultHistory(*history)
	if err != nil {
		return nil, nil, provider.Usage{}, 0, err
	}
	*history = admittedHistory
	if err := e.emitMCPHealthChanges(scope.spec.MCP, send); err != nil {
		return nil, nil, provider.Usage{}, 0, err
	}
	_, imageReopenAvailable := catalog.Lookup(tool.ImageReopenToolName)
	if imageReopenAvailable {
		if err := e.options.Tools.BindImageHandles(ctx, *history); err != nil {
			return nil, nil, provider.Usage{}, 0, err
		}
	}
	if changed := e.catalogChange(catalog); changed != nil {
		if err := send(CallingModel, Event{CatalogChanged: changed}); err != nil {
			return nil, nil, provider.Usage{}, 0, err
		}
	}
	stableContext, worldDelta, worldReceipts, worldProjection, err := e.projectWorldState(
		ctx, *history, catalog, advertised,
	)
	if err != nil {
		return nil, nil, provider.Usage{}, 0, err
	}
	*history = append(*history, cloneMessages(worldDelta)...)
	scope.mu.Lock()
	if scope.state.contextLedger == nil {
		scope.state.contextLedger = agentcontext.NewMessageLedger(agentcontext.LedgerInput{
			Stable: stableContext, History: *history, Definitions: definitions,
		})
	}
	contextLedger := scope.state.contextLedger
	scope.mu.Unlock()
	totalUsage := assembly.TotalUsage()
	var lastEstimate uint64
	var providerAttempt uint32
	var continuationMessages []provider.Message
	var continuedBlocks []provider.ContentBlock
	continuations := uint32(0)
	if assembly.TransportCount() != 0 {
		continuedBlocks = assembly.ConfirmedBlocks()
		if len(continuedBlocks) != 0 {
			continuationMessages = append(
				continuationMessages,
				provider.ProducedAssistant(
					e.activeRoute(),
					cloneBlocks(continuedBlocks),
					e.turn,
					nil,
				),
			)
		}
		continuationMessages = append(
			continuationMessages,
			promptcontext.IncompleteOutputFeedback(
				provider.StopReasonIncomplete,
				assembly.IncompleteToolFragments(),
				e.turn,
			),
		)
		continuations = uint32(assembly.TransportCount())
		if continued != nil {
			*continued = true
		}
	}
	baseReasoningEffort := e.reasoningEffort()
	for attempt := 0; ; attempt++ {
		var turnContext []provider.Message
		turnReceipts := append([]promptcontext.Receipt(nil), worldReceipts...)
		e.recordTurnContextReceipts(turnReceipts)
		route := e.activeRoute()
		maxOutputTokens := e.maxOutputFor(route)
		turnContext = append(turnContext, e.closedTurnCheckpointMessages()...)
		budgetMessage, budgetFinishOnly := e.budgetConvergence(
			turnUsage.Total() + totalUsage.Total(),
		)
		if len(budgetMessage.Blocks) != 0 {
			turnContext = append(turnContext, budgetMessage)
		}
		requestTools := definitions
		reasoningEffort := baseReasoningEffort
		nativeSearch := e.options.NativeSearch
		if convergenceOnly {
			reasoningEffort = finishOnlyReasoningEffort(
				route.Model().Capabilities,
			)
			nativeSearch = false
		}
		remainingCalls := tool.RemainingBusinessCalls(
			requestTools,
			finishOnly || convergenceOnly || budgetFinishOnly,
		)
		sampleReason := promptcontext.SampleReason(
			reason, attempt, continuations > 0,
		)
		statelessProjector := contextview.NewStatelessProjector(
			route.Model().Capabilities.IncrementalResponses,
		)
		projectHistory := e.contextViewProject(statelessProjector.Project)
		project := func() agentcontext.MessageSnapshot {
			return contextLedger.Project(agentcontext.LedgerProjection{
				Stable: stableContext, History: projectHistory(*history), Dynamic: turnContext,
				Continuation: continuationMessages, Definitions: requestTools,
			})
		}
		snapshot := project()
		phase := CompactionPhaseMidTurn
		if turnUsage.InputTokens+totalUsage.InputTokens == 0 {
			phase = CompactionPhasePreSampling
		}
		gateSend := deduplicateCompactionReceipts(send)
		admission := e.economicAdmission(
			turnUsage, totalUsage, maxOutputTokens, maxOutputTokens,
			remainingCalls,
		)
		economicFinishOnly := false
		if admission.Budgeted && admission.AllowedInput == 0 {
			admission = e.economicAdmission(
				turnUsage, totalUsage, 0, 0, 1,
			)
			economicFinishOnly = true
		}
		window, err := e.runCompactGate(
			ctx,
			history, snapshot, maxOutputTokens, phase, true, gateSend,
			admission.AllowedInput, projectHistory,
		)
		if err != nil {
			return nil, nil, totalUsage, window.estimated, err
		}
		if admission.Budgeted && window.active > admission.AllowedInput &&
			!economicFinishOnly {
			finalAdmission := e.economicAdmission(
				turnUsage, totalUsage, 0, 0, 1,
			)
			if finalAdmission.AllowedInput > admission.AllowedInput {
				admission = finalAdmission
				economicFinishOnly = true
				snapshot = project()
				window, err = e.runCompactGate(
					ctx,
					history, snapshot, maxOutputTokens,
					phase, true, gateSend,
					admission.AllowedInput, projectHistory,
				)
				if err != nil {
					return nil, nil, totalUsage, window.estimated, err
				}
			}
		}
		if admission.Budgeted && window.active > admission.AllowedInput {
			return nil, nil, totalUsage, window.estimated,
				contextview.EconomicBudgetError(admission, window.active)
		}
		finishOnly = finishOnly || convergenceOnly || budgetFinishOnly ||
			economicFinishOnly
		if convergenceOnly {
			requestTools = slices.DeleteFunc(
				append([]provider.ToolDefinition(nil), requestTools...),
				func(definition provider.ToolDefinition) bool {
					return !tool.ConvergenceDefinitionAllowed(definition)
				})
			reasoningEffort = finishOnlyReasoningEffort(
				route.Model().Capabilities,
			)
			nativeSearch = false
		}
		snapshot = project()
		snapshot, normalization, normalizationErr := snapshot.Normalize(
			route.Model().Capabilities,
		)
		if normalizationErr != nil {
			return nil, nil, totalUsage, lastEstimate,
				fmt.Errorf("normalize context projection: %w", normalizationErr)
		}
		requestTools = snapshot.Definitions()
		e.recordSampledTools(scope, catalog, requestTools)
		measurement, attributionErr := snapshot.MeasureDetailed(
			sampleReason, reasoningEffort, e.options.TokenEstimator,
		)
		if attributionErr != nil {
			return nil, nil, totalUsage, lastEstimate, attributionErr
		}
		attribution := measurement.Data
		attribution.WorldRevision = worldProjection.Baseline.Revision
		attribution.WorldDigest = worldProjection.Baseline.Digest
		attribution.WorldMode = string(worldProjection.Mode)
		attribution.WorldChangedSections = len(worldProjection.Changed)
		contextview.ApplyEconomicAttribution(&attribution, admission)
		attribution.PairingCalls = normalization.ToolCalls
		attribution.PairingResults = normalization.ToolResults
		attribution.PairingPairs = normalization.PairedCalls
		attribution.PairingDroppedOrphans = normalization.DroppedOrphans
		attribution.PairingVisibleOrphans = normalization.ModelVisibleOrphans
		attribution.ProjectedImages = normalization.ProjectedImages
		attribution.DroppedReasoning = normalization.DroppedReasoning
		e.recordToolSurfaceBudget(scope, attribution, admission)
		lastEstimate = attribution.EstimatedTokens
		windowProjection := e.prepareTokenWindow(&attribution, 0)
		attribution.EconomicRequestedTokens =
			windowProjection.FullActiveTokens
		maxOutputTokens, err = e.checkBudget(
			windowProjection.FullActiveTokens,
			turnUsage,
			totalUsage,
			maxOutputTokens,
		)
		if err != nil {
			return nil, nil, totalUsage, lastEstimate, err
		}
		routeDigest, propertyDigest := contextview.PrefixRequestIdentity(
			route, maxOutputTokens, reasoningEffort, nativeSearch,
		)
		prefixManifest, prefixErr := contextview.BuildPrefixManifestFromMeasurement(
			snapshot,
			measurement,
			routeDigest,
			propertyDigest,
		)
		if prefixErr != nil {
			return nil, nil, totalUsage, lastEstimate, prefixErr
		}
		e.prefixMu.Lock()
		previousPrefix := e.prefixManifest
		e.prefixMu.Unlock()
		contextview.ApplyPrefixAttribution(
			&attribution, previousPrefix, prefixManifest,
		)
		windowProjection = e.prepareTokenWindow(
			&attribution,
			maxOutputTokens,
		)
		shrinkThroughput := func() (uint64, bool) {
			next, ok := e.foldWorkingSetForThroughput(
				history, projectHistory, snapshot, maxOutputTokens,
				phase, gateSend,
			)
			if ok {
				snapshot = project()
			}
			return next, ok
		}
		if err := e.admitProviderThroughput(
			ctx,
			route,
			windowProjection.FullActiveTokens+maxOutputTokens,
			&rateLimitWaited,
			shrinkThroughput,
		); err != nil {
			return nil, nil, totalUsage, lastEstimate, err
		}
		messages := snapshot.Messages()
		if beginAttempt != nil {
			if err := beginAttempt(); err != nil {
				return nil, nil, totalUsage, lastEstimate, err
			}
		}
		sampleLease, holdErr := e.holdProviderSample(ctx)
		if holdErr != nil {
			return nil, nil, totalUsage, lastEstimate, holdErr
		}
		// Metadata sampling may have settled usage while this attempt queued.
		maxOutputTokens, err = e.checkBudget(
			windowProjection.FullActiveTokens, turnUsage, totalUsage, maxOutputTokens,
		)
		if err != nil {
			sampleLease.Release()
			return nil, nil, totalUsage, lastEstimate, err
		}
		providerAttempt++
		attemptStarted := time.Now()
		if err := send(CallingModel, Event{
			ModelExecution: &ModelExecution{
				Kind: "provider_attempt", SampleID: sampleID,
				Attempt: providerAttempt, Status: protocol.ProviderAttemptStarted,
				Reason:               sampleReason,
				ProjectedInputTokens: windowProjection.FullActiveTokens,
				StartedAt:            attemptStarted,
			},
		}); err != nil {
			sampleLease.Release()
			return nil, nil, totalUsage, lastEstimate, err
		}
		var lastPublishedUsage *provider.Usage
		call := sample{
			index: e.nextSample(), provider: route.ProviderID(),
			model: route.Model().ID, pricing: route.Model().Pricing, context: &attribution,
			observe: e.observeTokenWindow,
		}
		transport, err := providerassembly.RunTransportAttempt(
			ctx,
			e.options.Provider,
			provider.ModelRequest{
				Route: route, Messages: messages,
				LogicalRequestID: sampleID,
				TransportAttempt: assembly.NextTransportAttempt(),
				Projection: provider.ProjectionContext{
					ContextRevision: attribution.ContextRevision,
					WindowID:        attribution.WindowID,
					WindowNumber:    attribution.WindowNumber,
					Retry: providerRetries > 0 || rateLimitRetries > 0 ||
						assembly.TransportCount() > 0,
					RecoveryID: providerassembly.ProjectionRecoveryID(
						scope.spec.Request.Recovery,
					),
				},
				MaxOutputTokens: maxOutputTokens, Tools: requestTools,
				ReasoningEffort: reasoningEffort, NativeSearch: nativeSearch,
				Idempotent: true,
				PromptCacheKey: provider.StickyPromptCacheKey(
					e.options.PromptCacheKey,
					route,
				),
			},
			assembly,
			providerassembly.ConsumeConfig{
				FirstOutput: e.tracer().NoteFirstOutput,
				Checkpoint:  checkpoint,
				Project: func(projected providerassembly.StreamProjection) error {
					if projected.Usage != nil {
						agentcontext.ApplyTransport(
							call.context,
							projected.Transport,
						)
						copy := *projected.Usage
						if lastPublishedUsage != nil &&
							(provider.SameSnapshot(*lastPublishedUsage, copy) ||
								provider.DoubledSnapshot(*lastPublishedUsage, copy)) {
							return nil
						}
						lastPublishedUsage = &copy
						if call.observe != nil {
							call.observe(
								call.context,
								copy.InputTokens,
								copy.CachedTokens,
							)
						}
						return send(Streaming, Event{
							Usage: &copy,
							CostUSD: provider.EstimateCost(
								call.pricing,
								copy,
							),
							CostKnown: provider.PricingKnown(
								call.pricing,
								copy,
							),
							Sample: call.index, Provider: call.provider,
							Model: call.model,
							ModelMetadata: modelMetadataProvenance(
								route.Model().MetadataProvenance,
							),
							SampleContext: call.context,
						})
					}
					return send(Streaming, Event{
						Text: projected.Text, Block: projected.Block,
						Search: projected.Search, Citation: projected.Citation,
						Sample: call.index, SampleID: sampleID,
					})
				},
			},
			providerassembly.TransportLifecycle{
				Activate: e.setActiveCancel,
				Clear:    e.clearActiveCancel,
				Begin: func(callCtx context.Context) (
					context.Context,
					func(error),
				) {
					span := e.tracer().Start(
						trace.NameModelCall,
						0,
						map[string]any{
							"provider": call.provider,
							"model":    call.model,
							"sample":   call.index,
							"attempt":  attempt + 1,
						},
					)
					return e.tracer().Context(callCtx, span.ID()), func(err error) {
						if err != nil {
							span.Set("error", errorText(err))
							span.End(trace.StatusError)
							return
						}
						span.End(trace.StatusOK)
					}
				},
			},
		)
		if err != nil && !transport.Opened {
			if sendErr := send(CallingModel, Event{ModelExecution: &ModelExecution{
				Kind: "provider_attempt", SampleID: sampleID, Attempt: providerAttempt,
				Status:               protocol.ProviderAttemptFailed,
				ProjectedInputTokens: windowProjection.FullActiveTokens,
				StartedAt:            attemptStarted, FinishedAt: time.Now(),
			}}); sendErr != nil {
				sampleLease.Release()
				return nil, nil, totalUsage, lastEstimate, sendErr
			}
			if errors.Is(err, context.Canceled) && ctx.Err() == nil && e.appendSteering(history) {
				sampleLease.Release()
				attempt = -1
				continue
			}
			contextChanged, recoveryErr := e.recoverContextOverflow(
				err,
				false,
				history,
				snapshot,
				maxOutputTokens,
				send,
			)
			if recoveryErr != nil {
				sampleLease.Release()
				return nil, nil, totalUsage, lastEstimate, recoveryErr
			}
			retry, retryable := e.providerRetry(
				err,
				false,
				providerRetries,
				contextChanged,
				rateLimitBudget{
					retries:  rateLimitRetries,
					waited:   rateLimitWaited,
					cooldown: e.routeCooldown(route),
				},
			)
			if retryable && ctx.Err() == nil {
				if abort := e.abortOversizedRateLimitRetry(
					ctx,
					route,
					windowProjection.FullActiveTokens+maxOutputTokens,
					retry,
					shrinkThroughput,
				); abort != nil {
					sampleLease.Release()
					return nil, nil, totalUsage, lastEstimate, abort
				}
				if retry.Failure.Code == provider.FailureRateLimit {
					sampleLease.NoteRateLimit(retry.EffectiveDelay)
				} else {
					sampleLease.Release()
				}
				if sendErr := send(CallingModel, Event{
					ProviderRetry: &retry,
					ModelExecution: e.providerAttemptRetry(
						sampleID, providerAttempt, attemptStarted,
						windowProjection.FullActiveTokens, assembly,
						rateLimitRetries, rateLimitWaited,
					),
				}); sendErr != nil {
					sampleLease.Release()
					return nil, nil, totalUsage, lastEstimate, sendErr
				}
				if waitErr := waitRetryDelay(
					ctx,
					retry.EffectiveDelay,
				); waitErr != nil {
					return nil, nil, totalUsage, lastEstimate, waitErr
				}
				if retry.Failure.Code == provider.FailureRateLimit {
					rateLimitRetries, rateLimitWaited = e.recordRateLimitWait(
						rateLimitRetries, rateLimitWaited, retry.EffectiveDelay,
					)
				} else {
					providerRetries++
				}
				continue
			}
			e.finishFailedSample(sampleLease, err, false)
			return nil, nil, totalUsage, lastEstimate,
				exhaustedSampleRetry(err, false)
		}
		e.prefixMu.Lock()
		e.prefixManifest = prefixManifest
		e.prefixMu.Unlock()
		consumed := transport.ConsumeResult
		blocks, calls := consumed.Blocks, consumed.Calls
		usage, meaningful := consumed.Usage, consumed.Meaningful
		replay := consumed.Replay
		totalUsage.Add(usage)
		attemptStatus := protocol.ProviderAttemptFailed
		if err == nil {
			attemptStatus = protocol.ProviderAttemptCompleted
		} else {
			var incomplete *providerassembly.IncompleteOutputError
			if errors.As(err, &incomplete) {
				attemptStatus = protocol.ProviderAttemptIncomplete
			}
		}
		attemptExecution := &ModelExecution{
			Kind: "provider_attempt", SampleID: sampleID,
			Attempt: providerAttempt, Status: attemptStatus,
			ProjectedInputTokens: windowProjection.FullActiveTokens,
			StartedAt:            attemptStarted,
			FinishedAt:           time.Now(),
		}
		if len(assembly.Segments) != 0 {
			segment := assembly.Segments[len(assembly.Segments)-1]
			attemptExecution.Transport = segment.Transport
			attemptExecution.StopReason = segment.StopReason
		}
		if attemptStatus == protocol.ProviderAttemptCompleted &&
			attemptExecution.StopReason == "" {
			attemptExecution.StopReason = provider.StopReasonEndTurn
		}
		if sendErr := send(CallingModel, Event{ModelExecution: attemptExecution}); sendErr != nil {
			sampleLease.Release()
			return nil, nil, totalUsage, lastEstimate, sendErr
		}
		pending := e.drainPending()
		if ctx.Err() == nil && len(pending) != 0 {
			if pendingInputInjected != nil {
				*pendingInputInjected = true
			}
			pendingBlocks := providerassembly.AppendBlocks(
				continuedBlocks,
				blocks,
			)
			if len(continuedBlocks) != 0 {
				replay = nil
			}
			if len(pendingBlocks) != 0 {
				*history = append(*history, provider.ProducedAssistant(
					route, pendingBlocks, e.turn, replay,
				))
			}
			e.appendPendingInputs(history, pending)
			continuationMessages = nil
			continuedBlocks = nil
			continuations = 0
			sampleLease.Succeeded()
			if finishTransport != nil {
				if err := finishTransport(); err != nil {
					return nil, nil, totalUsage, lastEstimate, err
				}
			}
			attempt = -1
			continue
		}
		var incomplete *providerassembly.IncompleteOutputError
		if errors.As(err, &incomplete) && ctx.Err() == nil {
			if continued != nil {
				*continued = true
			}
			continuedBlocks = providerassembly.AppendBlocks(
				continuedBlocks,
				blocks,
			)
			if len(blocks) != 0 {
				continuationMessages = append(
					continuationMessages,
					provider.ProducedAssistant(
						route, cloneBlocks(blocks), e.turn, nil,
					),
				)
			}
			continuationMessages = append(
				continuationMessages,
				promptcontext.IncompleteOutputFeedback(
					incomplete.Reason,
					incomplete.ToolFragments,
					e.turn,
				),
			)
			sampleLease.Succeeded()
			if finishTransport != nil {
				if err := finishTransport(); err != nil {
					return nil, nil, totalUsage, lastEstimate, err
				}
			}
			continuations++
			attempt = -1
			continue
		}
		if err == nil {
			if len(continuedBlocks) != 0 {
				replay = nil
			}
			completeBlocks := providerassembly.AppendBlocks(
				continuedBlocks,
				blocks,
			)
			if continued != nil {
				text := strings.TrimSpace(
					providerassembly.BlocksText(completeBlocks),
				)
				*continued = strings.HasSuffix(text, ":") || strings.HasSuffix(text, "：")
			}
			if capturedReplay != nil {
				*capturedReplay = replay
			}
			bindToolCalls(calls, catalog, advertised)
			sampleLease.Succeeded()
			return completeBlocks, calls, totalUsage, lastEstimate, nil
		}
		contextChanged, recoveryErr := e.recoverContextOverflow(
			err,
			meaningful,
			history,
			snapshot,
			maxOutputTokens,
			send,
		)
		if recoveryErr != nil {
			sampleLease.Release()
			return nil, nil, totalUsage, lastEstimate, recoveryErr
		}
		retry, retryable := e.providerRetry(
			err,
			meaningful,
			providerRetries,
			contextChanged,
			rateLimitBudget{
				retries:  rateLimitRetries,
				waited:   rateLimitWaited,
				cooldown: e.routeCooldown(route),
			},
		)
		if !retryable || ctx.Err() != nil {
			if ctx.Err() != nil {
				sampleLease.Release()
				return blocks, nil, totalUsage, lastEstimate, ctx.Err()
			}
			e.finishFailedSample(sampleLease, err, meaningful)
			return blocks, nil, totalUsage, lastEstimate,
				exhaustedSampleRetry(err, meaningful)
		}
		if abort := e.abortOversizedRateLimitRetry(
			ctx,
			route,
			windowProjection.FullActiveTokens+maxOutputTokens,
			retry,
			shrinkThroughput,
		); abort != nil {
			sampleLease.Release()
			return blocks, nil, totalUsage, lastEstimate, abort
		}
		if retry.Failure.Code == provider.FailureRateLimit {
			sampleLease.NoteRateLimit(retry.EffectiveDelay)
		} else {
			sampleLease.Release()
		}
		if sendErr := send(CallingModel, Event{
			ProviderRetry: &retry,
			ModelExecution: e.providerAttemptRetry(
				sampleID, providerAttempt, attemptStarted,
				windowProjection.FullActiveTokens, assembly,
				rateLimitRetries, rateLimitWaited,
			),
		}); sendErr != nil {
			sampleLease.Release()
			return nil, nil, totalUsage, lastEstimate, sendErr
		}
		if waitErr := waitRetryDelay(
			ctx,
			retry.EffectiveDelay,
		); waitErr != nil {
			return nil, nil, totalUsage, lastEstimate, waitErr
		}
		if retry.Failure.Code == provider.FailureRateLimit {
			rateLimitRetries, rateLimitWaited = e.recordRateLimitWait(
				rateLimitRetries, rateLimitWaited, retry.EffectiveDelay,
			)
		} else {
			providerRetries++
		}
	}
}

func (e *Engine) recordRateLimitWait(
	retries uint32,
	waited time.Duration,
	delay time.Duration,
) (uint32, time.Duration) {
	return retries + 1, waited + delay
}

func (e *Engine) finishFailedSample(
	lease *providerSampleLease,
	err error,
	meaningful bool,
) {
	failure := providerwire.ClassifyFailure(err, meaningful)
	if failure.Code == provider.FailureRateLimit {
		lease.NoteRateLimit(time.Duration(failure.RetryAfterMS) * time.Millisecond)
		return
	}
	lease.Release()
}

func exhaustedSampleRetry(err error, meaningful bool) error {
	if providerwire.ClassifyFailure(err, meaningful).Code ==
		provider.FailureRateLimit {
		return exhaustedRateLimitRetry(err)
	}
	return exhaustedProviderRetry(err)
}

func (e *Engine) providerAttemptRetry(
	sampleID string,
	attempt uint32,
	started time.Time,
	projectedInputTokens uint64,
	assembly *providerassembly.ResponseAssembly,
	rateLimitRetries uint32,
	rateLimitWaited time.Duration,
) *ModelExecution {
	execution := &ModelExecution{
		Kind: "provider_attempt", SampleID: sampleID,
		Attempt: attempt, Status: protocol.ProviderAttemptRetryWait,
		ProjectedInputTokens: projectedInputTokens,
		StartedAt:            started,
		RateLimitRetries:     rateLimitRetries,
		RateLimitWaited:      rateLimitWaited,
		RateLimitWaitBudget:  e.options.RateLimitMaxWait,
	}
	if e.options.RateLimitMaxRetries > 0 {
		execution.RateLimitRetryLimit = uint32(e.options.RateLimitMaxRetries)
	}
	if assembly != nil && len(assembly.Segments) != 0 {
		segment := assembly.Segments[len(assembly.Segments)-1]
		execution.Transport = segment.Transport
		execution.StopReason = segment.StopReason
	}
	return execution
}

func bindToolCalls(
	calls []provider.ToolCall,
	catalog tool.CatalogSnapshot,
	advertised map[string]bool,
) {
	for index := range calls {
		binding, known := catalog.Binding(calls[index].Name)
		entry, _ := catalog.Lookup(calls[index].Name)
		unavailable := known &&
			entry.Descriptor.Visibility == tool.VisibleModel &&
			entry.Descriptor.Availability == tool.AvailabilityUnavailable
		if !known || (!advertised[calls[index].Name] && !unavailable) {
			calls[index].CatalogID = catalog.CatalogID
			calls[index].CatalogGeneration = catalog.Generation
			continue
		}
		calls[index].CatalogID = binding.CatalogID
		calls[index].CatalogGeneration = binding.Generation
		calls[index].CatalogRevision = binding.Revision
		calls[index].CatalogAuthority = binding.Authority
	}
}

func deduplicateCompactionReceipts(
	send func(State, Event) error,
) func(State, Event) error {
	var previous *CompactionReceipt
	return func(state State, event Event) error {
		if state == Compacting && event.Compaction != nil {
			current := observableCompactionReceipt(event.Compaction)
			if previous != nil && reflect.DeepEqual(previous, &current) {
				return nil
			}
			previous = &current
		}
		return send(state, event)
	}
}

func observableCompactionReceipt(receipt *CompactionReceipt) CompactionReceipt {
	value := *receipt
	value.OriginalMessages = 0
	value.OriginalTokens = 0
	value.RetainedTokens = 0
	value.SummaryOriginalBytes = 0
	value.SummaryRetainedBytes = 0
	value.TruncationReason = ""
	value.ContextReceipts = nil
	value.WorkingSet = nil
	value.CriticalPaths = nil
	return value
}

func (e *Engine) emitMCPHealthChanges(
	current []MCPHealthSnapshot,
	send func(State, Event) error,
) error {
	scope := e.executionScope()
	if scope == nil {
		return errors.New("turn scope is not active")
	}
	scope.mu.Lock()
	if scope.state.mcpProjected {
		scope.mu.Unlock()
		return nil
	}
	scope.state.mcpProjected = true
	scope.mu.Unlock()
	for _, change := range mcp.ProjectHealth(current) {
		value := change
		if err := send(CallingModel, Event{
			MCPHealthChanged: &value,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) scopeCatalog(scope *Scope) tool.CatalogSnapshot {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.state.catalog.Digest != "" {
		return scope.state.catalog
	}
	return scope.spec.Catalog
}

func (e *Engine) refreshScopeCatalog() error {
	current, err := e.options.Tools.Snapshot()
	if err != nil {
		return err
	}
	scope := e.runningScope()
	if scope == nil {
		return errors.New("turn scope is not active")
	}
	scope.mu.Lock()
	scope.state.catalog = current
	scope.mu.Unlock()
	return nil
}

func (e *Engine) catalogChange(current tool.CatalogSnapshot) *CatalogChanged {
	scope := e.executionScope()
	if scope == nil {
		return nil
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	previous := scope.state.catalogProjected
	if previous.Digest == current.Digest {
		return nil
	}
	scope.state.catalogProjected = current
	diff := tool.DiffCatalog(previous, current)
	changed := &CatalogChanged{
		CatalogID: current.CatalogID, Generation: current.Generation, Digest: current.Digest,
		Added: diff.Added, Replaced: diff.Replaced, Revoked: diff.Revoked,
	}
	return changed
}

// sample attributes one provider call and its usage.
type sample struct {
	index    uint32
	provider string
	model    string
	pricing  model.Pricing
	context  *protocol.SampleContextData
	observe  func(*protocol.SampleContextData, uint64, uint64)
}

func (e *Engine) toolDefinitionsFromSnapshot(
	snapshot tool.CatalogSnapshot,
	request TurnRequest,
) ([]provider.ToolDefinition, map[string]bool, error) {
	return toolsearch.ProjectDefinitions(toolsearch.ProjectionRequest{
		Catalog: snapshot, Prompt: request.Prompt, Intent: request.Intent,
		MaxDefinitions: e.options.MaxToolDefinitions,
		MaxSchemaBytes: e.options.MaxToolSchemaBytes,
		Enabled:        e.toolEnabled,
	})
}

func (e *Engine) recordSampledTools(
	scope *Scope,
	catalog tool.CatalogSnapshot,
	definitions []provider.ToolDefinition,
) {
	if scope == nil {
		return
	}
	advertised := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		advertised[definition.Name] = true
	}
	scope.mu.Lock()
	scope.state.sampledCatalog = catalog
	scope.state.sampledTools = advertised
	scope.mu.Unlock()
}

// maxOutputFor returns the frozen Turn ceiling. Actual input, token, and cost
// budgets may reduce it immediately before the provider call.
func (e *Engine) maxOutputFor(route model.ReadyRoute) uint64 {
	modelLimit := route.Model().Limits.MaxOutputTokens
	configured := e.options.MaxOutputTokens
	if scope := e.runningScope(); scope != nil {
		if ceiling := scope.spec.Limits.Context.OutputCeiling; ceiling != 0 {
			return min(modelLimit, ceiling)
		}
		configured = scope.spec.Limits.MaxOutputTokens
	}
	if configured != 0 {
		return min(configured, modelLimit)
	}
	return modelLimit
}
