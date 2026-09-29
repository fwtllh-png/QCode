package prompt_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/builtin"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestDefaultToolCatalogBaseline(t *testing.T) {
	root := t.TempDir()
	store := contentstore.NewMemory(contentstore.Options{})
	manager := process.NewSessionManager(0)
	t.Cleanup(func() {
		manager.CloseAll()
		_ = store.Close(context.Background())
	})
	started := time.Now()
	registry, _, err := builtin.NewWithDependencies(
		root, catalogBenchmarkBackend{}, store, manager,
	)
	if err != nil {
		t.Fatal(err)
	}
	startup := time.Since(started)
	descriptors := registry.Descriptors(tool.VisibleModel)
	available, deferred, unavailable := availabilityCounts(descriptors)
	rendered := promptcontext.NewToolCatalogSection(registry).Render()
	t.Logf(
		"baseline tools=%d available=%d deferred=%d unavailable=%d "+
			"catalog_bytes=%d estimated_tokens=%d startup=%s",
		len(descriptors), available, deferred, unavailable,
		len(rendered), estimateTokens(rendered), startup.Round(time.Microsecond),
	)
	if len(descriptors) == 0 || len(rendered) == 0 {
		t.Fatal("default catalog baseline is empty")
	}
}

func BenchmarkToolCatalogScale(b *testing.B) {
	for _, count := range []int{100, 500, 1000} {
		b.Run(fmt.Sprintf("tools_%d", count), func(b *testing.B) {
			registry := syntheticRegistry(b, count)
			rendered := promptcontext.NewToolCatalogSection(registry).Render()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				section := promptcontext.NewToolCatalogSection(registry)
				if section.Digest() == "" || section.Render() == "" {
					b.Fatal("catalog render is empty")
				}
			}
			b.ReportMetric(float64(len(rendered)), "catalog_bytes")
			b.ReportMetric(float64(estimateTokens(rendered)), "estimated_tokens")
			b.ReportMetric(float64(count), "tools")
		})
	}
}

func BenchmarkToolRegistryStartupScale(b *testing.B) {
	for _, count := range []int{100, 500, 1000} {
		b.Run(fmt.Sprintf("tools_%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				registry := tool.NewRegistry(nil, nil)
				for index := range count {
					if err := registry.Register(syntheticCatalogTool{index: index}); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.ReportMetric(float64(count), "tools")
		})
	}
}

type syntheticCatalogTool struct {
	index int
}

func (s syntheticCatalogTool) Descriptor() tool.Descriptor {
	name := fmt.Sprintf("fixture_tool_%04d", s.index)
	return tool.Descriptor{
		Name: name,
		Description: "Hermetic catalog benchmark tool for measuring dynamic " +
			"tool discovery and prompt context overhead",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer", "minimum": float64(1)},
			},
			"required":             []string{"query"},
			"additionalProperties": false,
		},
		Visibility: tool.VisibleModel, Capability: tool.CapabilityRead,
		AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelConcurrent,
		SandboxRequirement: tool.SandboxNone, Availability: tool.AvailabilityAvailable,
	}
}

func (syntheticCatalogTool) Execute(
	context.Context,
	json.RawMessage,
) (tool.Result, error) {
	return tool.Result{Content: "ok"}, nil
}

func syntheticRegistry(tb testing.TB, count int) *tool.Registry {
	tb.Helper()
	registry := tool.NewRegistry(nil, nil)
	for index := range count {
		if err := registry.Register(syntheticCatalogTool{index: index}); err != nil {
			tb.Fatal(err)
		}
	}
	return registry
}

func availabilityCounts(descriptors []tool.Descriptor) (
	available int,
	deferred int,
	unavailable int,
) {
	for _, descriptor := range descriptors {
		switch descriptor.Availability {
		case tool.AvailabilityAvailable:
			available++
		case tool.AvailabilityDeferred:
			deferred++
		case tool.AvailabilityUnavailable:
			unavailable++
		}
	}
	return
}

func estimateTokens(text string) int {
	return (len(text) + 3) / 4
}

type catalogBenchmarkBackend struct{}

func (catalogBenchmarkBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "fixture", Backend: "passthrough",
		Available: true,
		Effective: securitymodel.Controls{
			FilesystemRead: securitymodel.FilesystemReadDeclaredRoots,

			FilesystemWrite: securitymodel.FilesystemWriteExactPaths, Network: securitymodel.NetworkDenied, ProcessTree: securitymodel.ProcessTreeGroupKill,
			CrossProcess: securitymodel.CrossProcessUnrestricted,
			Syscall:      securitymodel.SyscallDenyDangerous, IPC: securitymodel.IPCUnrestricted, PathIdentity: securitymodel.PathIdentityDescriptorRelative,
			ArtifactOrigin: securitymodel.ArtifactOriginUnverifiedPath,

			DurableRecovery: securitymodel.DurableRecoveryMemoryOnly,
		},
	}
}

func (catalogBenchmarkBackend) Prepare(
	_ context.Context,
	command sandbox.Command,
) (sandbox.Command, error) {
	return command, nil
}
