package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type ideTool struct {
	kind    string
	checker Checker
}

type ideInput struct {
	Path         string `json:"path"`
	Line         int    `json:"line"`
	Character    int    `json:"character"`
	EndLine      int    `json:"end_line"`
	EndCharacter int    `json:"end_character"`
	NewName      string `json:"new_name"`
}

func registerIDETools(
	registry *tool.Registry,
	root string,
	backend sandbox.Backend,
) error {
	for _, kind := range []string{
		"lsp_hover", "lsp_format_edits", "lsp_code_actions", "lsp_rename_edits",
	} {
		instance := &ideTool{
			kind: kind,
			checker: Checker{
				Root: root, Sandbox: backend,
			},
		}
		executor, err := instance.typedExecutor()
		if err != nil {
			return err
		}
		if err := registry.Register(executor); err != nil {
			return err
		}
	}
	return nil
}

func (t *ideTool) Descriptor() tool.Descriptor {
	servers := AvailableServers()
	availability := tool.AvailabilityAvailable
	unavailableReason := ""
	if len(servers) == 0 {
		availability = tool.AvailabilityUnavailable
		unavailableReason = "no supported language server is installed"
	}
	properties := map[string]any{
		"path": map[string]any{"type": "string", "minLength": 1},
	}
	required := []string{"path"}
	if t.kind != "lsp_format_edits" {
		properties["line"] = map[string]any{"type": "integer", "minimum": 1}
		properties["character"] = map[string]any{"type": "integer", "minimum": 1}
		required = append(required, "line", "character")
	}
	if t.kind == "lsp_code_actions" {
		properties["end_line"] = map[string]any{"type": "integer", "minimum": 1}
		properties["end_character"] = map[string]any{"type": "integer", "minimum": 1}
	}
	if t.kind == "lsp_rename_edits" {
		properties["new_name"] = map[string]any{"type": "string", "minLength": 1}
		required = append(required, "new_name")
	}
	description := map[string]string{
		"lsp_hover":        "Return language-server hover information at a source position",
		"lsp_format_edits": "Return language-server formatting edits without writing files",
		"lsp_code_actions": "Return language-server code actions for a source range",
		"lsp_rename_edits": "Return a language-server workspace rename edit without writing files",
	}[t.kind]
	return tool.Descriptor{
		Name: t.kind, Description: description + ". Installed servers: " + strings.Join(servers, ", "),
		DiscoveryTerms:     ideDiscoveryTerms(t.kind),
		Visibility:         tool.VisibleModel,
		Capability:         tool.CapabilityProcess,
		AccessMode:         tool.AccessTree,
		ResourceResolver:   tool.ResourceResolver{Templates: languageServerResources(servers)},
		ParallelPolicy:     tool.ParallelSerial,
		RepeatPolicy:       tool.RepeatExecute,
		SandboxRequirement: tool.SandboxStrong,
		Availability:       availability,
		UnavailableReason:  unavailableReason,
		InputSchema: map[string]any{
			"type": "object", "properties": properties, "required": required,
			"additionalProperties": false,
		},
	}
}

func ideDiscoveryTerms(kind string) []string {
	switch kind {
	case "lsp_hover":
		return []string{"hover", "type information", "悬停", "类型信息"}
	case "lsp_format_edits":
		return []string{"format document", "formatting", "格式化", "代码格式"}
	case "lsp_code_actions":
		return []string{"code action", "quick fix", "代码操作", "快速修复"}
	default:
		return []string{"rename symbol", "rename", "重命名符号", "重构"}
	}
}

func (t *ideTool) typedExecutor() (tool.Executor, error) {
	return typed.Define(typed.Spec[ideInput, IDEResult]{
		Descriptor:  t.Descriptor(),
		Disposition: tool.DispositionWaitForTeardown,
		Run: func(ctx context.Context, input ideInput) (IDEResult, error) {
			query := IDEQuery{
				Path: input.Path, Line: input.Line, Character: input.Character,
				EndLine: input.EndLine, EndCharacter: input.EndCharacter,
				NewName: input.NewName,
			}
			switch t.kind {
			case "lsp_hover":
				return t.checker.Hover(ctx, query)
			case "lsp_format_edits":
				return t.checker.Formatting(ctx, query)
			case "lsp_code_actions":
				return t.checker.CodeActions(ctx, query)
			case "lsp_rename_edits":
				return t.checker.Rename(ctx, query)
			default:
				return IDEResult{}, fmt.Errorf("unsupported LSP tool %q", t.kind)
			}
		},
		Metadata: func(result IDEResult) map[string]any {
			return map[string]any{"method": result.Method, "server": result.Server}
		},
	})
}

func languageServerResources(servers []string) []tool.ResourceTemplate {
	resources := []tool.ResourceTemplate{
		{Kind: "repo", ID: ".", Access: tool.AccessRead, Tree: true},
		{Kind: "process", ID: "lsp", Access: tool.AccessWrite, Tree: true},
	}
	for _, server := range servers {
		spec, err := ResolveServer(serverProbePath(server))
		if err == nil {
			resources = append(resources, tool.ResourceTemplate{
				Kind: "directory", ID: filepath.Dir(spec.Binary),
				Access: tool.AccessRead, Tree: true,
			})
		}
	}
	return resources
}

type IDEQuery struct {
	Path         string
	Line         int
	Character    int
	EndLine      int
	EndCharacter int
	NewName      string
}

type IDEResult struct {
	Method string          `json:"method"`
	Server string          `json:"server"`
	Result json.RawMessage `json:"result"`
}

func (c Checker) Hover(ctx context.Context, query IDEQuery) (IDEResult, error) {
	return c.documentRequest(ctx, "textDocument/hover", query)
}

func (c Checker) Formatting(ctx context.Context, query IDEQuery) (IDEResult, error) {
	return c.documentRequest(ctx, "textDocument/formatting", query)
}

func (c Checker) CodeActions(ctx context.Context, query IDEQuery) (IDEResult, error) {
	return c.documentRequest(ctx, "textDocument/codeAction", query)
}

func (c Checker) Rename(ctx context.Context, query IDEQuery) (IDEResult, error) {
	if strings.TrimSpace(query.NewName) == "" {
		return IDEResult{}, errors.New("rename requires a non-empty new name")
	}
	return c.documentRequest(ctx, "textDocument/rename", query)
}

func (c Checker) documentRequest(
	ctx context.Context,
	method string,
	query IDEQuery,
) (IDEResult, error) {
	if strings.TrimSpace(query.Path) == "" {
		return IDEResult{}, errors.New("language server request requires a relative path")
	}
	if method != "textDocument/formatting" &&
		(query.Line < 1 || query.Character < 1) {
		return IDEResult{}, errors.New(
			"language server request requires 1-based line and character",
		)
	}
	resolved, err := c.forPaths([]string{query.Path})
	if err != nil {
		return IDEResult{}, err
	}
	c = resolved
	client, err := c.start(ctx)
	if err != nil {
		return IDEResult{}, err
	}
	defer client.close()

	var initialized struct {
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := client.call(ctx, "initialize", map[string]any{
		"processId": nil,
		"rootUri":   pathURI(client.root),
		"capabilities": map[string]any{"textDocument": map[string]any{
			"hover":      map[string]any{},
			"formatting": map[string]any{},
			"codeAction": map[string]any{},
			"rename":     map[string]any{"prepareSupport": true},
		}},
	}, &initialized, nil); err != nil {
		return IDEResult{}, fmt.Errorf("initialize language server: %w", err)
	}
	if err := client.notify("initialized", map[string]any{}); err != nil {
		return IDEResult{}, err
	}
	path, text, err := semanticDocument(client.root, query.Path)
	if err != nil {
		return IDEResult{}, err
	}
	uri := pathURI(path)
	if err := client.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri": uri, "languageId": languageID(path), "version": 1, "text": text,
		},
	}); err != nil {
		return IDEResult{}, err
	}
	params := map[string]any{"textDocument": map[string]any{"uri": uri}}
	position := map[string]any{
		"line": query.Line - 1, "character": query.Character - 1,
	}
	switch method {
	case "textDocument/formatting":
		params["options"] = map[string]any{
			"tabSize": 4, "insertSpaces": true, "trimTrailingWhitespace": true,
			"insertFinalNewline": true, "trimFinalNewlines": true,
		}
	case "textDocument/codeAction":
		endLine, endCharacter := query.EndLine, query.EndCharacter
		if endLine < 1 {
			endLine, endCharacter = query.Line, query.Character
		}
		params["range"] = map[string]any{
			"start": position,
			"end": map[string]any{
				"line": endLine - 1, "character": max(endCharacter-1, 0),
			},
		}
		params["context"] = map[string]any{"diagnostics": []any{}}
	default:
		params["position"] = position
	}
	if method == "textDocument/rename" {
		params["newName"] = query.NewName
	}
	var raw json.RawMessage
	if err := client.call(ctx, method, params, &raw, nil); err != nil {
		return IDEResult{}, err
	}
	if err := client.call(ctx, "shutdown", nil, nil, nil); err != nil {
		return IDEResult{}, err
	}
	if err := client.notify("exit", nil); err != nil {
		return IDEResult{}, err
	}
	client.finish(500 * time.Millisecond)
	server := strings.TrimSpace(initialized.ServerInfo.Name)
	if server == "" {
		server = filepath.Base(c.Binary)
	}
	return IDEResult{Method: method, Server: server, Result: raw}, nil
}
