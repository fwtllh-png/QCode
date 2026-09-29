package wire

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mcpruntime "github.com/fwtllh-png/QCode/internal/adapter/mcp"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	mcptool "github.com/fwtllh-png/QCode/internal/adapter/tool/mcp"
)

// MCPPrewarm coalesces dirty MCP refresh requests onto a single worker (N9).
type MCPPrewarm struct {
	dirty       atomic.Bool
	ch          chan struct{}
	pool        *mcpruntime.Pool
	adapter     *mcptool.Adapter
	registry    *tool.Registry
	loadConfig  func() (mcpruntime.Config, error)
	configMu    sync.Mutex
	blocked     map[string]bool
	cancel      context.CancelFunc
	unsubscribe func()
	done        sync.WaitGroup
}

func NewMCPPrewarmConfig(
	pool *mcpruntime.Pool,
	config mcpruntime.Config,
) *MCPPrewarm {
	snapshot := mcpruntime.CloneConfig(config)
	return &MCPPrewarm{
		ch: make(chan struct{}, 1), pool: pool,
		blocked: make(map[string]bool),
		loadConfig: func() (mcpruntime.Config, error) {
			return mcpruntime.CloneConfig(snapshot), nil
		},
	}
}

func (p *MCPPrewarm) SetServerPrefixEnabled(prefix string, enabled bool) {
	if p == nil || prefix == "" {
		return
	}
	p.configMu.Lock()
	if p.blocked == nil {
		p.blocked = make(map[string]bool)
	}
	if enabled {
		delete(p.blocked, prefix)
	} else {
		p.blocked[prefix] = true
	}
	p.configMu.Unlock()
	p.dirty.Store(true)
}

func (p *MCPPrewarm) DisableServerPrefix(
	ctx context.Context,
	prefix string,
) error {
	if p == nil || p.pool == nil {
		return nil
	}
	p.SetServerPrefixEnabled(prefix, false)
	return p.pool.RemoveServerPrefix(ctx, prefix)
}

func (p *MCPPrewarm) SetRegistry(registry *tool.Registry) {
	if p != nil {
		p.registry = registry
	}
}

// Start runs the worker and breaker-retry timers until Stop. parent only
// supplies values: construction may run under a request context that ends
// long before the Session that owns this prewarm.
func (p *MCPPrewarm) Start(parent context.Context) {
	if p == nil || p.pool == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	p.cancel = cancel
	p.unsubscribe = p.pool.SubscribeHealth(func(change mcpruntime.HealthChange) {
		if change.Current.State != mcpruntime.HealthOpen || change.Current.RetryAt.IsZero() {
			return
		}
		p.scheduleRetry(ctx, change.Current.RetryAt)
	})
	for _, snapshot := range p.pool.HealthSnapshots() {
		if snapshot.State == mcpruntime.HealthOpen && !snapshot.RetryAt.IsZero() {
			p.scheduleRetry(ctx, snapshot.RetryAt)
		}
	}
	p.done.Add(1)
	go func() {
		defer p.done.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.ch:
				_ = p.refreshIfDirty(ctx)
			}
		}
	}()
}

func (p *MCPPrewarm) scheduleRetry(ctx context.Context, retryAt time.Time) {
	delay := time.Until(retryAt)
	if delay < 0 {
		delay = 0
	}
	p.done.Add(1)
	go func() {
		defer p.done.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
			p.requestRetry()
		}
	}()
}

func (p *MCPPrewarm) Stop() {
	if p == nil {
		return
	}
	if p.cancel != nil {
		p.cancel()
	}
	if p.unsubscribe != nil {
		p.unsubscribe()
		p.unsubscribe = nil
	}
	p.done.Wait()
}

func (p *MCPPrewarm) requestRetry() {
	if p == nil {
		return
	}
	p.dirty.Store(true)
	select {
	case p.ch <- struct{}{}:
	default:
	}
}

// RequestRefresh marks the pool dirty and wakes the worker (coalesced).
func (p *MCPPrewarm) RequestRefresh() {
	if p == nil {
		return
	}
	p.dirty.Store(true)
	select {
	case p.ch <- struct{}{}:
	default:
	}
}

// RefreshNow runs a synchronous dirty refresh (correctness path before MCP use).
func (p *MCPPrewarm) RefreshNow(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.dirty.Store(true)
	return p.refreshIfDirty(ctx)
}

// SyncCatalog reconciles the current Pool view into the shared Registry.
// Background refresh is an optimization; sampling calls this as its
// correctness boundary so an asynchronous failure cannot expose stale tools.
func (p *MCPPrewarm) SyncCatalog() error {
	if p == nil {
		return nil
	}
	if err := p.ensureAdapter(); err != nil {
		return err
	}
	return p.adapter.Sync()
}

func (p *MCPPrewarm) refreshIfDirty(ctx context.Context) error {
	if !p.dirty.Swap(false) {
		return nil
	}
	if p.pool == nil || p.loadConfig == nil {
		return nil
	}
	if err := p.ensureAdapter(); err != nil {
		p.dirty.Store(true)
		return err
	}
	config, err := p.loadConfig()
	if err != nil {
		p.dirty.Store(true)
		return err
	}
	p.configMu.Lock()
	blocked := make(map[string]bool, len(p.blocked))
	for prefix := range p.blocked {
		blocked[prefix] = true
	}
	p.configMu.Unlock()
	for name := range config.Servers {
		for prefix := range blocked {
			if strings.HasPrefix(name, prefix) {
				delete(config.Servers, name)
				break
			}
		}
	}
	if _, err := p.pool.Reload(ctx, config); err != nil {
		p.dirty.Store(true)
		return err
	}
	if err := p.pool.ProbeOpen(ctx); err != nil {
		p.dirty.Store(true)
		return err
	}
	if p.adapter != nil {
		if err := p.adapter.Sync(); err != nil {
			p.dirty.Store(true)
			return err
		}
	}
	return nil
}

func (p *MCPPrewarm) ensureAdapter() error {
	if p.adapter != nil {
		return nil
	}
	adapter, err := mcptool.NewAdapter(p.registry, p.pool)
	if err != nil {
		return err
	}
	p.adapter = adapter
	return nil
}
