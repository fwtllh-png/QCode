package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerratelimit "github.com/fwtllh-png/QCode/internal/adapter/provider/ratelimit"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/security/guardian"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/google/uuid"
)

type GuardianConfig struct {
	Enabled         bool
	Timeout         time.Duration
	MaxOutputTokens uint64
}

func validateGuardianConfig(config GuardianConfig, routes model.RouteSet) error {
	if config.Timeout < 0 || (config.Enabled && config.Timeout == 0) {
		return errors.New("Guardian requires an explicit positive timeout when enabled")
	}
	if !config.Enabled && config.MaxOutputTokens == 0 {
		return nil
	}
	route, err := routes.For(model.PurposeJudge)
	if err != nil {
		return err
	}
	if route.Model().Limits.ContextTokens == 0 || route.Model().Limits.MaxOutputTokens == 0 || config.MaxOutputTokens > route.Model().Limits.MaxOutputTokens {
		return errors.New("Guardian output setting is incompatible with the judge model limits")
	}
	return nil
}

// GuardianReview freezes one request's route and rules before Guard constructs
// its Candidate. It can be consumed once; it has no approval or execution API.
type GuardianReview struct {
	toolSpend bool
	engine    *Engine
	options   Options
	route     model.ReadyRoute
	id        string
	sessionID string
	versions  guardian.ReviewVersions
	used      atomic.Bool
}

type GuardianReviewResult struct {
	ReviewID, Provider, Model string
	Evidence                  *guardian.ReviewEvidence
	Usage                     provider.Usage
	CostUSD                   float64
	CostKnown                 bool
	Attempted                 bool
	Duration                  time.Duration
	// QueueDuration covers the shared concurrency and throughput admission
	// waits. ProviderDuration includes transport and stream consumption; it
	// cannot separate server-side queueing from model inference.
	QueueDuration    time.Duration
	ProviderDuration time.Duration
	UsageObserved    bool
	InvalidOutput    bool
}

func (e *Engine) PrepareGuardianReview() (*GuardianReview, error) {
	return e.prepareGuardianReview(context.Background())
}

func (e *Engine) prepareGuardianReview(ctx context.Context) (*GuardianReview, error) {
	e.titleState.mu.Lock()
	options := e.titleState.options
	e.titleState.mu.Unlock()
	security := options.Security.CloneSampling()
	if !options.Guardian.Enabled || security == nil || security.DisableAutoReview || security.Permission != policy.PermissionAuto {
		return nil, errors.New("Guardian review is disabled for this permission posture")
	}
	if options.SharedRateLimit == nil {
		return nil, errors.New("Guardian requires the shared provider gate")
	}
	if err := validateGuardianConfig(options.Guardian, options.Routes); err != nil {
		return nil, err
	}
	route, err := options.Routes.For(model.PurposeJudge)
	if err != nil {
		return nil, err
	}
	// Capabilities contain slices and cached pricing is optional by pointer.
	// Retain owned values even if the caller later replaces model metadata.
	modelDescriptor := route.Model()
	if modelDescriptor.Pricing.CachedInputPerMillion != nil {
		price := *modelDescriptor.Pricing.CachedInputPerMillion
		modelDescriptor.Pricing.CachedInputPerMillion = &price
	}
	route = route.WithModel(modelDescriptor)
	descriptor, err := route.Describe()
	if err != nil {
		return nil, err
	}
	return &GuardianReview{engine: e, options: options, route: route, id: uuid.NewString(), sessionID: e.sessionIdentity(ctx), versions: guardian.ReviewVersions{
		ConfigurationDigest: guardianReviewDigest(options.Guardian), RouteDigest: guardianReviewDigest(descriptor),
		PromptVersion: guardianPromptVersion, SchemaVersion: guardianSchemaVersion,
	}}, nil
}

func (r *GuardianReview) ID() string                        { return r.id }
func (r *GuardianReview) Versions() guardian.ReviewVersions { return r.versions }

func (r *GuardianReview) Review(ctx context.Context, candidate guardian.ReviewCandidate, authorization agentcontext.GuardianAuthorization, content map[string][]byte) (result GuardianReviewResult, err error) {
	if !r.used.CompareAndSwap(false, true) {
		return result, errors.New("Guardian review attempt was already consumed")
	}
	started := time.Now()
	result.ReviewID, result.Provider, result.Model = r.id, r.route.ProviderID(), r.route.Model().ID
	defer func() { result.Duration = time.Since(started) }()
	ctx, cancel := context.WithTimeout(ctx, r.options.Guardian.Timeout)
	defer cancel()
	defer func() {
		if cancelled := ctx.Err(); cancelled != nil {
			result.Evidence, err = nil, cancelled
		}
	}()
	if err = ctx.Err(); err != nil {
		return result, err
	}
	messages, err := r.messages(candidate, authorization, content)
	if err != nil {
		return result, err
	}
	input, err := r.options.TokenEstimator.Estimate(messages)
	if err != nil {
		return result, err
	}
	output := r.route.Model().Limits.MaxOutputTokens
	if r.options.Guardian.MaxOutputTokens != 0 {
		output = r.options.Guardian.MaxOutputTokens
	}
	queued := time.Now()
	release, err := r.options.SharedRateLimit.Acquire(ctx)
	result.QueueDuration += time.Since(queued)
	if err != nil {
		return result, err
	}
	defer release()
	output, settle, err := r.engine.reserveModelBudget(r.options, r.route, input, output, candidate.Identity.TurnID, !r.toolSpend)
	if err != nil {
		return result, err
	}
	defer func() {
		settle(result.Usage)
		var failure *provider.Failure
		if errors.As(err, &failure) && failure.Code == provider.FailureRateLimit {
			delay := min(failure.RetryAfterMS, uint64(math.MaxInt64/int64(time.Millisecond)))
			r.options.SharedRateLimit.Record(time.Duration(delay) * time.Millisecond)
		}
		result.CostUSD = provider.EstimateCost(r.route.Model().Pricing, result.Usage)
		result.CostKnown = provider.PricingKnown(r.route.Model().Pricing, result.Usage)
		if r.toolSpend {
			r.engine.recordGuardianCost(candidate.Identity.TurnID, result)
		}
	}()
	queued = time.Now()
	err = r.admitThroughput(ctx, input+output)
	result.QueueDuration += time.Since(queued)
	if err != nil {
		return result, err
	}
	request := provider.ModelRequest{
		Route: r.route, Purpose: model.PurposeJudge, LogicalRequestID: "guardian:" + r.id,
		Messages: messages, MaxOutputTokens: output, SingleAttempt: true,
		ReasoningEffort: agentcontext.NarrativeReasoningEffort(r.route.Model().Capabilities),
	}
	if err = request.Validate(); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	result.Attempted = true
	providerStarted := time.Now()
	defer func() { result.ProviderDuration = time.Since(providerStarted) }()
	stream, err := r.options.Provider.Stream(ctx, request)
	if err != nil {
		return result, err
	}
	var closeOnce sync.Once
	closeStream := func() { closeOnce.Do(func() { _ = stream.Close() }) }
	stopClose := context.AfterFunc(ctx, closeStream)
	defer func() { stopClose(); closeStream() }()
	var body, reasoning strings.Builder
	complete := false
	var invalid error
	for {
		event, recvErr := stream.Recv()
		if event.Usage != nil {
			result.UsageObserved = true
			observed := *event.Usage
			if !event.Usage.Consistent() {
				invalid = errors.New("Guardian provider usage is invalid")
				// Keep observed totals billable even on a malformed snapshot.
				// An impossible cache counter must not earn a pricing discount.
				if observed.CachedTokens > observed.InputTokens {
					observed.CachedTokens = 0
				}
				observed.ReasoningTokens = min(observed.ReasoningTokens, observed.OutputTokens)
			}
			result.Usage = provider.MergeCumulative(result.Usage, observed)
			if result.Usage.OutputTokens > output {
				invalid = errors.New("Guardian provider exceeded the admitted output budget")
			}
		}
		if err = ctx.Err(); err != nil {
			return result, err
		}
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return result, recvErr
		}
		if event.Validate() != nil {
			invalid = errors.New("Guardian provider event is invalid")
		}
		switch event.Type {
		case provider.EventTextDelta:
			if complete {
				invalid = errors.New("Guardian text arrived after message_stop")
			}
			body.WriteString(event.Text)
		case provider.EventReasoningDelta:
			reasoning.WriteString(event.Text)
		case provider.EventMessageStop:
			if complete || event.StopReason != provider.StopReasonEndTurn {
				invalid = errors.New("Guardian output is incomplete")
			}
			complete = true
		case provider.EventUsage, provider.EventMessageStart, provider.EventReasoningSignature, provider.EventTransportProgress, provider.EventReplayState, provider.EventResponseState:
		default:
			invalid = fmt.Errorf("Guardian provider emitted forbidden event %q", event.Type)
		}
		if event.Type == provider.EventTextDelta || event.Type == provider.EventReasoningDelta {
			used, estimateErr := r.options.TokenEstimator.Estimate([]provider.Message{provider.TextMessage(provider.RoleAssistant, body.String()+reasoning.String())})
			if estimateErr != nil {
				return result, estimateErr
			}
			if used > output {
				result.InvalidOutput = true
				return result, errors.New("Guardian output exceeds the admitted token budget")
			}
		}
	}
	if invalid != nil {
		result.InvalidOutput = true
		return result, invalid
	}
	if !complete {
		result.InvalidOutput = true
		return result, errors.New("Guardian provider omitted message_stop")
	}
	result.Evidence, err = guardian.BindAssessment(candidate, []byte(body.String()))
	result.InvalidOutput = err != nil
	return result, err
}

// Admission waits are part of the same deadline; they are not model retries.
func (r *GuardianReview) admitThroughput(ctx context.Context, tokens uint64) error {
	governor, ok := r.options.Provider.(atomicThroughputGovernor)
	if !ok {
		if r.options.TokensPerMinute != 0 {
			return errors.New("Guardian throughput governor is unavailable")
		}
		return nil
	}
	for {
		decision := governor.TryReserveThroughput(r.route, tokens, r.options.TokensPerMinute)
		switch decision.Status {
		case providerratelimit.StatusAdmit:
			return nil
		case providerratelimit.StatusWait:
			if decision.Wait <= 0 {
				return errors.New("Guardian throughput wait is invalid")
			}
			if err := waitRetryDelay(ctx, decision.Wait); err != nil {
				return err
			}
		default:
			return throughputRefusal(decision, decision.Reason, false)
		}
	}
}
