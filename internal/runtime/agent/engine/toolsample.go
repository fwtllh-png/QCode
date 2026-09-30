package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/trace"
)

type toolSampleMetadata struct {
	Index              uint32
	Provider           string
	Model              string
	Purpose            model.Purpose
	CallID             string
	Pricing            model.Pricing
	MetadataProvenance model.MetadataProvenance
}

type toolSampleProjection struct {
	Usage     provider.Usage
	CostUSD   float64
	CostKnown bool
	Metadata  toolSampleMetadata
}

type toolSampleHooks struct {
	NextSample func() uint32
	Begin      func(
		context.Context,
		toolSampleMetadata,
	) (context.Context, func(error))
	Price  func(model.Pricing, provider.Usage) (float64, bool)
	Record func(toolSampleProjection)
	Emit   func(toolSampleProjection) error
}

type toolSampleAccount struct {
	mu    sync.Mutex
	hooks toolSampleHooks
}

type toolSampleAccountKey struct{}

func withToolSampleAccount(
	ctx context.Context,
	hooks toolSampleHooks,
) context.Context {
	return context.WithValue(ctx, toolSampleAccountKey{}, &toolSampleAccount{
		hooks: hooks,
	})
}

type ToolSampler struct {
	provider provider.Provider
}

func NewToolSampler(target provider.Provider) *ToolSampler {
	return &ToolSampler{provider: target}
}

func (s *ToolSampler) Stream(
	ctx context.Context,
	request provider.ModelRequest,
) (provider.Stream, error) {
	if s == nil || s.provider == nil {
		return nil, errors.New("tool sampler has no provider")
	}
	account, _ := ctx.Value(toolSampleAccountKey{}).(*toolSampleAccount)
	if account == nil {
		return s.provider.Stream(ctx, request)
	}
	return account.stream(ctx, s.provider, request)
}

var _ provider.Provider = (*ToolSampler)(nil)

func (a *toolSampleAccount) stream(
	ctx context.Context,
	target provider.Provider,
	request provider.ModelRequest,
) (provider.Stream, error) {
	purpose := request.Purpose
	if purpose == "" {
		purpose = model.PurposeAct
	}
	metadata := toolSampleMetadata{
		Provider:           request.Route.ProviderID(),
		Model:              request.Route.Model().ID,
		Purpose:            purpose,
		CallID:             tool.InvocationIdentityFrom(ctx).CallID,
		Pricing:            request.Route.Model().Pricing,
		MetadataProvenance: request.Route.Model().MetadataProvenance,
	}
	if a.hooks.NextSample != nil {
		metadata.Index = a.hooks.NextSample()
	}
	finish := func(error) {}
	if a.hooks.Begin != nil {
		ctx, finish = a.hooks.Begin(ctx, metadata)
	}
	stream, err := target.Stream(ctx, request)
	if err != nil {
		finish(err)
		return nil, err
	}
	return &toolSampleStream{
		stream: stream, account: a, metadata: metadata, finish: finish,
	}, nil
}

type toolSampleStream struct {
	stream   provider.Stream
	account  *toolSampleAccount
	metadata toolSampleMetadata
	finish   func(error)

	usage     provider.Usage
	closeOnce sync.Once
}

func (s *toolSampleStream) Recv() (provider.StreamEvent, error) {
	event, err := s.stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			s.complete(nil)
		} else {
			s.complete(err)
		}
		return event, err
	}
	if event.Type == provider.EventUsage && event.Usage != nil {
		// Stream usage is cumulative for this transport, so merge the latest
		// snapshot instead of double-counting repeated Provider reports.
		s.usage = provider.MergeCumulative(s.usage, *event.Usage)
		projection := toolSampleProjection{
			Usage: s.usage, Metadata: s.metadata,
		}
		if s.account.hooks.Price != nil {
			projection.CostUSD, projection.CostKnown =
				s.account.hooks.Price(s.metadata.Pricing, s.usage)
		}
		if s.account.hooks.Record != nil {
			s.account.hooks.Record(projection)
		}
		s.account.mu.Lock()
		emit := s.account.hooks.Emit
		var emitErr error
		if emit != nil {
			emitErr = emit(projection)
		}
		s.account.mu.Unlock()
		if emitErr != nil {
			s.complete(emitErr)
			return provider.StreamEvent{},
				fmt.Errorf("persist tool sample usage: %w", emitErr)
		}
	}
	if event.Type == provider.EventMessageStop {
		s.complete(nil)
	}
	return event, nil
}

func (s *toolSampleStream) Close() error {
	err := s.stream.Close()
	s.complete(err)
	return err
}

func (s *toolSampleStream) complete(err error) {
	s.closeOnce.Do(func() { s.finish(err) })
}

type toolAccount struct {
	engine *Engine
	emit   func(Event) error
}

func withToolAccount(ctx context.Context, account *toolAccount) context.Context {
	if account == nil || account.engine == nil {
		return ctx
	}
	return withToolSampleAccount(
		ctx,
		account.hooks(),
	)
}

func (a *toolAccount) stream(
	ctx context.Context,
	target provider.Provider,
	request provider.ModelRequest,
) (provider.Stream, error) {
	return NewToolSampler(target).Stream(
		withToolAccount(ctx, a),
		request,
	)
}

func (a *toolAccount) hooks() toolSampleHooks {
	return toolSampleHooks{
		NextSample: a.engine.nextSample,
		Begin: func(
			ctx context.Context,
			metadata toolSampleMetadata,
		) (context.Context, func(error)) {
			span := a.engine.tracer().Start(
				trace.NameModelCall,
				a.engine.toolSpanID(metadata.CallID),
				map[string]any{
					"provider": metadata.Provider,
					"model":    metadata.Model,
					"sample":   metadata.Index,
					"purpose":  string(metadata.Purpose),
					"call_id":  metadata.CallID,
				},
			)
			return a.engine.tracer().Context(ctx, span.ID()), func(err error) {
				if err != nil {
					span.Set("error", errorText(err))
					span.End(trace.StatusError)
					return
				}
				span.End(trace.StatusOK)
			}
		},
		Price: func(
			pricing model.Pricing,
			usage provider.Usage,
		) (float64, bool) {
			return provider.EstimateCost(pricing, usage),
				provider.PricingKnown(pricing, usage)
		},
		Record: func(projection toolSampleProjection) {
			a.engine.addToolSpend(
				projection.Usage,
				projection.CostUSD,
				projection.CostKnown,
				projection.Metadata.Index,
			)
		},
		Emit: func(projection toolSampleProjection) error {
			if a.emit == nil {
				return nil
			}
			return a.emit(Event{
				Usage:   &projection.Usage,
				CostUSD: projection.CostUSD, CostKnown: projection.CostKnown,
				Sample:   projection.Metadata.Index,
				Provider: projection.Metadata.Provider,
				Model:    projection.Metadata.Model,
				Purpose:  string(projection.Metadata.Purpose),
				ModelMetadata: modelMetadataProvenance(
					projection.Metadata.MetadataProvenance,
				),
			})
		},
	}
}

type toolSpend struct {
	usage   provider.Usage
	cost    float64
	known   bool
	samples int
}

func (e *Engine) nextSample() uint32 {
	scope := e.executionScope()
	if scope == nil {
		return 0
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	scope.state.samples++
	return scope.state.samples
}

func (e *Engine) addToolSpend(
	usage provider.Usage,
	cost float64,
	known bool,
	index uint32,
) {
	scope := e.executionScope()
	if scope == nil {
		return
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.state.toolSamples == nil {
		scope.state.toolSamples = make(map[uint32]toolSpend)
	}
	scope.state.toolSamples[index] = toolSpend{
		usage: usage, cost: cost, known: known,
	}
}

func (e *Engine) drainToolSpend() toolSpend {
	scope := e.executionScope()
	if scope == nil {
		return toolSpend{known: true}
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	total := toolSpend{known: true}
	for index, spend := range scope.state.toolSamples {
		total.usage.Add(spend.usage)
		total.cost += spend.cost
		total.known = total.known && spend.known
		total.samples++
		delete(scope.state.toolSamples, index)
	}
	return total
}

func (e *Engine) toolSpanID(callID string) uint64 {
	if callID == "" {
		return 0
	}
	scope := e.executionScope()
	if scope == nil {
		return 0
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	return scope.state.toolSpans[callID]
}
