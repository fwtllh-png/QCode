package toolsearch_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/toolsearch"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

type stubExec struct {
	name, desc string
	terms      []string
	deferred   bool
}

func (s stubExec) Descriptor() tool.Descriptor {
	availability := tool.AvailabilityAvailable
	if s.deferred {
		availability = tool.AvailabilityDeferred
	}
	return tool.Descriptor{
		Name: s.name, Description: s.desc, Visibility: tool.VisibleModel,
		DiscoveryTerms: s.terms,
		Capability:     tool.CapabilityRead, AccessMode: tool.AccessRead,
		ParallelPolicy: tool.ParallelConcurrent, SandboxRequirement: tool.SandboxNone,
		Availability: availability,
		InputSchema:  map[string]any{"type": "object"},
	}
}

func TestToolSearchRanksMultilingualDiscoveryTerms(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := toolsearch.Register(registry); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(stubExec{
		name: "format_code", desc: "format exact source files",
		terms: []string{"格式化代码", "代码格式"},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      toolsearch.ToolName,
		Arguments: json.RawMessage(`{"query":"请格式化代码并验证"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "format_code") {
		t.Fatalf("multilingual search result = %s", result.Content)
	}
}

func TestToolSearchPreservesSampledEagerToolBinding(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := toolsearch.Register(registry); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(stubExec{
		name: "file_read", desc: "read source file",
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := snapshot.Binding("file_read")
	if !ok {
		t.Fatal("file_read binding is missing")
	}
	if _, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      toolsearch.ToolName,
		Arguments: json.RawMessage(`{"query":"read source file"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := registry.ResolveBound("file_read", binding); err != nil {
		t.Fatalf("sampled eager tool binding became stale: %v", err)
	}
	after, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := after.Lookup("file_read")
	if !ok || entry.State != tool.CatalogEntryMaterialized ||
		entry.Revision != binding.Revision ||
		after.Generation <= snapshot.Generation {
		t.Fatalf(
			"materialized eager entry = %+v generation=%d, want revision=%d and newer generation",
			entry,
			after.Generation,
			binding.Revision,
		)
	}
}

func (stubExec) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Content: "ok"}, nil
}

func TestToolSearchRanksDeferredMatches(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := toolsearch.Register(registry); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(stubExec{name: "alpha_fetch", desc: "fetch remote alpha"}); err != nil {
		t.Fatal(err)
	}
	deferred := stubExec{
		name: "beta_extension", desc: "external beta helper", deferred: true,
	}.Descriptor()
	if err := registry.RegisterTrusted(
		"test:beta-extension",
		tool.NewExternalDeferredRegistration(
			tool.ExternalFromDescriptor(deferred),
			tool.TrustedBindingFromDescriptor(deferred),
			func() (tool.Executor, error) {
				return stubExec{name: "beta_extension", desc: "external beta helper"}, nil
			},
		),
	); err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      toolsearch.ToolName,
		Arguments: json.RawMessage(`{"query":"external beta"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "beta_extension") {
		t.Fatalf("content=%s", result.Content)
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range snapshot.Entries() {
		if entry.Name == "beta_extension" {
			if entry.State != tool.CatalogEntryMaterialized ||
				entry.Descriptor.Availability != tool.AvailabilityAvailable {
				t.Fatalf("entry = %+v, want materialized/available", entry)
			}
			called, executeErr := tooltest.Execute(t.Context(), registry, tool.Call{
				Name: "beta_extension", Arguments: json.RawMessage(`{}`),
			})
			if executeErr != nil || called.Content == "" {
				t.Fatalf("execute materialized tool: result=%+v err=%v", called, executeErr)
			}
			return
		}
	}
	t.Fatal("beta_extension descriptor is missing")
}

func TestConcurrentToolSearchSharesMaterializationTransition(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := toolsearch.Register(registry); err != nil {
		t.Fatal(err)
	}
	deferred := stubExec{
		name: "workflow_create", desc: "create durable workflow", deferred: true,
	}.Descriptor()
	if err := registry.RegisterTrusted(
		"test:workflow-create",
		tool.NewExternalDeferredRegistration(
			tool.ExternalFromDescriptor(deferred),
			tool.TrustedBindingFromDescriptor(deferred),
			func() (tool.Executor, error) {
				return stubExec{name: "workflow_create", desc: "create durable workflow"}, nil
			},
		),
	); err != nil {
		t.Fatal(err)
	}
	const searches = 32
	start := make(chan struct{})
	errs := make(chan error, searches)
	var wait sync.WaitGroup
	for range searches {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, err := tooltest.Execute(t.Context(), registry, tool.Call{
				Name:      toolsearch.ToolName,
				Arguments: json.RawMessage(`{"query":"durable workflow"}`),
			})
			if err == nil && result.IsError {
				err = &toolSearchError{message: result.Content}
			}
			errs <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

type toolSearchError struct{ message string }

func (e *toolSearchError) Error() string { return e.message }

func TestToolSearchEnabledFilterIsInvocationScoped(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := toolsearch.Register(registry); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha_inspect", "beta_inspect"} {
		if err := registry.Register(stubExec{name: name, desc: "inspect"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, allowed := range []string{"alpha_inspect", "beta_inspect"} {
		t.Run(allowed, func(t *testing.T) {
			t.Parallel()
			ctx := toolsearch.WithEnabled(t.Context(), func(entry tool.CatalogEntrySnapshot) bool {
				return entry.Name == allowed
			})
			for range 4 {
				result, err := tooltest.Execute(ctx, registry, tool.Call{
					Name: toolsearch.ToolName, Arguments: json.RawMessage(`{"query":"inspect"}`),
				})
				if err != nil || result.IsError {
					t.Fatalf("search=%+v err=%v", result, err)
				}
				var body struct {
					Matches []struct{ Name string } `json:"matches"`
				}
				if err := json.Unmarshal([]byte(result.Content), &body); err != nil {
					t.Fatal(err)
				}
				if len(body.Matches) != 1 || body.Matches[0].Name != allowed {
					t.Fatalf("invocations shared enablement: %s", result.Content)
				}
			}
		})
	}
}
