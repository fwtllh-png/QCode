package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpruntime "github.com/fwtllh-png/QCode/internal/adapter/mcp"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/toolsearch"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

type scriptedTransport struct {
	calls       atomic.Int64
	readCalls   atomic.Int64
	promptCalls atomic.Int64
}

func (t *scriptedTransport) Request(
	_ context.Context,
	method string,
	_ any,
	target any,
) error {
	switch method {
	case "initialize":
		*target.(*mcpruntime.InitializeResult) = mcpruntime.InitializeResult{
			ProtocolVersion: mcpruntime.ProtocolVersion,
			Capabilities: map[string]any{
				"tools":     map[string]any{},
				"resources": map[string]any{},
				"prompts":   map[string]any{},
			},
			ServerInfo: mcpruntime.ClientInfo{Name: "scripted", Version: "1"},
		}
	case "tools/list":
		*target.(*mcpruntime.ListToolsResult) = mcpruntime.ListToolsResult{
			Tools: []mcpruntime.Tool{{
				Name:        "danger",
				Description: "guarded remote action",
				InputSchema: map[string]any{"type": "object"},
			}},
		}
	case "tools/call":
		t.calls.Add(1)
		*target.(*mcpruntime.CallToolResult) = mcpruntime.CallToolResult{
			Content: []mcpruntime.Content{{Type: "text", Text: "called"}},
		}
	case "resources/list":
		*target.(*mcpruntime.ListResourcesResult) = mcpruntime.ListResourcesResult{
			Resources: []mcpruntime.Resource{{
				URI: "fixture://readme", Name: "readme",
			}},
		}
	case "resources/templates/list":
		*target.(*mcpruntime.ListResourceTemplatesResult) =
			mcpruntime.ListResourceTemplatesResult{}
	case "resources/read":
		t.readCalls.Add(1)
		*target.(*mcpruntime.ReadResourceResult) = mcpruntime.ReadResourceResult{
			Contents: []mcpruntime.ResourceContent{{
				URI: "fixture://readme", Text: "resource text",
			}},
		}
	case "prompts/list":
		*target.(*mcpruntime.ListPromptsResult) = mcpruntime.ListPromptsResult{
			Prompts: []mcpruntime.Prompt{{Name: "review"}},
		}
	case "prompts/get":
		t.promptCalls.Add(1)
		*target.(*mcpruntime.GetPromptResult) = mcpruntime.GetPromptResult{
			Messages: []mcpruntime.PromptMessage{{
				Role:    "user",
				Content: mcpruntime.Content{Type: "text", Text: "review it"},
			}},
		}
	}
	return nil
}

func TestCapabilityHelpersUseAdvertisedCatalogOnly(t *testing.T) {
	transport := &scriptedTransport{}
	pool := mcpruntime.NewPool(func(
		context.Context,
		string,
		mcpruntime.ServerConfig,
	) (mcpruntime.Transport, error) {
		return transport, nil
	})
	config := mcpruntime.Config{
		Version: mcpruntime.ConfigVersion,
		Servers: map[string]mcpruntime.ServerConfig{
			"remote": {
				Transport: "stdio", HostTrusted: true,
				Command: "scripted",
				Tools: map[string]mcpruntime.ToolBinding{
					"danger": {
						Capability:         "read",
						AccessMode:         "read",
						ParallelPolicy:     "concurrent",
						SandboxRequirement: "none",
					},
				},
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := pool.Reload(ctx, config); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if _, err := NewAdapter(registry, pool); err != nil {
		t.Fatal(err)
	}
	guard, err := toolguard.New(toolguard.Options{
		Registry: registry,
		Policy:   policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass), Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		name      string
		arguments string
		contains  string
	}{
		{"list_mcp_resources", `{}`, "fixture://readme"},
		{"read_mcp_resource", `{"uri":"fixture://readme"}`, "resource text"},
		{"mcp_get_prompt", `{"name":"review"}`, "review it"},
	} {
		result, err := guard.Execute(
			ctx,
			"helper-"+call.name,
			call.name,
			json.RawMessage(call.arguments),
		)
		if err != nil {
			t.Fatalf("%s: %v", call.name, err)
		}
		if !strings.Contains(result.Content, call.contains) {
			t.Fatalf("%s result = %q", call.name, result.Content)
		}
	}
	before := transport.readCalls.Load()
	if _, err := guard.Execute(
		ctx,
		"missing-resource",
		"read_mcp_resource",
		json.RawMessage(`{"uri":"fixture://missing"}`),
	); err == nil {
		t.Fatal("unadvertised resource was accepted")
	}
	if transport.readCalls.Load() != before {
		t.Fatal("unadvertised resource reached MCP wire")
	}
}

func (*scriptedTransport) Notify(context.Context, string, any) error { return nil }
func (*scriptedTransport) Close(context.Context) error               { return nil }
func (*scriptedTransport) StderrTail() string                        { return "" }

func TestAdapterModelCallRequiresToolGuardPolicy(t *testing.T) {
	transport := &scriptedTransport{}
	pool := mcpruntime.NewPool(func(
		context.Context,
		string,
		mcpruntime.ServerConfig,
	) (mcpruntime.Transport, error) {
		return transport, nil
	})
	config := mcpruntime.Config{
		Version: mcpruntime.ConfigVersion,
		Servers: map[string]mcpruntime.ServerConfig{
			"remote": {
				Transport: "stdio", HostTrusted: true,
				Command: "scripted",
				Tools: map[string]mcpruntime.ToolBinding{
					"danger": {
						Capability:         "process",
						AccessMode:         "write",
						ParallelPolicy:     "serial",
						SandboxRequirement: "none",
						Resources: []mcpruntime.ResourceBinding{{
							Kind:   "remote",
							ID:     "scripted",
							Access: "write",
						}},
					},
				},
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := pool.Reload(ctx, config); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if _, err := NewAdapter(registry, pool); err != nil {
		t.Fatal(err)
	}
	descriptors := registry.Descriptors(tool.VisibleModel)
	var descriptor tool.Descriptor
	for _, candidate := range descriptors {
		if candidate.Name == "mcp_remote_danger" {
			descriptor = candidate
		}
	}
	if descriptor.Capability != tool.CapabilityProcess ||
		descriptor.ParallelPolicy != tool.ParallelSerial ||
		len(descriptor.ResourceResolver.Templates) != 1 {
		t.Fatalf("descriptor does not preserve configured security truth: %+v", descriptor)
	}

	denyingGuard, err := toolguard.New(toolguard.Options{
		Registry: registry,
		Policy:   policy.DefaultRuntime(policy.ModeAct, policy.PermissionNever), Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, executeErr := denyingGuard.Execute(
		ctx,
		"denied",
		"mcp_remote_danger",
		json.RawMessage(`{}`),
	); executeErr == nil {
		t.Fatal("high-risk MCP call bypassed ToolGuard policy")
	}
	if transport.calls.Load() != 0 {
		t.Fatal("denied MCP call reached remote transport")
	}

	allowingGuard, err := toolguard.New(toolguard.Options{
		Registry: registry,
		Policy:   policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass), Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := allowingGuard.Execute(
		ctx,
		"allowed",
		"mcp_remote_danger",
		json.RawMessage(`{}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "called" || transport.calls.Load() != 1 {
		t.Fatalf("allowed result=%+v calls=%d", result, transport.calls.Load())
	}
}

func TestLargeMCPToolCatalogIsDeferredUntilSearchMaterializes(t *testing.T) {
	const toolCount = 100
	transport := &largeCatalogTransport{}
	bindings := make(map[string]mcpruntime.ToolBinding, toolCount)
	transport.tools = make([]mcpruntime.Tool, 0, toolCount)
	for index := range toolCount {
		name := fmt.Sprintf("remote_%03d", index)
		bindings[name] = mcpruntime.ToolBinding{
			Capability: "read", AccessMode: "read",
			ParallelPolicy: "concurrent", SandboxRequirement: "none",
		}
		transport.tools = append(transport.tools, mcpruntime.Tool{
			Name: name, Description: "large catalog " + name,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"value": map[string]any{"type": "string"},
				},
				"additionalProperties": false,
			},
		})
	}
	pool := mcpruntime.NewPool(func(
		context.Context,
		string,
		mcpruntime.ServerConfig,
	) (mcpruntime.Transport, error) {
		return transport, nil
	})
	if _, err := pool.Reload(t.Context(), mcpruntime.Config{
		Version: mcpruntime.ConfigVersion,
		Servers: map[string]mcpruntime.ServerConfig{
			"large": {
				Transport: "stdio", Command: "scripted", HostTrusted: true, Tools: bindings,
				ConnectTimeout: time.Second, CallTimeout: time.Second,
				ShutdownTimeout: time.Second,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.ShutdownAll(context.Background()) }()
	registry := tool.NewRegistry(nil, nil)
	if err := toolsearch.Register(registry); err != nil {
		t.Fatal(err)
	}
	adapter, err := NewAdapter(registry, pool)
	if err != nil {
		t.Fatal(err)
	}

	assertMCPStates(t, registry, toolCount, 0)
	target := "mcp_large_remote_042"
	before, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	beforeEntry, ok := before.Lookup(target)
	if !ok || beforeEntry.Descriptor.Availability != tool.AvailabilityDeferred {
		t.Fatalf("target before search = %+v ok=%v", beforeEntry, ok)
	}
	if transport.calls.Load() != 0 {
		t.Fatal("catalog discovery called a remote tool")
	}

	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      toolsearch.ToolName,
		Arguments: json.RawMessage(`{"query":"remote_042","limit":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(result.Content, target) {
		t.Fatalf("tool_search result = %+v", result)
	}
	if transport.calls.Load() != 0 {
		t.Fatal("materializing an MCP tool called the remote tool")
	}
	assertMCPStates(t, registry, toolCount-1, 1)

	materialized, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	materializedEntry, _ := materialized.Lookup(target)
	if syncErr := adapter.Sync(); syncErr != nil {
		t.Fatal(syncErr)
	}
	afterSync, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	afterEntry, _ := afterSync.Lookup(target)
	if afterEntry.State != tool.CatalogEntryMaterialized ||
		afterEntry.Revision != materializedEntry.Revision {
		t.Fatalf(
			"materialized entry changed during no-op sync: before=%+v after=%+v",
			materializedEntry, afterEntry,
		)
	}

	called, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      target,
		Arguments: json.RawMessage(`{"value":"ok"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if called.Content != "called" || transport.calls.Load() != 1 {
		t.Fatalf("called=%+v remote calls=%d", called, transport.calls.Load())
	}
}

func assertMCPStates(
	t *testing.T,
	registry *tool.Registry,
	wantDeferred int,
	wantMaterialized int,
) {
	t.Helper()
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	deferred, materialized := 0, 0
	for _, entry := range snapshot.Entries() {
		if !strings.HasPrefix(entry.Source, "mcp:large") {
			continue
		}
		switch entry.State {
		case tool.CatalogEntryDeferred:
			deferred++
		case tool.CatalogEntryMaterialized:
			materialized++
		}
	}
	if deferred != wantDeferred || materialized != wantMaterialized {
		t.Fatalf(
			"MCP states deferred=%d materialized=%d, want %d/%d",
			deferred, materialized, wantDeferred, wantMaterialized,
		)
	}
}

type largeCatalogTransport struct {
	tools []mcpruntime.Tool
	calls atomic.Int64
}

func (t *largeCatalogTransport) Request(
	_ context.Context,
	method string,
	_ any,
	target any,
) error {
	switch method {
	case "initialize":
		*target.(*mcpruntime.InitializeResult) = mcpruntime.InitializeResult{
			ProtocolVersion: mcpruntime.ProtocolVersion,
			Capabilities:    map[string]any{"tools": map[string]any{}},
			ServerInfo:      mcpruntime.ClientInfo{Name: "large", Version: "1"},
		}
	case "tools/list":
		*target.(*mcpruntime.ListToolsResult) = mcpruntime.ListToolsResult{
			Tools: append([]mcpruntime.Tool(nil), t.tools...),
		}
	case "tools/call":
		t.calls.Add(1)
		*target.(*mcpruntime.CallToolResult) = mcpruntime.CallToolResult{
			Content: []mcpruntime.Content{{Type: "text", Text: "called"}},
		}
	case "ping":
	}
	return nil
}

func (*largeCatalogTransport) Notify(context.Context, string, any) error { return nil }
func (*largeCatalogTransport) Close(context.Context) error               { return nil }
func (*largeCatalogTransport) StderrTail() string                        { return "" }

func TestAdapterRevokesOpenServerAndRestoresAfterProbe(t *testing.T) {
	transport := &scriptedTransport{}
	transportErr := atomic.Bool{}
	pool := mcpruntime.NewPool(func(
		context.Context,
		string,
		mcpruntime.ServerConfig,
	) (mcpruntime.Transport, error) {
		return &failureSwitchTransport{
			scriptedTransport: transport, fail: &transportErr,
		}, nil
	})
	server := mcpruntime.ServerConfig{
		Transport: "stdio", Command: "scripted", HostTrusted: true,
		Tools: map[string]mcpruntime.ToolBinding{
			"danger": {
				Capability: "read", AccessMode: "read",
				ParallelPolicy: "concurrent", SandboxRequirement: "none",
			},
		},
		ConnectTimeout: time.Second, CallTimeout: time.Second,
		ShutdownTimeout: time.Second,
		CircuitBreaker: mcpruntime.CircuitBreakerConfig{
			FailureThreshold: 1, Cooldown: 5 * time.Millisecond,
		},
	}
	if _, err := pool.Reload(t.Context(), mcpruntime.Config{
		Version: mcpruntime.ConfigVersion,
		Servers: map[string]mcpruntime.ServerConfig{"remote": server},
	}); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	adapter, err := NewAdapter(registry, pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := registry.Resolve("mcp_remote_danger"); err != nil {
		t.Fatal(err)
	}
	transportErr.Store(true)
	connection, _ := pool.Connection("remote")
	if _, err := connection.CallTool(
		t.Context(), "danger", json.RawMessage(`{}`),
	); err == nil {
		t.Fatal("failing MCP call succeeded")
	}
	if err := adapter.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := registry.Resolve(
		"mcp_remote_danger",
	); !errors.Is(err, tool.ErrToolRevoked) {
		t.Fatalf("open server tool error = %v, want revoked", err)
	}
	time.Sleep(10 * time.Millisecond)
	transportErr.Store(false)
	if err := pool.ProbeOpen(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := registry.Resolve("mcp_remote_danger"); err != nil {
		t.Fatalf("recovered server tool was not restored: %v", err)
	}
}

func TestExecutorRejectsRevokedCapabilityBeforeRemoteCall(t *testing.T) {
	executor := &executor{
		entry: mcpruntime.CatalogEntry{
			RemoteName: "danger",
			Authority: func(context.Context) error {
				return errors.New("revoked")
			},
		},
	}
	if _, err := executor.Execute(
		t.Context(), json.RawMessage(`{}`),
	); err == nil || !strings.Contains(err.Error(), "authority") {
		t.Fatalf("revoked MCP execution error = %v", err)
	}
}

func TestAsyncSyncQuarantinesStaleCatalogAndRecovers(t *testing.T) {
	pool := mcpruntime.NewPool(func(
		context.Context,
		string,
		mcpruntime.ServerConfig,
	) (mcpruntime.Transport, error) {
		return &scriptedTransport{}, nil
	})
	config := mcpruntime.Config{
		Version: mcpruntime.ConfigVersion,
		Servers: map[string]mcpruntime.ServerConfig{
			"remote": {
				Transport: "stdio", Command: "scripted", HostTrusted: true,
				Tools: map[string]mcpruntime.ToolBinding{
					"danger": {
						Capability: "read", AccessMode: "read",
						ParallelPolicy: "concurrent", SandboxRequirement: "none",
					},
				},
			},
		},
	}
	if _, err := pool.Reload(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	adapter, err := NewAdapter(registry, pool)
	if err != nil {
		t.Fatal(err)
	}

	before, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := before.Binding("mcp_remote_danger")
	if !ok {
		t.Fatal("MCP tool binding is missing")
	}
	_, _, existing, err := registry.Resolve("list_mcp_resources")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Revoke("mcp:helpers", "list_mcp_resources", 0); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Reconcile(
		"fixture",
		snapshot.Generation,
		[]tool.Registration{tool.NewRegistration(existing)},
	); err != nil {
		t.Fatal(err)
	}

	pool.Invalidate()
	if _, err := pool.Reload(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	syncErr := adapter.Sync()
	if syncErr == nil {
		t.Fatal("Sync error was not returned")
	}
	if _, _, _, err := registry.ResolveBound(
		"mcp_remote_danger",
		binding,
	); !errors.Is(err, tool.ErrCatalogStale) &&
		!errors.Is(err, tool.ErrToolRevoked) {
		t.Fatalf("old MCP binding after quarantine error = %v", err)
	}
	if registrations := registry.SourceRegistrations("mcp:remote"); len(registrations) != 0 {
		t.Fatalf("quarantined MCP registrations = %d, want 0", len(registrations))
	}

	snapshot, err = registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Reconcile("fixture", snapshot.Generation, nil); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := registry.Resolve("mcp_remote_danger"); err != nil {
		t.Fatalf("MCP tool was not restored after recovery: %v", err)
	}
}

func TestAdapterReplacesExecutorWhenConnectionChanges(t *testing.T) {
	pool := mcpruntime.NewPool(func(
		context.Context,
		string,
		mcpruntime.ServerConfig,
	) (mcpruntime.Transport, error) {
		return &scriptedTransport{}, nil
	})
	config := mcpruntime.Config{
		Version: mcpruntime.ConfigVersion,
		Servers: map[string]mcpruntime.ServerConfig{
			"remote": {
				Transport: "stdio", Command: "scripted", HostTrusted: true,
				Tools: map[string]mcpruntime.ToolBinding{
					"danger": {
						Capability: "read", AccessMode: "read",
						ParallelPolicy: "concurrent", SandboxRequirement: "none",
					},
				},
			},
		},
	}
	if _, err := pool.Reload(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	adapter, err := NewAdapter(registry, pool)
	if err != nil {
		t.Fatal(err)
	}
	before, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	beforeEntry, ok := before.Lookup("mcp_remote_danger")
	if !ok {
		t.Fatal("MCP tool is missing before reconnect")
	}
	_, _, beforeExecutor, err := registry.Resolve("mcp_remote_danger")
	if err != nil {
		t.Fatal(err)
	}

	pool.Invalidate()
	if _, err := pool.Reload(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Sync(); err != nil {
		t.Fatal(err)
	}
	after, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	afterEntry, ok := after.Lookup("mcp_remote_danger")
	if !ok || afterEntry.Revision <= beforeEntry.Revision {
		t.Fatalf("MCP entry after reconnect = %+v, before = %+v", afterEntry, beforeEntry)
	}
	_, _, afterExecutor, err := registry.Resolve("mcp_remote_danger")
	if err != nil {
		t.Fatal(err)
	}
	if beforeExecutor == afterExecutor {
		t.Fatal("MCP reconnect reused the old executor")
	}
}

func TestCatalogNotificationReconcilesOnlyServerSource(t *testing.T) {
	var created atomic.Int64
	var first *notifyingTransport
	pool := mcpruntime.NewPool(func(
		context.Context,
		string,
		mcpruntime.ServerConfig,
	) (mcpruntime.Transport, error) {
		version := created.Add(1)
		transport := &notifyingTransport{description: fmt.Sprintf("version %d", version)}
		if version == 1 {
			first = transport
		}
		return transport, nil
	})
	server := mcpruntime.ServerConfig{
		Transport: "stdio", Command: "scripted", HostTrusted: true,
		Tools: map[string]mcpruntime.ToolBinding{
			"echo": {
				Capability: "read", AccessMode: "read",
				ParallelPolicy: "concurrent", SandboxRequirement: "none",
			},
		},
		ConnectTimeout: time.Second, CallTimeout: time.Second,
		ShutdownTimeout: time.Second,
	}
	if _, err := pool.Reload(t.Context(), mcpruntime.Config{
		Version: mcpruntime.ConfigVersion,
		Servers: map[string]mcpruntime.ServerConfig{"remote": server},
	}); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	adapter, err := NewAdapter(registry, pool)
	if err != nil {
		t.Fatal(err)
	}
	before, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	beforeEntry, _ := before.Lookup("mcp_remote_echo")
	first.notify(mcpruntime.Notification{Method: "notifications/tools/list_changed"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		catalog := pool.Catalog()
		if len(catalog) == 1 &&
			catalog[0].Tool.Description == "version 2" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	stale, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	staleEntry, _ := stale.Lookup("mcp_remote_echo")
	if staleEntry.Revision != beforeEntry.Revision {
		t.Fatal("MCP watcher changed runtime authority directly")
	}
	if err := adapter.Sync(); err != nil {
		t.Fatal(err)
	}
	after, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := after.Lookup("mcp_remote_echo")
	if !ok || entry.Revision <= beforeEntry.Revision ||
		entry.Descriptor.Description !=
			"[Host-trusted MCP server process] version 2" {
		t.Fatalf("replacement entry = %+v", entry)
	}
	if entry.State != tool.CatalogEntryDeferred ||
		entry.Descriptor.Availability != tool.AvailabilityDeferred {
		t.Fatalf("replacement entry = %+v, want deferred", entry)
	}
}

type notifyingTransport struct {
	description string
	handler     func(mcpruntime.Notification)
}

func (t *notifyingTransport) SetNotificationHandler(handler func(mcpruntime.Notification)) {
	t.handler = handler
}

func (t *notifyingTransport) notify(notification mcpruntime.Notification) {
	if t.handler != nil {
		t.handler(notification)
	}
}

func (t *notifyingTransport) Request(
	_ context.Context,
	method string,
	_ any,
	target any,
) error {
	switch method {
	case "initialize":
		*target.(*mcpruntime.InitializeResult) = mcpruntime.InitializeResult{
			ProtocolVersion: mcpruntime.ProtocolVersion,
			Capabilities:    map[string]any{"tools": map[string]any{}},
			ServerInfo:      mcpruntime.ClientInfo{Name: "notify", Version: "1"},
		}
	case "tools/list":
		*target.(*mcpruntime.ListToolsResult) = mcpruntime.ListToolsResult{
			Tools: []mcpruntime.Tool{{
				Name: "echo", Description: t.description,
				InputSchema: map[string]any{"type": "object"},
			}},
		}
	case "tools/call":
		*target.(*mcpruntime.CallToolResult) = mcpruntime.CallToolResult{}
	case "ping":
	}
	return nil
}

func (*notifyingTransport) Notify(context.Context, string, any) error { return nil }
func (*notifyingTransport) Close(context.Context) error               { return nil }
func (*notifyingTransport) StderrTail() string                        { return "" }

type failureSwitchTransport struct {
	scriptedTransport *scriptedTransport
	fail              *atomic.Bool
}

func (t *failureSwitchTransport) Request(
	ctx context.Context,
	method string,
	params any,
	target any,
) error {
	if method == "ping" {
		if t.fail.Load() {
			return errors.New("probe failed")
		}
		return nil
	}
	if method == "tools/call" && t.fail.Load() {
		t.scriptedTransport.calls.Add(1)
		return errors.New("call failed")
	}
	return t.scriptedTransport.Request(ctx, method, params, target)
}

func (t *failureSwitchTransport) Notify(
	ctx context.Context,
	method string,
	params any,
) error {
	return t.scriptedTransport.Notify(ctx, method, params)
}

func (t *failureSwitchTransport) Close(ctx context.Context) error {
	return t.scriptedTransport.Close(ctx)
}

func (t *failureSwitchTransport) StderrTail() string {
	return t.scriptedTransport.StderrTail()
}
