package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/platform/symbols"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

var lspFixtureLog = flag.String("lsp-fixture-log", "", "internal LSP fixture log")

func TestCheckerRunsJSONRPCLifecycleAndReturnsEditedDiagnostics(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "lifecycle.log")
	checker := Checker{
		Binary: os.Args[0],
		Args: []string{
			"-test.run=^TestLSPFixtureProcess$",
			"-lsp-fixture-log=" + logPath,
		},
		Root: root, DiagnosticTimeout: 2 * time.Second, Sandbox: lspTestBackend{},
	}
	diagnostics, err := checker.Analyze(t.Context(), []string{"main.go"}, []Change{{
		Path: "main.go", Text: "package main\nvar broken =\n", Version: 2,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
	got := diagnostics[0]
	if got.File != "main.go" || got.Message != "after edit" || got.Severity != 1 || got.Code != "fixture" {
		t.Fatalf("diagnostic = %+v", got)
	}
	lifecycle, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"initialize", "initialized", "didOpen", "didChange", "shutdown", "exit"} {
		if !strings.Contains(string(lifecycle), event+"\n") {
			t.Fatalf("lifecycle missing %q:\n%s", event, lifecycle)
		}
	}
}

func TestCheckerRoutesCPPToInstalledClangd(t *testing.T) {
	if _, err := exec.LookPath("clangd"); err != nil {
		t.Skip("clangd unavailable")
	}
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "main.cpp"),
		[]byte("int main() { return missing_symbol; }\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	diagnostics, err := (Checker{
		Root: root, DiagnosticTimeout: 5 * time.Second, Sandbox: backend,
	}).Analyze(ctx, []string{"main.cpp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) == 0 ||
		!strings.Contains(strings.ToLower(diagnostics[0].Message), "undeclared") {
		t.Fatalf("clangd diagnostics = %+v", diagnostics)
	}
}

func TestCheckerReturnsSemanticLocationsWithProviderProvenance(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "main.go"),
		[]byte("package main\n\nfunc Serve() {}\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	checker := Checker{
		Binary: os.Args[0],
		Args: []string{
			"-test.run=^TestLSPFixtureProcess$",
			"-lsp-fixture-log=" + filepath.Join(root, "semantic.log"),
		},
		Root: root, Sandbox: lspTestBackend{},
	}
	definition, err := checker.Definition(t.Context(), symbols.SemanticQuery{
		Path: "main.go", Line: 3, Character: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if definition.Source != "lsp:fixture-lsp" ||
		definition.Version != "1.2.3" || definition.Confidence != "high" ||
		len(definition.Locations) != 1 ||
		definition.Locations[0].Path != "main.go" ||
		definition.Locations[0].Line != 3 {
		t.Fatalf("definition = %+v", definition)
	}
	references, err := checker.References(t.Context(), symbols.SemanticQuery{
		Path: "main.go", Line: 3, Character: 6,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(references.Locations) != 2 ||
		references.Locations[1].Line != 5 {
		t.Fatalf("references = %+v", references)
	}
}

func TestCheckerReturnsIDEEditsWithoutApplyingThem(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "main.go")
	original := []byte("package main\n\nfunc Serve() {}\n")
	if err := os.WriteFile(source, original, 0o600); err != nil {
		t.Fatal(err)
	}
	checker := Checker{
		Binary: os.Args[0],
		Args: []string{
			"-test.run=^TestLSPFixtureProcess$",
			"-lsp-fixture-log=" + filepath.Join(root, "ide.log"),
		},
		Root: root, Sandbox: lspTestBackend{},
	}
	hover, err := checker.Hover(t.Context(), IDEQuery{
		Path: "main.go", Line: 3, Character: 6,
	})
	if err != nil || !strings.Contains(string(hover.Result), "func Serve") {
		t.Fatalf("hover = %+v, %v", hover, err)
	}
	formatting, err := checker.Formatting(t.Context(), IDEQuery{Path: "main.go"})
	if err != nil || !strings.Contains(string(formatting.Result), "newText") {
		t.Fatalf("formatting = %+v, %v", formatting, err)
	}
	rename, err := checker.Rename(t.Context(), IDEQuery{
		Path: "main.go", Line: 3, Character: 6, NewName: "Run",
	})
	if err != nil || !strings.Contains(string(rename.Result), `"changes"`) {
		t.Fatalf("rename = %+v, %v", rename, err)
	}
	after, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("LSP edit preview modified the file: %q", after)
	}
}

type lspTestBackend struct{}

func (lspTestBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "test", Backend: "passthrough", Available: true,
		Effective: securitymodel.Controls{
			FilesystemRead: securitymodel.FilesystemReadDeclaredRoots,

			FilesystemWrite: securitymodel.FilesystemWriteExactPaths,

			Network: securitymodel.NetworkDenied, ProcessTree: securitymodel.ProcessTreeGroupKill, CrossProcess: securitymodel.CrossProcessUnrestricted,
			Syscall: securitymodel.SyscallDenyDangerous, IPC: securitymodel.IPCUnrestricted, PathIdentity: securitymodel.PathIdentityDescriptorRelative,
			ArtifactOrigin: securitymodel.ArtifactOriginUnverifiedPath, DurableRecovery: securitymodel.DurableRecoveryMemoryOnly},
	}
}

func (lspTestBackend) Prepare(_ context.Context, command sandbox.Command) (sandbox.Command, error) {
	command.PreparedReadOnly = command.WorkspaceReadOnly
	command.PreparedReadPaths = append(
		[]string(nil), command.AdditionalReadPaths...,
	)
	command.PreparedNetworkDenied = command.DenyNetwork
	return command, nil
}

func TestLSPFixtureProcess(t *testing.T) {
	if *lspFixtureLog == "" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	logPath := *lspFixtureLog
	documentURI := ""
	for {
		message, err := readMessage(reader)
		if err != nil {
			os.Exit(2)
		}
		event := strings.TrimPrefix(message.Method, "textDocument/")
		appendFixtureLog(logPath, event)
		switch message.Method {
		case "initialize":
			writeFixtureMessage(writer, map[string]any{
				"jsonrpc": "2.0", "id": rawID(message.ID),
				"result": map[string]any{"capabilities": map[string]any{
					"textDocumentSync": map[string]any{"openClose": true, "change": 1},
				}, "serverInfo": map[string]any{"name": "fixture-lsp", "version": "1.2.3"}},
			})
		case "textDocument/didOpen":
			var params struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(message.Params, &params)
			documentURI = params.TextDocument.URI
			writeFixtureDiagnostics(writer, params.TextDocument.URI, 1, "before edit")
		case "textDocument/didChange":
			var params struct {
				TextDocument struct {
					URI     string `json:"uri"`
					Version int    `json:"version"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(message.Params, &params)
			writeFixtureDiagnostics(writer, params.TextDocument.URI, params.TextDocument.Version, "after edit")
		case "textDocument/definition":
			writeFixtureMessage(writer, map[string]any{
				"jsonrpc": "2.0", "id": rawID(message.ID),
				"result": []map[string]any{fixtureLocation(documentURI, 2)},
			})
		case "textDocument/references":
			writeFixtureMessage(writer, map[string]any{
				"jsonrpc": "2.0", "id": rawID(message.ID),
				"result": []map[string]any{
					fixtureLocation(documentURI, 2),
					fixtureLocation(documentURI, 4),
				},
			})
		case "textDocument/hover":
			writeFixtureMessage(writer, map[string]any{
				"jsonrpc": "2.0", "id": rawID(message.ID),
				"result": map[string]any{
					"contents": map[string]any{"kind": "markdown", "value": "func Serve()"},
				},
			})
		case "textDocument/formatting":
			writeFixtureMessage(writer, map[string]any{
				"jsonrpc": "2.0", "id": rawID(message.ID),
				"result": []map[string]any{{
					"range":   fixtureLocation(documentURI, 0)["range"],
					"newText": "package main\n",
				}},
			})
		case "textDocument/codeAction":
			writeFixtureMessage(writer, map[string]any{
				"jsonrpc": "2.0", "id": rawID(message.ID), "result": []any{},
			})
		case "textDocument/rename":
			writeFixtureMessage(writer, map[string]any{
				"jsonrpc": "2.0", "id": rawID(message.ID),
				"result": map[string]any{"changes": map[string]any{
					documentURI: []map[string]any{{
						"range":   fixtureLocation(documentURI, 2)["range"],
						"newText": "Run",
					}},
				}},
			})
		case "shutdown":
			writeFixtureMessage(writer, map[string]any{
				"jsonrpc": "2.0", "id": rawID(message.ID), "result": nil,
			})
		case "exit":
			os.Exit(0)
		}
	}
}

func fixtureLocation(uri string, line int) map[string]any {
	return map[string]any{
		"uri": uri,
		"range": map[string]any{
			"start": map[string]any{"line": line, "character": 5},
			"end":   map[string]any{"line": line, "character": 10},
		},
	}
}

func writeFixtureDiagnostics(writer *bufio.Writer, uri string, version int, message string) {
	writeFixtureMessage(writer, map[string]any{
		"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
		"params": map[string]any{
			"uri": uri, "version": version,
			"diagnostics": []map[string]any{{
				"range": map[string]any{
					"start": map[string]any{"line": 1, "character": 4},
					"end":   map[string]any{"line": 1, "character": 10},
				},
				"severity": 1, "code": "fixture", "source": "fixture-lsp", "message": message,
			}},
		},
	})
}

func writeFixtureMessage(writer *bufio.Writer, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		os.Exit(3)
	}
	_, _ = fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n%s", len(body), body)
	_ = writer.Flush()
}

func appendFixtureLog(path, event string) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(4)
	}
	_, _ = file.WriteString(event + "\n")
	_ = file.Close()
}

func rawID(value json.RawMessage) any {
	var result any
	_ = json.Unmarshal(value, &result)
	return result
}
