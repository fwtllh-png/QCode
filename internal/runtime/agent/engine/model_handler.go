package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
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
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
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

// preparedModelInput keeps the normalized messages and their admission
// accounting together. A visible-tail fold replaces this value as a whole.
type preparedModelInput struct {
	snapshot        agentcontext.MessageSnapshot
	projection      agentcontext.ProjectionResult
	measurement     agentcontext.Measurement
	window          agentcontext.WindowProjection
	maxOutputTokens uint64
}

type modelRetryState struct {
	budget      turnkernel.ProviderRetryBudget
	reserveWait func(time.Duration, time.Time) error
}

func (e *Engine) modelStep(
	ctx context.Context,
	history *[]provider.Message,
	turnUsage provider.Usage,
	sampleID string,
	reason string,
	retries modelRetryState,
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
	providerRetries := retries.budget.TransientRetries
	rateLimitRetries := retries.budget.RateLimitRetries
	rateLimitWaited := retries.budget.RateLimitWaited
	if err := waitRetryDelay(ctx, time.Until(retries.budget.WaitUntil)); err != nil {
		return nil, nil, provider.Usage{}, 0, err
	}
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
				*continued = assembly.CurrentStopReason().Incomplete()
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
		feedback := promptcontext.IncompleteOutputFeedback(
			provider.StopReasonIncomplete,
			assembly.IncompleteToolFragments(),
			e.turn,
		)
		if assembly.ToolArgumentsRejected() {
			feedback = promptcontext.ToolArgumentRepairFeedback(e.turn)
		}
		continuationMessages = append(continuationMessages, feedback)
		continuations = uint32(assembly.TransportCount())
		if continued != nil {
			*continued = true
		}
	}
	baseReasoningEffort := e.reasoningEffort()
	for attempt := 0; ; attempt++ {
		var sampleFeedback []provider.Message
		turnReceipts := append([]promptcontext.Receipt(nil), worldReceipts...)
		e.recordTurnContextReceipts(turnReceipts)
		route := e.activeRoute()
		maxOutputTokens := e.maxOutputFor(route)
		budgetMessage, budgetFinishOnly := e.budgetConvergence(
			turnUsage.Total() + totalUsage.Total(),
		)
		if len(budgetMessage.Blocks) != 0 {
			sampleFeedback = append(sampleFeedback, budgetMessage)
		}
		feedback := append(slices.Clone(sampleFeedback), continuationMessages...)
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
		if assembly.ToolArgumentsRejected() {
			sampleReason = promptcontext.SampleToolArgumentRepair
		}
		statelessProjector := contextview.NewStatelessProjector(
			route.Model().Capabilities.IncrementalResponses,
		)
		admission := e.economicAdmission(
			turnUsage, totalUsage, maxOutputTokens, maxOutputTokens, remainingCalls,
		)
		economicFinishOnly := false
		if admission.Budgeted && admission.AllowedInput == 0 {
			admission = e.economicAdmission(turnUsage, totalUsage, 0, 0, 1)
			economicFinishOnly = true
		}
		var selection agentcontext.ProjectionResult
		var selectionError error
		var unavailableTurns map[uint64]bool
		// Once directory recovery is needed, keep that decision for this sample.
		// Repeated compaction projections must not oscillate as schemas shrink.
		recoveryOnly := false
		fitsRequest := func(snapshot agentcontext.MessageSnapshot) (bool, error) {
			normalized, _, err := snapshot.Normalize(route.Model().Capabilities)
			if err != nil {
				return false, err
			}
			measurement, err := normalized.MeasureDetailed(sampleReason, reasoningEffort, e.options.TokenEstimator)
			if err != nil {
				return false, err
			}
			window := e.prepareTokenWindow(&measurement.Data, maxOutputTokens)
			return window.FullActiveTokens <= admission.AllowedInput &&
				window.FullActiveTokens <= window.HardLimit-min(window.HardLimit, maxOutputTokens), nil
		}
		projectHistory := func(history []provider.Message) []provider.Message {
			selectionError = nil
			plan := e.currentPlan()
			conversation, unavailable, omissions, err := e.availableConversation(ctx, e.contextAuthority().Conversation(), plan)
			if err != nil {
				selectionError = err
				return nil
			}
			unavailableTurns = unavailable
			baseSelection := contextview.ExcludeUnavailableTurns(e.contextProjection(history), unavailable, e.estimateMessageTokens)
			original := statelessProjector.Project(baseSelection.Messages)
			render := func(directory bool) []provider.Message {
				selection = baseSelection
				selection.References = slices.Clone(baseSelection.References)
				state := conversation
				// These are request-local background facts, not a new message
				// after the latest tool result. Rebuild from the current selection
				// on every projection, without writing them into durable history.
				var background []provider.Message
				if directory {
					state = agentcontext.CloneConversation(conversation)
					state.Selection = &agentcontext.ConversationSelection{}
					background = append(background, promptcontext.ConversationCatalogHint(len(conversation.CandidateSources(plan)), e.sessionStateBudget()))
				}
				excerpts := contextview.SelectConversation(state, plan, original)
				for _, excerpt := range excerpts {
					selection.References = append(selection.References, excerpt.Coverage)
				}
				if references := promptcontext.ConversationReferences(excerpts); references != nil {
					background = append(background, *references)
				}
				selection.Representations = contextview.ConversationOmissions(conversation, selection.References, directory)
				selection.Representations = append(selection.Representations, omissions...)
				selection.Seal()
				hint, err := promptcontext.ContextSelectionHint(selection, e.sessionStateBudget())
				if err != nil {
					selectionError = err
				}
				if hint != nil {
					background = append(background, *hint)
				}
				return contextview.WithTurnBackground(original, background, e.turn)
			}
			messages := render(recoveryOnly)
			if !recoveryOnly && conversation != nil && conversation.Selection == nil && len(selection.References) != 0 {
				candidate := contextLedger.Project(agentcontext.LedgerProjection{
					Stable: stableContext, History: messages,
					DynamicBeforeTurn: e.turn,
					Continuation:      feedback, Definitions: requestTools,
				})
				fits, err := fitsRequest(candidate)
				if err != nil {
					selectionError = err
				} else if !fits {
					recoveryOnly = true
					requestTools = slices.DeleteFunc(slices.Clone(requestTools), func(definition provider.ToolDefinition) bool {
						return !conversationRecoveryTool(definition.Name)
					})
					nativeSearch = false
					messages = render(true)
				}
			}
			return messages
		}
		project := func() agentcontext.MessageSnapshot {
			base := contextLedger.Project(agentcontext.LedgerProjection{
				Stable: stableContext, History: projectHistory(*history),
				DynamicBeforeTurn: e.turn,
				Continuation:      feedback, Definitions: requestTools,
			})
			e.checkpointMu.Lock()
			checkpoints := agentcontext.CloneTurnCheckpoints(e.turnCheckpoints)
			e.checkpointMu.Unlock()
			checkpoints = slices.DeleteFunc(checkpoints, func(checkpoint agentcontext.TurnCheckpoint) bool { return unavailableTurns[checkpoint.Turn] })
			selected, err := contextview.SelectCheckpoints(checkpoints, selection.References, e.options.Context.CheckpointMaxBytes, func(candidate []provider.Message) (bool, error) {
				return fitsRequest(base.WithDynamic(candidate))
			})
			if err != nil {
				selectionError = err
				return base
			}
			dynamic := slices.Clone(selected)
			summaryRoute, _ := e.SummaryRouteDigest()
			narrative, observations, err := contextview.SelectNarrative(
				e.contextAuthority().Compaction().Digest, e.options.Context.Digest == "ledger+narrative",
				summaryRoute, e.currentWindowLedger().ID, *history, selection, time.Now().UTC(),
				func(message provider.Message) (bool, error) {
					return fitsRequest(base.WithDynamic(append(slices.Clone(dynamic), message)))
				})
			if err != nil {
				selectionError = err
				return base
			}
			selection.Representations = append(selection.Representations, observations...)
			if narrative != nil {
				dynamic = append(dynamic, *narrative)
			}
			return contextLedger.Project(agentcontext.LedgerProjection{
				Stable: stableContext, History: base.Partition(agentcontext.KindHistory),
				Dynamic:           dynamic,
				DynamicBeforeTurn: e.turn,
				Continuation:      feedback, Definitions: requestTools,
			})
		}
		snapshot := project()
		if selectionError != nil {
			return nil, nil, totalUsage, lastEstimate, selectionError
		}
		phase := CompactionPhaseMidTurn
		if turnUsage.InputTokens+totalUsage.InputTokens == 0 {
			phase = CompactionPhasePreSampling
		}
		gateSend := deduplicateCompactionReceipts(send)
		economicInput := func() uint64 {
			if admission.Budgeted {
				return admission.AllowedInput
			}
			return 0
		}
		runGate := func(input agentcontext.MessageSnapshot) (tokenWindow, error) {
			window, gateErr := e.runCompactGate(ctx, history, input, maxOutputTokens,
				phase, true, gateSend, economicInput(), projectHistory)
			if protocol.CodeOf(gateErr) != protocol.CodeResourceExhausted ||
				window.hardLimit == 0 || window.total <= window.hardLimit || len(continuationMessages) == 0 {
				return window, gateErr
			}
			originalFeedback := feedback
			compacted, changed, compactErr := e.compactModelContinuation(continuationMessages, window, func(candidate []provider.Message) (tokenWindow, error) {
				feedback = append(slices.Clone(sampleFeedback), candidate...)
				candidateInput := project()
				if selectionError != nil {
					return tokenWindow{}, selectionError
				}
				return e.measureTokenWindow(candidateInput, maxOutputTokens, economicInput())
			})
			feedback = originalFeedback
			if compactErr != nil {
				return window, compactErr
			}
			if !changed {
				return window, gateErr
			}
			continuationMessages = compacted
			feedback = append(slices.Clone(sampleFeedback), compacted...)
			e.advanceTokenWindow()
			nextInput := project()
			if selectionError != nil {
				return window, selectionError
			}
			next, err := e.measureTokenWindow(nextInput, maxOutputTokens, economicInput())
			if err != nil {
				return window, err
			}
			if err := gateSend(Compacting, Event{Compaction: &CompactionReceipt{
				Status: "pruned", Mode: "surface", Phase: phase,
				OriginalTokens: window.total, RetainedTokens: next.total,
				TruncationReason: "provider_continuation",
			}}); err != nil {
				return window, err
			}
			return e.runCompactGate(ctx, history, nextInput, maxOutputTokens,
				phase, true, gateSend, economicInput(), projectHistory)
		}
		window, err := runGate(snapshot)
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
				window, err = runGate(snapshot)
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
		prepareInput := func() (preparedModelInput, error) {
			var previousDigest string
			var previousTokens uint64
			for {
				candidate := project()
				if selectionError != nil {
					return preparedModelInput{}, selectionError
				}
				normalized, normalization, err := candidate.Normalize(route.Model().Capabilities)
				if err != nil {
					return preparedModelInput{}, fmt.Errorf("normalize context projection: %w", err)
				}
				measurement, err := normalized.MeasureDetailed(
					sampleReason, reasoningEffort, e.options.TokenEstimator,
				)
				if err != nil {
					return preparedModelInput{}, err
				}
				attribution := &measurement.Data
				attribution.WorldRevision = worldProjection.Baseline.Revision
				attribution.WorldDigest = worldProjection.Baseline.Digest
				attribution.WorldMode = string(worldProjection.Mode)
				attribution.WorldChangedSections = len(worldProjection.Changed)
				contextview.ApplyEconomicAttribution(attribution, admission)
				attribution.PairingCalls = normalization.ToolCalls
				attribution.PairingResults = normalization.ToolResults
				attribution.PairingPairs = normalization.PairedCalls
				attribution.PairingDroppedOrphans = normalization.DroppedOrphans
				attribution.PairingVisibleOrphans = normalization.ModelVisibleOrphans
				attribution.ProjectedImages = normalization.ProjectedImages
				attribution.DroppedReasoning = normalization.DroppedReasoning
				window := e.prepareTokenWindow(attribution, 0)
				// Admission uses the complete normalized request, including schemas,
				// dynamic/continuation partitions and the freshly rendered omissions.
				// Every retry must reduce that cost, not just the raw tail estimate.
				if previousDigest != "" && (attribution.ContextDigest == previousDigest ||
					window.FullActiveTokens >= previousTokens) {
					if admission.Budgeted && window.FullActiveTokens > admission.AllowedInput {
						return preparedModelInput{}, contextview.EconomicBudgetError(admission, window.FullActiveTokens)
					}
					return preparedModelInput{}, compactionBudgetError(tokenWindow{
						total: window.FullActiveTokens + maxOutputTokens, hardLimit: window.HardLimit,
					})
				}
				if window.FullActiveTokens+maxOutputTokens > window.HardLimit ||
					admission.Budgeted && window.FullActiveTokens > admission.AllowedInput {
					previousDigest, previousTokens = attribution.ContextDigest, window.FullActiveTokens
					if _, err := runGate(normalized); err != nil {
						return preparedModelInput{}, err
					}
					continue
				}
				attribution.EconomicRequestedTokens = window.FullActiveTokens
				outputReserve, err := e.checkBudget(
					window.FullActiveTokens, turnUsage, totalUsage, maxOutputTokens,
				)
				if err != nil {
					return preparedModelInput{}, err
				}
				window = e.prepareTokenWindow(attribution, outputReserve)
				selection.SourceWindowID, selection.SourceWindowNumber = window.ID, window.Number
				selection.RouteDigest, _ = contextview.PrefixRequestIdentity(route, outputReserve, reasoningEffort, nativeSearch)
				selection.ContextRevision, selection.ContextDigest = attribution.ContextRevision, attribution.ContextDigest
				selection.InputTokens, selection.OutputReserve = window.FullActiveTokens, outputReserve
				selection.Seal()
				attribution.ContextProjectionDigest = selection.Digest
				return preparedModelInput{
					snapshot: normalized, projection: selection, measurement: measurement,
					window: window, maxOutputTokens: outputReserve,
				}, nil
			}
		}
		prepared, err := prepareInput()
		if err != nil {
			return nil, nil, totalUsage, lastEstimate, err
		}
		lastEstimate = prepared.measurement.Data.EstimatedTokens
		shrinkThroughput := func() (uint64, bool, error) {
			_, folded, err := e.foldWorkingSetForThroughput(
				history, projectHistory, prepared.snapshot, prepared.maxOutputTokens,
				phase, gateSend,
			)
			if err != nil || !folded {
				return 0, false, err
			}
			next, err := prepareInput()
			if err != nil {
				return 0, false, err
			}
			prepared = next
			lastEstimate = prepared.measurement.Data.EstimatedTokens
			return prepared.window.FullActiveTokens + prepared.maxOutputTokens, true, nil
		}
		if err := e.admitProviderThroughput(
			ctx,
			route,
			prepared.window.FullActiveTokens+prepared.maxOutputTokens,
			&rateLimitWaited,
			shrinkThroughput,
			retries.reserveWait,
		); err != nil {
			return nil, nil, totalUsage, lastEstimate, err
		}
		snapshot = prepared.snapshot
		attribution := prepared.measurement.Data
		windowProjection := prepared.window
		maxOutputTokens = prepared.maxOutputTokens
		requestTools = snapshot.Definitions()
		routeDigest, propertyDigest := contextview.PrefixRequestIdentity(
			route, maxOutputTokens, reasoningEffort, nativeSearch,
		)
		prefixManifest, prefixErr := contextview.BuildPrefixManifestFromMeasurement(
			snapshot,
			prepared.measurement,
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
		messages := snapshot.Messages()
		if beginAttempt != nil {
			if err := beginAttempt(); err != nil {
				return nil, nil, totalUsage, lastEstimate, err
			}
		}
		if assembly.ToolArgumentsRejected() {
			if repairErr := assembly.AuthorizeToolArgumentRepair(e.options.MaxRetries); repairErr != nil {
				return assembly.ConfirmedBlocks(), nil, totalUsage, lastEstimate,
					exhaustedToolArgumentRepair(repairErr, len(assembly.ToolArgumentRepairs), e.options.MaxRetries)
			}
			// Persist the authorization before opening a transport. A restart
			// can reuse it but cannot reset the regeneration budget.
			if checkpoint != nil {
				if err := checkpoint(assembly); err != nil {
					return nil, nil, totalUsage, lastEstimate, err
				}
			}
		}
		sampleLease, holdErr := e.holdProviderSample(ctx)
		if holdErr != nil {
			return nil, nil, totalUsage, lastEstimate, holdErr
		}
		// Metadata sampling may have settled usage while this attempt queued.
		currentUsage := turnUsage
		currentUsage.Add(totalUsage)
		e.syncSessionTitleState(currentUsage)
		var settleBudget func(provider.Usage)
		maxOutputTokens, settleBudget, err = e.reserveModelBudget(e.options, route,
			windowProjection.FullActiveTokens, maxOutputTokens, scope.spec.Identity.TurnID, false)
		if err != nil {
			sampleLease.Release()
			return nil, nil, totalUsage, lastEstimate, err
		}
		if maxOutputTokens != windowProjection.OutputReserve {
			_, prefixManifest.PropertyDigest = contextview.PrefixRequestIdentity(
				route, maxOutputTokens, reasoningEffort, nativeSearch,
			)
			contextview.ApplyPrefixAttribution(&attribution, previousPrefix, prefixManifest)
			windowProjection = e.prepareTokenWindow(&attribution, maxOutputTokens)
			prepared.projection.OutputReserve = maxOutputTokens
			prepared.projection.Seal()
			attribution.ContextProjectionDigest = prepared.projection.Digest
		}
		e.recordSampledTools(scope, catalog, requestTools)
		scope.mu.Lock()
		scope.state.referenceRecoveryOnly = recoveryOnly
		scope.mu.Unlock()
		e.recordToolSurfaceBudget(scope, attribution, admission)
		providerAttempt++
		attemptStarted := time.Now()
		if err := send(CallingModel, Event{
			ContextProjection: contextProjectionReceipt(prepared.projection, recoveryOnly),
			InputContext:      &attribution,
			ModelExecution: &ModelExecution{
				Kind: "provider_attempt", SampleID: sampleID,
				Attempt: providerAttempt, Status: protocol.ProviderAttemptStarted,
				Reason:               sampleReason,
				ProjectedInputTokens: windowProjection.FullActiveTokens,
				StartedAt:            attemptStarted,
			},
		}); err != nil {
			settleBudget(provider.Usage{})
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
					RecoveryID: projectionRecoveryID(
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
				Activate: e.setSampleCancel,
				Clear:    e.clearSampleCancel,
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
		settleBudget(transport.ConsumeResult.Usage)
		blocks, meaningful := transport.Blocks, transport.Meaningful
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
				if pendingInputInjected != nil {
					*pendingInputInjected = true
				}
				sampleLease.Release()
				if finishTransport != nil {
					if finishErr := finishTransport(); finishErr != nil {
						return nil, nil, totalUsage, lastEstimate, finishErr
					}
				}
				attempt = -1
				continue
			}
		} else {
			e.prefixMu.Lock()
			e.prefixManifest = prefixManifest
			e.prefixMu.Unlock()
			consumed := transport.ConsumeResult
			calls := consumed.Calls
			replay := consumed.Replay
			totalUsage.Add(consumed.Usage)
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
			if err != nil && assembly.ToolArgumentsRejected() && ctx.Err() == nil {
				continuedBlocks = assembly.ConfirmedBlocks()
				continuationMessages = nil
				if len(continuedBlocks) != 0 {
					continuationMessages = append(continuationMessages, provider.ProducedAssistant(
						route, cloneBlocks(continuedBlocks), e.turn, nil,
					))
				}
				continuationMessages = append(continuationMessages, promptcontext.ToolArgumentRepairFeedback(e.turn))
				sampleLease.Release()
				if finishTransport != nil {
					if err := finishTransport(); err != nil {
						return nil, nil, totalUsage, lastEstimate, err
					}
				}
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
					// Length and provider stop_reason are the continuation
					// evidence. A finished end_turn that happens to end with
					// ":" is a complete draft, not an unfinished sample.
					*continued = assembly.CurrentStopReason().Incomplete()
				}
				if capturedReplay != nil {
					*capturedReplay = replay
				}
				bindToolCalls(calls, catalog, advertised)
				sampleLease.Succeeded()
				return completeBlocks, calls, totalUsage, lastEstimate, nil
			}
		}
		// Both transport-open and stream-consumption failures share this retry
		// transition after their output and lifecycle handling has completed.
		contextChanged, recoveryErr := e.recoverContextOverflow(
			err,
			meaningful,
			history,
			snapshot,
			maxOutputTokens,
			send,
			projectHistory,
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
			sampleID,
		)
		if !retryable || ctx.Err() != nil {
			// An opened stream returns its captured output with the context
			// error; a failed open retains the provider failure classification.
			if transport.Opened && ctx.Err() != nil {
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
		if retry.Failure.Code == provider.FailureRateLimit {
			rateLimitRetries++
			rateLimitWaited += retry.EffectiveDelay
		} else {
			providerRetries++
		}
		if waitErr := waitRetryDelay(ctx, retry.EffectiveDelay); waitErr != nil {
			return nil, nil, totalUsage, lastEstimate, waitErr
		}
	}
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

func projectionRecoveryID(
	recovery *protocol.TurnRecoveryContext,
) string {
	if recovery == nil {
		return ""
	}
	return string(recovery.Action) + "\x00" + string(recovery.SourceTurnID)
}
