package lsp

import (
	"context"
	"errors"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type Tool struct {
	checker Checker
}

type input struct {
	Files   []string `json:"files"`
	Changes []Change `json:"changes"`
}

func RegisterWithBackend(registry *tool.Registry, root string, backend sandbox.Backend) error {
	if backend == nil {
		return errors.New("LSP tools require an injected sandbox backend")
	}
	backend, err := sandbox.BindPolicy(backend, sandbox.Options{WorkspaceRoot: root})
	if err != nil {
		return err
	}
	registry.SetSandboxBackend(backend)
	instance := &Tool{
		checker: Checker{Root: root, Sandbox: backend},
	}
	executor, err := instance.typedExecutor()
	if err != nil {
		return err
	}
	if err := registry.Register(executor); err != nil {
		return err
	}
	return registerIDETools(registry, root, backend)
}

func (t *Tool) Descriptor() tool.Descriptor {
	servers := AvailableServers()
	availability := tool.AvailabilityAvailable
	unavailableReason := ""
	if len(servers) == 0 {
		availability = tool.AvailabilityUnavailable
		unavailableReason = "no supported language server is installed"
	}
	return tool.Descriptor{
		Name: "lsp_diagnostics",
		Description: "Analyze source files through an installed language server and return " +
			"structured diagnostics. Supported servers: " + strings.Join(servers, ", ") +
			". One request must contain files handled by the same server; changes are in-memory only.",
		Visibility: tool.VisibleModel,
		DiscoveryTerms: []string{
			"lsp", "language server", "diagnostics", "type error",
			"语言服务", "诊断", "类型错误",
		},
		Capability: tool.CapabilityProcess, AccessMode: tool.AccessTree,
		ResourceResolver:   tool.ResourceResolver{Templates: languageServerResources(servers)},
		ParallelPolicy:     tool.ParallelSerial,
		RepeatPolicy:       tool.RepeatExecute,
		SandboxRequirement: tool.SandboxStrong, Availability: availability,
		UnavailableReason: unavailableReason,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"files": map[string]any{
					"type": "array", "minItems": 1,
					"items": map[string]any{"type": "string", "minLength": 1},
				},
				"changes": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":    map[string]any{"type": "string", "minLength": 1},
							"text":    map[string]any{"type": "string"},
							"version": map[string]any{"type": "integer", "minimum": 1},
						},
						"required":             []string{"path", "text"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"files"},
			"additionalProperties": false,
		},
	}
}

func serverProbePath(server string) string {
	switch server {
	case "gopls":
		return "probe.go"
	case "clangd":
		return "probe.cpp"
	case "rust-analyzer":
		return "probe.rs"
	case "pyright":
		return "probe.py"
	case "typescript-language-server":
		return "probe.ts"
	case "jdtls":
		return "Probe.java"
	default:
		return ""
	}
}

func (t *Tool) typedExecutor() (tool.Executor, error) {
	return typed.Define(typed.Spec[input, []Diagnostic]{
		Descriptor:  t.Descriptor(),
		Disposition: tool.DispositionWaitForTeardown,
		Run: func(ctx context.Context, value input) ([]Diagnostic, error) {
			return t.checker.Analyze(ctx, value.Files, value.Changes)
		},
		Metadata: func(values []Diagnostic) map[string]any {
			return map[string]any{"diagnostics": values, "diagnostic_count": len(values)}
		},
	})
}
