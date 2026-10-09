package interact_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/internal/observability/telemetry"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
)

func TestRequestUserInputRejectsBlankAndDuplicateOptions(t *testing.T) {
	host := interact.NewHost(time.Minute)
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: host, Workspace: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "request_user_input",
		Arguments: mustJSON(map[string]any{
			"prompt": "pick", "options": []string{"a", " ", "b"},
		}),
	})
	if err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("blank option err = %v", err)
	}
	_, err = tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "request_user_input",
		Arguments: mustJSON(map[string]any{
			"prompt": "pick", "options": []string{"Yes", "yes"},
		}),
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate option err = %v", err)
	}
}

func TestRequestUserInputDescriptorRequiresStructuredWait(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: interact.NewHost(0), Workspace: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := snapshot.Lookup("request_user_input")
	if !ok ||
		!strings.Contains(entry.Descriptor.Description, "block the current Turn") ||
		!strings.Contains(entry.Descriptor.Description, "ordinary final text") ||
		!strings.Contains(entry.Descriptor.Description, "short, self-contained") {
		t.Fatalf("request_user_input descriptor = %+v", entry.Descriptor)
	}
	properties := entry.Descriptor.InputSchema["properties"].(map[string]any)
	promptDescription, _ := properties["prompt"].(map[string]any)["description"].(string)
	if !strings.Contains(promptDescription, "not inside options") {
		t.Fatalf("request_user_input prompt description = %q", promptDescription)
	}
	options := properties["options"].(map[string]any)
	optionsDescription, _ := options["description"].(string)
	if options["uniqueItems"] != true || options["maxItems"] != float64(12) ||
		!strings.Contains(optionsDescription, "Short, distinct labels") {
		t.Fatalf("request_user_input options schema = %+v", options)
	}
}

func TestRequestUserInputFailClosedWithoutHost(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: interact.NewHost(0), Workspace: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      "request_user_input",
		Arguments: mustJSON(map[string]any{"prompt": "hi"}),
	})
	var unavailable interact.HostUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestRequestUserInputBlocksUntilHostReply(t *testing.T) {
	host := interact.NewHost(time.Minute)
	seen := make(chan interact.Request, 1)
	host.SetEmitter(func(_ context.Context, req interact.Request) error {
		seen <- req
		return nil
	})
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: host, Workspace: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan tool.Result, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := tooltest.Execute(t.Context(), registry, tool.Call{
			Name:      "request_user_input",
			Arguments: mustJSON(map[string]any{"prompt": "Continue?", "options": []any{"yes", "no"}}),
		})
		if err != nil {
			errCh <- err
			return
		}
		done <- result
	}()
	req := <-seen
	if req.Prompt != "Continue?" {
		t.Fatalf("req = %+v", req)
	}
	if err := host.Reply(interact.Reply{RequestID: req.RequestID, Answer: "yes"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		t.Fatal(err)
	case result := <-done:
		if result.IsError || !strings.Contains(result.Content, `"answer":"yes"`) {
			t.Fatalf("result = %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

func TestUpdatePlanAppearsInContextReceipts(t *testing.T) {
	var eng *agentengine.Engine
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: interact.NewHost(0),
		OnPlan: func(plan interact.Plan) error {
			eng.ApplyPlan(plan)
			return nil
		}, Workspace: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	var err error
	eng, err = agentengine.New(agentengine.Options{ProviderConfig: agentengine.ProviderConfig{Provider: &noopProvider{}, Route: testRoute(t),
		MaxOutputTokens: 64}, ContextConfig: agentengine.ContextConfig{StaticContextReceipts: []promptcontext.Receipt{{
		Kind: promptcontext.PartitionBase, SourcePath: "builtin://base-system",
	}}}, ToolConfig: agentengine.ToolConfig{Tools: registry}, TelemetryConfig: agentengine.TelemetryConfig{Metrics: telemetry.NewMetrics()}, LifecycleConfig: agentengine.LifecycleConfig{TurnCoordinatorRuntime: turnkernel.NewEphemeralCoordinatorRuntime()},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := execute(t, registry, "update_plan", map[string]any{
		"title": "P24",
		"steps": []any{
			map[string]any{"title": "wire input", "status": "done"},
			map[string]any{"title": "wire plan", "status": "in_progress"},
		},
		"objective": "ship interact tools", "verification_plan": "go test ./internal/adapter/tool/interact",
		"critical_files": []any{"internal/adapter/tool/interact/interact.go"},
		"handoff_packet": "next: land relay",
	})
	if result.IsError || result.Metadata["plan_delta"] != true ||
		result.Metadata["submitted_plan"] != nil ||
		!strings.Contains(result.Content, "wire input") {
		t.Fatalf("result = %+v", result)
	}
	if _, err := interact.ParseSubmittedPlan([]byte(result.Content)); err != nil {
		t.Fatalf("update_plan result is not a structured Plan Artifact: %v", err)
	}
	if !strings.Contains(result.Content, "ship interact tools") {
		t.Fatalf("rich fields missing: %+v", result)
	}
	found := false
	for _, receipt := range eng.ContextReceipts() {
		if receipt.Kind == promptcontext.PartitionPlan {
			found = true
		}
	}
	if !found {
		t.Fatalf("receipts = %+v", eng.ContextReceipts())
	}
}

func TestSubmitPlanNormalizesStructuredSteps(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: interact.NewHost(0), Workspace: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	result := execute(t, registry, "submit_plan", map[string]any{
		"version": 1,
		"title":   "Parser change",
		"steps": []any{
			map[string]any{
				"id": "inspect", "title": "Inspect parser",
				"expected_evidence": "Relevant tests identified",
			},
			map[string]any{
				"id": "implement", "title": "Implement parser",
				"dependencies":   []any{"inspect"},
				"affected_files": []any{"parser.go"},
			},
		},
	})
	if result.IsError || result.Metadata["plan_delta"] != true ||
		result.Metadata["submitted_plan"] != true ||
		!strings.Contains(result.Content, `"id":"implement"`) {
		t.Fatalf("result = %+v", result)
	}
}

func TestSubmitPlanCapturesDirectoryBaseline(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "include"), 0o755); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: interact.NewHost(0), Workspace: workspace,
	}); err != nil {
		t.Fatal(err)
	}
	result := execute(t, registry, "submit_plan", map[string]any{
		"version":        1,
		"title":          "Directory baseline",
		"critical_files": []any{"include"},
		"steps": []any{map[string]any{
			"id": "inspect", "title": "Inspect include directory",
		}},
	})
	var plan interact.SubmittedPlan
	if err := json.Unmarshal([]byte(result.Content), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.FileBaseline) != 1 ||
		plan.FileBaseline[0].Path != "include" ||
		!plan.FileBaseline[0].Directory ||
		plan.FileBaseline[0].Missing ||
		plan.FileBaseline[0].Digest != "" {
		t.Fatalf("directory baseline = %+v", plan.FileBaseline)
	}
}

func TestDebugSubmitPlanDirectoryBaselineEvidence(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "include"), 0o755); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: interact.NewHost(0), Workspace: workspace,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "submit_plan",
		Arguments: mustJSON(map[string]any{
			"version":        1,
			"title":          "Directory baseline",
			"critical_files": []any{"include"},
			"steps": []any{map[string]any{
				"id": "inspect", "title": "Inspect include directory",
			}},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("submit_plan result = %+v", result)
	}
}

func TestUpdatePlanRejectsEmptyStepTitle(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: interact.NewHost(0), Workspace: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      "update_plan",
		Arguments: mustJSON(map[string]any{"steps": []any{map[string]any{"title": "  "}}}),
	})
	if err == nil || !strings.Contains(err.Error(), "must have a title") {
		t.Fatalf("blank step title err = %v", err)
	}
}

func TestProjectMapBounded(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg", "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "a", "x.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: interact.NewHost(0), Workspace: root,
	}); err != nil {
		t.Fatal(err)
	}
	result := execute(t, registry, "project_map", map[string]any{"max_depth": 3, "limit": 50})
	if result.IsError || !strings.Contains(result.Content, "pkg/") {
		t.Fatalf("result = %+v", result)
	}
}

func TestProjectMapSharesTheSearchEnumeration(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		"main.go":                   "package main\n",
		"pkg/a/x.go":                "package a\n",
		"pkg/a/deep/deeper/y.go":    "package deeper\n",
		"node_modules/dep/index.js": "module.exports = 1\n",
		".qcode/runtime-state.db":   "state\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: interact.NewHost(0), Workspace: root,
	}); err != nil {
		t.Fatal(err)
	}

	result := execute(t, registry, "project_map", map[string]any{"max_depth": 2})
	var payload struct {
		Entries []string `json:"entries"`
	}
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatal(err)
	}
	// Dependency trees and the runtime's own state are left out, and the depth
	// bound stops at directories rather than listing what is under them.
	want := []string{"main.go", "pkg/", "pkg/a/"}
	if len(payload.Entries) != len(want) {
		t.Fatalf("entries = %#v", payload.Entries)
	}
	for index := range want {
		if payload.Entries[index] != want[index] {
			t.Fatalf("entries = %#v, want %#v", payload.Entries, want)
		}
	}

	scoped := execute(t, registry, "project_map", map[string]any{
		"path": "pkg/a/deep", "max_depth": 5,
	})
	if !strings.Contains(scoped.Content, "pkg/a/deep/deeper/y.go") ||
		strings.Contains(scoped.Content, "main.go") {
		t.Fatalf("scoped result = %+v", scoped)
	}
	if _, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      "project_map",
		Arguments: []byte(`{"path":"../outside"}`),
	}); err == nil {
		t.Fatal("a path outside the workspace was accepted")
	}
}

func TestImageAnalyzeUnavailable(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: interact.NewHost(0), Workspace: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, d := range registry.Descriptors(tool.VisibleModel) {
		if d.Name == "image_analyze" {
			if d.Availability != tool.AvailabilityUnavailable {
				t.Fatalf("image_analyze should be unavailable: %+v", d)
			}
			if d.UnavailableReason != interact.VisionUnavailableReason {
				t.Fatalf("reason = %q", d.UnavailableReason)
			}
			return
		}
	}
	t.Fatal("image_analyze missing")
}

func TestImageAnalyzeAvailableWithFakeClient(t *testing.T) {
	root := t.TempDir()
	imagePath := filepath.Join(root, "shot.png")
	if err := os.WriteFile(imagePath, []byte("png-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: interact.NewHost(0),
		Vision: interact.FuncVision(func(_ context.Context, path, prompt string) (string, error) {
			if !strings.HasSuffix(path, "shot.png") {
				t.Fatalf("path = %q", path)
			}
			return "fixture:" + prompt, nil
		}), Workspace: root,
	}); err != nil {
		t.Fatal(err)
	}
	for _, d := range registry.Descriptors(tool.VisibleModel) {
		if d.Name == "image_analyze" {
			if d.Availability != tool.AvailabilityAvailable {
				t.Fatalf("want available: %+v", d)
			}
			break
		}
	}
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      "image_analyze",
		Arguments: mustJSON(map[string]any{"path": "shot.png", "prompt": "what"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || result.Content != "fixture:what" {
		t.Fatalf("result = %+v", result)
	}
}

func TestImageAnalyzeRejectsEscapingPath(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: interact.NewHost(0),
		Vision: interact.FuncVision(func(context.Context, string, string) (string, error) {
			return "should-not-run", nil
		}), Workspace: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      "image_analyze",
		Arguments: mustJSON(map[string]any{"path": "../secret.png"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "escapes") {
		t.Fatalf("result = %+v", result)
	}
}

func TestUpdatePlanRejectsUnchangedStepSignature(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: interact.NewHost(0), Workspace: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{
		"title": "P",
		"steps": []any{
			map[string]any{"id": "one-id", "title": "one", "status": "done"},
			map[string]any{"id": "two-id", "title": "two", "status": "pending"},
		},
	}
	first := execute(t, registry, "update_plan", input)
	if first.IsError || first.Metadata["plan_delta"] != true {
		t.Fatalf("first update = %+v", first)
	}
	second := execute(t, registry, "update_plan", map[string]any{
		"title":           "P",
		"context_summary": "rewritten prose",
		"steps": []any{
			map[string]any{"id": "one-id", "title": "one", "status": "done"},
			map[string]any{"id": "two-id", "title": "two", "status": "pending"},
		},
	})
	if !second.IsError ||
		second.Metadata["reason"] != "plan_progress_unchanged" ||
		second.Metadata["required_action"] !=
			"finish_open_plan_steps_or_declare_incomplete" ||
		second.Metadata["retry_original"] != false ||
		second.Metadata["plan_delta"] != false {
		t.Fatalf("unchanged plan = %+v", second)
	}
	progressed := execute(t, registry, "update_plan", map[string]any{
		"steps": []any{
			map[string]any{"id": "one-id", "title": "one", "status": "done"},
			map[string]any{"id": "two-id", "title": "two", "status": "done"},
		},
	})
	if progressed.IsError || progressed.Metadata["plan_delta"] != true {
		t.Fatalf("progressed plan = %+v", progressed)
	}
}

func execute(t *testing.T, registry *tool.Registry, name string, input map[string]any) tool.Result {
	t.Helper()
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: name, Arguments: mustJSON(input),
	})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return result
}

func mustJSON(value map[string]any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

type passthroughBackend struct{}

func (passthroughBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "fixture", Backend: "passthrough",
		Available: true,
		Effective: securitymodel.Controls{
			FilesystemRead: securitymodel.FilesystemReadDeclaredRoots,

			FilesystemWrite: securitymodel.FilesystemWriteExactPaths,

			Network: securitymodel.NetworkDenied, ProcessTree: securitymodel.ProcessTreeGroupKill, CrossProcess: securitymodel.CrossProcessUnrestricted,
			Syscall: securitymodel.SyscallDenyDangerous,
			IPC:     securitymodel.IPCUnrestricted, PathIdentity: securitymodel.PathIdentityDescriptorRelative, ArtifactOrigin: securitymodel.ArtifactOriginUnverifiedPath, DurableRecovery: securitymodel.DurableRecoveryMemoryOnly,
		},
	}
}

func (passthroughBackend) Prepare(_ context.Context, command sandbox.Command) (sandbox.Command, error) {
	return command, nil
}

type noopProvider struct{}

func (noopProvider) Stream(context.Context, provider.ModelRequest) (provider.Stream, error) {
	return nil, errors.New("noop provider")
}

func testRoute(t *testing.T) model.ReadyRoute {
	t.Helper()
	catalog, err := model.NewCatalog(model.Provider{
		ID: "test", Adapter: model.AdapterOpenAICompatible, Endpoint: "http://127.0.0.1:1",
		Protocol: model.ProtocolOpenAIChat, Provenance: model.ProvenanceFixture,
		Models: map[string]model.Model{"model": {
			ID: "model", CanonicalID: "model", WireID: "model",
			Limits:       model.Limits{ContextTokens: 4096, MaxOutputTokens: 1024},
			Capabilities: model.Capabilities{Streaming: true, ToolCalls: true},
			Pricing: model.Pricing{
				InputPerMillion: 1, OutputPerMillion: 1,
				Currency: "USD", Known: true, Provenance: model.ProvenanceFixture,
			},
			Provenance: model.ProvenanceFixture,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := model.NewResolver(catalog)
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(model.RouteRequest{ProviderID: "test", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	return route
}
