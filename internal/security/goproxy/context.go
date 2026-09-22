package goproxy

import (
	"context"
	"sync"

	"github.com/fwtllh-png/QCode/internal/environment"
)

type serviceKey struct{}

func WithService(ctx context.Context, service *Service) context.Context {
	if ctx == nil || service == nil {
		return ctx
	}
	return context.WithValue(ctx, serviceKey{}, service)
}

func ServiceFrom(ctx context.Context) *Service {
	if ctx == nil {
		return nil
	}
	service, _ := ctx.Value(serviceKey{}).(*Service)
	return service
}

type bindReportKey struct{}

// BindReport records why a host GOPROXY auth binding did not land. A silent
// nil service surfaces later as an unattributed 401 from the private proxy:
// the failure must stay a Fact so the first failed command can classify it
// as credential_unavailable instead of the model probing blind.
type BindReport struct {
	mu    sync.Mutex
	facts []environment.Fact
}

func (r *BindReport) Record(fact environment.Fact) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.facts = append(r.facts, fact)
	r.mu.Unlock()
}

func (r *BindReport) Facts() []environment.Fact {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]environment.Fact(nil), r.facts...)
}

func WithBindReport(ctx context.Context, report *BindReport) context.Context {
	if ctx == nil || report == nil {
		return ctx
	}
	return context.WithValue(ctx, bindReportKey{}, report)
}

func BindReportFrom(ctx context.Context) *BindReport {
	if ctx == nil {
		return nil
	}
	report, _ := ctx.Value(bindReportKey{}).(*BindReport)
	return report
}
