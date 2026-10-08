package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/google/uuid"
)

type NarrativeUsage struct {
	ID              string
	Attempt         uint32
	Usage           provider.Usage
	Provider, Model string
	ModelMetadata   protocol.ModelMetadataProvenance
	CostUSD         float64
	CostKnown       bool
}

type NarrativeGenerationResult struct {
	Artifact        agentcontext.NarrativeArtifact
	Usage           provider.Usage
	Provider, Model string
	ModelMetadata   protocol.ModelMetadataProvenance
	CostUSD         float64
	CostKnown       bool
	Attempt         uint32
	Calls           []NarrativeUsage
	RouteDigest     string
	Fallback        bool
	FailureReason   string
	Receipt         *CompactionReceipt
	DurationMS      int64
	Background      bool
}

func (e *Engine) SummaryRouteDigest() (string, error) {
	return agentcontext.SummaryRouteDigest(e.options.Routes)
}

// The one deadline and retry pot cover the complete captured source set.
func (e *Engine) generateNarrative(ctx context.Context, options Options, truth agentcontext.TruthCapsule, input agentcontext.NarrativeInputArtifact, turn uint64, focus string, background bool, jobID string) (result NarrativeGenerationResult, err error) {
	started := time.Now()
	defer func() {
		result.DurationMS = time.Since(started).Milliseconds()
		result.Background = background
	}()
	if options.Context.SemanticNarrative == "off" {
		return narrativeFallback("disabled"), nil
	}
	if options.Context.NarrativeTimeout <= 0 {
		return result, errors.New("summary request timeout is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, options.Context.NarrativeTimeout)
	defer cancel()
	if err = input.Validate(time.Now().UTC()); err != nil {
		return result, err
	}
	config := agentcontext.NarrativeGeneratorConfig{Routes: options.Routes, TokenEstimator: options.TokenEstimator, Limits: options.Context.NarrativeLimits, Focus: focus}
	queue := [][]agentcontext.NarrativeExcerpt{input.Excerpts}
	var parts []agentcontext.NarrativeArtifact
	var retries, rateRetries uint32
	var waited time.Duration
	for len(queue) > 0 {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		excerpts := queue[0]
		queue = queue[1:]
		part := agentcontext.NarrativeInputPart(input, excerpts)
		part.RequiredKinds = nil
		part = agentcontext.NarrativeInputPart(part, excerpts)
		request, bytes, prepareErr := agentcontext.PrepareNarrativeRequest(config, truth, part)
		if prepareErr != nil {
			if !errors.Is(prepareErr, agentcontext.ErrNarrativeInputBudget) {
				return result, prepareErr
			}
			// Subdivide structurally first, then UTF-8 ranges. No prefix is lost.
			if len(excerpts) > 1 {
				middle := len(excerpts) / 2
				queue = append([][]agentcontext.NarrativeExcerpt{excerpts[:middle], excerpts[middle:]}, queue...)
				continue
			}
			if len(excerpts) == 0 {
				return result, prepareErr
			}
			split, splitErr := agentcontext.SplitNarrativeExcerpt(excerpts[0])
			if splitErr != nil {
				return result, fmt.Errorf("%w: %v", prepareErr, splitErr)
			}
			queue = append([][]agentcontext.NarrativeExcerpt{split[:1], split[1:]}, queue...)
			continue
		}
		request.LogicalRequestID = "narrative:" + jobID + ":" + part.Digest
		for {
			call, callErr := e.callNarrative(ctx, options, request, part, turn, bytes, background)
			if call.Attempt != 0 {
				result.Attempt++
				result.Calls = append(result.Calls, NarrativeUsage{ID: request.LogicalRequestID, Attempt: result.Attempt, Usage: call.Usage, Provider: call.Provider, Model: call.Model, ModelMetadata: call.ModelMetadata, CostUSD: call.CostUSD, CostKnown: call.CostKnown})
			}
			result.Usage.Add(call.Usage)
			result.CostUSD += call.CostUSD
			result.Provider, result.Model, result.ModelMetadata = call.Provider, call.Model, call.ModelMetadata
			result.RouteDigest = call.RouteDigest
			if call.Attempt != 0 {
				if result.Attempt == 1 {
					result.CostKnown = call.CostKnown
				} else {
					result.CostKnown = result.CostKnown && call.CostKnown
				}
			}
			if callErr == nil {
				parts = append(parts, call.Artifact)
				break
			}
			if ctx.Err() != nil {
				return result, callErr
			}
			limit := max(options.MaxRetries, 0)
			if options.Context.NarrativeRetryLimit > 0 {
				limit = min(limit, options.Context.NarrativeRetryLimit)
			}
			retry, ok := providerwire.RetryPolicy{MaxRetries: limit, MaxDelay: options.MaxRetryDelay, RateLimitMaxRetries: options.RateLimitMaxRetries, RateLimitMaxWait: options.Context.NarrativeTimeout, RateLimitRetries: rateRetries, RateLimitWaited: waited, Now: options.Observability.Now}.Decide(callErr, false, retries, false)
			if !ok {
				return result, callErr
			}
			if retry.Failure.Code == provider.FailureRateLimit {
				rateRetries++
				waited += retry.EffectiveDelay
				options.SharedRateLimit.Record(retry.EffectiveDelay)
			} else {
				retries++
			}
			if err = waitRetryDelay(ctx, retry.EffectiveDelay); err != nil {
				return result, err
			}
		}
	}
	result.Artifact, err = agentcontext.MergeNarrativeParts(input, parts, time.Now().UTC())
	return result, err
}

type narrativeSnapshot struct {
	sessionID          string
	threadID           protocol.ThreadID
	turnID             protocol.TurnID
	epoch, createdTurn uint64
	windowID           string
	truth              agentcontext.TruthCapsule
	input              agentcontext.NarrativeInputArtifact
	options            Options
	permission         string
	keys               []string
	jobID              string
	detached           bool
	replacements       []agentcontext.ConversationReplacement
	sourceTurns        []protocol.TurnID
}

type PostTurnNarrativeRunner interface {
	Run(context.Context) (NarrativeGenerationResult, error)
}

type PostTurnNarrative struct {
	engine   *Engine
	snapshot narrativeSnapshot
	ctx      context.Context
	cancel   context.CancelFunc
	once     sync.Once
	result   NarrativeGenerationResult
	err      error
	// ready/candidate are protected by engine.narrativeMu.
	ready     bool
	candidate NarrativeGenerationResult
	// Configured before Run. Notifications contain no billable Calls: usage
	// is published once by the runner owner, independently of installation.
	observer func(NarrativeGenerationResult)
}

func (p *PostTurnNarrative) Observe(observer func(NarrativeGenerationResult)) {
	p.observer = observer
	if observer != nil {
		observer(NarrativeGenerationResult{Receipt: &CompactionReceipt{
			CompactionID: p.snapshot.jobID, Status: "started", Mode: "post_turn",
			Phase: CompactionPhasePostTurn, NarrativeBackground: true,
		}})
	}
}

func (p *PostTurnNarrative) notify(result NarrativeGenerationResult) {
	if p.observer != nil {
		result.Calls = nil
		p.observer(result)
	}
}

func (p *PostTurnNarrative) Run(ctx context.Context) (NarrativeGenerationResult, error) {
	p.once.Do(func() {
		callCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(p.ctx, cancel)
		defer cancel()
		defer stop()
		if p.ctx.Err() != nil {
			cancel()
		}
		p.result, p.err = p.engine.generateNarrative(callCtx, p.snapshot.options, p.snapshot.truth, p.snapshot.input, p.snapshot.createdTurn, "", true, p.snapshot.jobID)
		if p.err != nil {
			p.result.Fallback = true
			p.result.FailureReason = p.err.Error()
			p.err = nil
		}
		p.result.Receipt = &CompactionReceipt{
			CompactionID: p.snapshot.jobID, Status: "prepared", Mode: "post_turn",
			Phase: CompactionPhasePostTurn, FallbackReason: "awaiting_safe_boundary",
		}
		if p.result.Fallback {
			p.result.Receipt.Status = "fallback"
			p.result.Receipt.FallbackReason = p.result.FailureReason
		}
		annotateNarrativeReceipt(&p.result)
		p.notify(p.result)
		p.engine.narrativeMu.Lock()
		p.candidate = p.result
		p.ready = true
		p.engine.narrativeMu.Unlock()
		// Never wait for the foreground lock. Busy engines consume this mailbox
		// at the next turn boundary; the frozen sample remains untouched.
		if p.engine.mu.TryLock() {
			installed := p.engine.installPendingNarrative(context.WithoutCancel(ctx))
			if installed.Receipt != nil {
				p.result = installed
			}
			p.engine.mu.Unlock()
		}
	})
	return p.result, p.err
}

func narrativeKey(route string, source agentcontext.NarrativeSourceRange) string {
	return fmt.Sprintf("p3:%s:%s:%s", route, source.MessageID, source.Digest)
}

func narrativePermission(options Options) string {
	var frozen struct {
		Profile, Revision                        uint64
		Mode                                     policy.Mode
		Permission                               policy.Permission
		Granular                                 policy.Granular
		Grants, User, Repository, Constitution   []policy.Rule
		DisableAutoReview, ForceEditPlanApproval bool
	}
	frozen.Profile = options.ProfileRevision
	if options.Security != nil {
		s := options.Security.CloneSampling()
		frozen.Revision = s.Revision
		frozen.Mode = s.Mode
		frozen.Permission = s.Permission
		frozen.Granular = s.Granular
		frozen.Grants = s.Grants
		frozen.User = s.User
		frozen.Repository = s.Repository
		frozen.Constitution = s.Constitution
		frozen.DisableAutoReview = s.DisableAutoReview
		frozen.ForceEditPlanApproval = s.ForceEditPlanApproval
	}
	raw, err := json.Marshal(frozen)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func (e *Engine) captureNarrativeSnapshot(ctx context.Context, thread protocol.ThreadID, turn protocol.TurnID, source []provider.Message, explicit bool) (narrativeSnapshot, error) {
	sessionID := e.sessionIdentity(ctx)
	if owner := e.options.SessionForTurn; owner != nil {
		var found bool
		sessionID, found = owner(ctx, string(turn))
		if !found || sessionID == "" {
			return narrativeSnapshot{}, errors.New("narrative source turn session is unavailable")
		}
	}
	if source == nil {
		source = e.history
	}
	var omitted []provider.Message
	candidates := source[:e.contextProjection(source).TailStart]
	if explicit {
		// A forced compaction has already replaced history. Its removed input
		// is the actual before/after difference, even in capacity mode.
		retained := make(map[string]bool)
		for _, message := range e.history {
			retained[provider.MessageContentDigest(message)] = true
		}
		candidates = nil
		for _, message := range source {
			if !retained[provider.MessageContentDigest(message)] {
				candidates = append(candidates, message)
			}
		}
	}
	for _, message := range candidates {
		if !agentcontext.IsWorldStateMessage(message) {
			omitted = append(omitted, message)
		}
	}
	truth := e.buildTruthCapsule(e.buildCompactSummary(nil), nil)
	authority, err := truth.AuthorityDigest()
	if err != nil {
		return narrativeSnapshot{}, err
	}
	route, err := e.SummaryRouteDigest()
	if err != nil {
		return narrativeSnapshot{}, err
	}
	input, err := agentcontext.BuildNarrativeInput(thread, e.context.Window().ID, authority, route, omitted, e.options.Context.NarrativeLimits, time.Now().UTC(), e.options.Context.NarrativeTimeout)
	if err != nil {
		return narrativeSnapshot{}, err
	}
	attempted := e.context.Compaction().NarrativeAttempts
	var selected []agentcontext.NarrativeExcerpt
	var keys []string
	for _, excerpt := range input.Excerpts {
		rule, _ := json.Marshal(e.options.Context.NarrativeLimits)
		key := narrativeKey(fmt.Sprintf("%s:%x", route, sha256.Sum256(rule)), *excerpt.Source)
		if !explicit && slices.Contains(attempted, key) {
			continue
		}
		selected = append(selected, excerpt)
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	input = agentcontext.NarrativeInputPart(input, selected)
	sourceTurns := []protocol.TurnID{turn}
	for id, number := range e.turnIDs {
		for _, excerpt := range selected {
			if excerpt.Turn == number && !slices.Contains(sourceTurns, protocol.TurnID(id)) {
				sourceTurns = append(sourceTurns, protocol.TurnID(id))
			}
		}
	}
	slices.Sort(sourceTurns)

	return narrativeSnapshot{sessionID: sessionID, threadID: thread, turnID: turn, epoch: e.stateEpoch, createdTurn: e.turn, windowID: e.context.Window().ID, truth: truth, input: input, options: e.options, permission: narrativePermission(e.options), keys: keys, jobID: uuid.NewString(), detached: explicit, replacements: narrativeReplacements(e.context.Conversation(), input), sourceTurns: sourceTurns}, nil
}

func (e *Engine) PreparePostTurnNarrative(thread protocol.ThreadID, turn protocol.TurnID) *PostTurnNarrative {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.installPendingNarrative(context.Background())
	if e.options.Context.SemanticNarrative != "post_turn" || e.options.Context.Digest != "ledger+narrative" {
		return nil
	}
	if status, ok := e.closedTurnSealStatus(); ok && status != agentcontext.CheckpointCompleted {
		return nil
	}
	// Deterministic checkpoint closure happens before handing work to a goroutine.
	e.sealClosedTurnMemory(agentcontext.CheckpointCompleted, nil, "")
	e.narrativeMu.Lock()
	defer e.narrativeMu.Unlock()
	if e.narrativeClosed || e.pendingNarrative != nil {
		return nil
	}
	snapshot, err := e.captureNarrativeSnapshot(context.Background(), thread, turn, nil, false)
	if err != nil || len(snapshot.input.Excerpts) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &PostTurnNarrative{engine: e, snapshot: snapshot, ctx: ctx, cancel: cancel}
	e.pendingNarrative = p
	return p
}

// CancelContextMaintenance is called on close/delete; restore invalidates the
// epoch independently. It never waits for provider completion.
func (e *Engine) CancelContextMaintenance() {
	e.narrativeMu.Lock()
	defer e.narrativeMu.Unlock()
	e.narrativeClosed = true
	if e.pendingNarrative != nil {
		e.pendingNarrative.cancel()
	}
}

func (e *Engine) generatePostTurnDigest(ctx context.Context, thread protocol.ThreadID, turn protocol.TurnID, focus string, source []provider.Message) (NarrativeGenerationResult, error) {
	e.mu.Lock()
	snapshot, err := e.captureNarrativeSnapshot(ctx, thread, turn, source, true)
	e.mu.Unlock()
	if err != nil {
		return narrativeFallback(err.Error()), nil
	}
	if len(snapshot.input.Excerpts) == 0 {
		return narrativeFallback("no_pending_input"), nil
	}
	result, err := e.generateNarrative(ctx, snapshot.options, snapshot.truth, snapshot.input, snapshot.createdTurn, focus, false, snapshot.jobID)
	if err != nil {
		result.Fallback = true
		result.FailureReason = err.Error()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.installNarrative(ctx, snapshot, result), nil
}
func narrativeFallback(reason string) NarrativeGenerationResult {
	return NarrativeGenerationResult{
		Fallback: true, FailureReason: reason,
		Receipt: &CompactionReceipt{
			Status:         "fallback",
			Mode:           "post_turn",
			Phase:          CompactionPhasePostTurn,
			FallbackReason: reason,
		},
	}
}

func digestReceipt(
	artifact *agentcontext.NarrativeArtifact,
	usage provider.Usage,
	fallback bool,
	reason string,
) *CompactionReceipt {
	if artifact == nil {
		return nil
	}
	status := "completed"
	if fallback {
		status = "fallback"
	}
	bytes := 0
	for _, item := range artifact.Body.Items {
		bytes += len(item.Text)
	}
	return &CompactionReceipt{
		CompactionID:          artifact.Digest,
		Status:                status,
		Mode:                  "post_turn",
		Phase:                 CompactionPhasePostTurn,
		SourceWindowID:        artifact.WindowID,
		TargetWindowID:        artifact.WindowID,
		AuthorityDigest:       artifact.AuthorityDigest,
		AuthorityEquivalent:   true,
		NarrativeIncluded:     !fallback && len(artifact.Body.Items) > 0,
		NarrativeBytes:        bytes,
		NarrativeInputTokens:  usage.InputTokens,
		NarrativeOutputTokens: usage.OutputTokens,
		FallbackReason:        reason,
	}
}

func narrativeReplacements(conversation *agentcontext.ConversationState, input agentcontext.NarrativeInputArtifact) []agentcontext.ConversationReplacement {
	if conversation == nil {
		return nil
	}
	turns := map[uint64]bool{}
	for _, excerpt := range input.Excerpts {
		turns[excerpt.Turn] = true
	}
	var values []agentcontext.ConversationReplacement
	for _, replacement := range conversation.Replacements {
		if source, ok := conversation.Sources[replacement.OldGroupID]; ok && turns[source.Turn] {
			values = append(values, replacement)
		}
	}
	return values
}

// invalidatePendingNarrative cancels expensive work at restore/withdrawal
// boundaries; the epoch/source fence still decides whether a result can land.
func (e *Engine) invalidatePendingNarrative() {
	e.narrativeMu.Lock()
	defer e.narrativeMu.Unlock()
	if e.pendingNarrative != nil {
		e.pendingNarrative.cancel()
	}
}
