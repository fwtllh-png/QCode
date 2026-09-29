package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/platform/symbols"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func (c Checker) Definition(
	ctx context.Context, query symbols.SemanticQuery,
) (symbols.SemanticResult, error) {
	return c.semantic(ctx, "textDocument/definition", query, false)
}

func (c Checker) References(
	ctx context.Context, query symbols.SemanticQuery, includeDeclaration bool,
) (symbols.SemanticResult, error) {
	return c.semantic(ctx, "textDocument/references", query, includeDeclaration)
}

func (c Checker) semantic(
	ctx context.Context,
	method string,
	query symbols.SemanticQuery,
	includeDeclaration bool,
) (symbols.SemanticResult, error) {
	if strings.TrimSpace(query.Path) == "" || query.Line < 1 || query.Character < 1 {
		return symbols.SemanticResult{}, errors.New(
			"semantic query requires a relative path and 1-based line/character",
		)
	}
	resolved, err := c.forPaths([]string{query.Path})
	if err != nil {
		return symbols.SemanticResult{}, err
	}
	c = resolved
	client, err := c.start(ctx)
	if err != nil {
		return symbols.SemanticResult{}, err
	}
	defer client.close()

	var initialized struct {
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := client.call(ctx, "initialize", map[string]any{
		"processId": nil,
		"rootUri":   pathURI(client.root),
		"capabilities": map[string]any{"textDocument": map[string]any{
			"definition": map[string]any{"linkSupport": true},
			"references": map[string]any{},
		}},
	}, &initialized, nil); err != nil {
		return symbols.SemanticResult{}, fmt.Errorf("initialize language server: %w", err)
	}
	if err := client.notify("initialized", map[string]any{}); err != nil {
		return symbols.SemanticResult{}, err
	}
	path, text, err := semanticDocument(client.root, query.Path)
	if err != nil {
		return symbols.SemanticResult{}, err
	}
	uri := pathURI(path)
	if err := client.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri": uri, "languageId": languageID(path), "version": 1, "text": text,
		},
	}); err != nil {
		return symbols.SemanticResult{}, err
	}
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position": map[string]any{
			"line": query.Line - 1, "character": query.Character - 1,
		},
	}
	if method == "textDocument/references" {
		params["context"] = map[string]any{"includeDeclaration": includeDeclaration}
	}
	var raw json.RawMessage
	if err := client.call(ctx, method, params, &raw, nil); err != nil {
		return symbols.SemanticResult{}, err
	}
	locations, err := semanticLocations(client.root, raw)
	if err != nil {
		return symbols.SemanticResult{}, err
	}
	if err := client.call(ctx, "shutdown", nil, nil, nil); err != nil {
		return symbols.SemanticResult{}, err
	}
	if err := client.notify("exit", nil); err != nil {
		return symbols.SemanticResult{}, err
	}
	client.finish(500 * time.Millisecond)
	name := strings.TrimSpace(initialized.ServerInfo.Name)
	if name == "" {
		name = filepath.Base(c.Binary)
	}
	if name == "" || name == "." {
		name = "gopls"
	}
	return symbols.SemanticResult{
		Locations: locations, Source: "lsp:" + name,
		Version: initialized.ServerInfo.Version, Confidence: "high",
	}, nil
}

func semanticDocument(root, name string) (string, string, error) {
	path, err := workspacePath(root, name)
	if err != nil {
		return "", "", err
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		return "", "", err
	}
	file, err := workspace.OpenFile(name)
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, (16<<20)+1))
	if err != nil {
		return "", "", err
	}
	if len(content) > 16<<20 {
		return "", "", fmt.Errorf("LSP document %q exceeds 16 MiB", name)
	}
	return path, string(content), nil
}

func semanticLocations(root string, raw json.RawMessage) ([]symbols.Location, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []symbols.Location{}, nil
	}
	var values []json.RawMessage
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, err
		}
	} else {
		values = []json.RawMessage{raw}
	}
	locations := make([]symbols.Location, 0, len(values))
	for _, value := range values {
		var location struct {
			URI                  string `json:"uri"`
			Range                Range  `json:"range"`
			TargetURI            string `json:"targetUri"`
			TargetSelectionRange Range  `json:"targetSelectionRange"`
		}
		if err := json.Unmarshal(value, &location); err != nil {
			return nil, err
		}
		uri, selected := location.URI, location.Range
		if uri == "" {
			uri, selected = location.TargetURI, location.TargetSelectionRange
		}
		path := uriPath(uri)
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		locations = append(locations, symbols.Location{
			Path: filepath.ToSlash(relative),
			Line: selected.Start.Line + 1, Character: selected.Start.Character + 1,
		})
	}
	return locations, nil
}
