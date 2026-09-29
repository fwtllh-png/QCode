package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Diagnostic struct {
	File     string `json:"file"`
	Range    Range  `json:"range"`
	Severity int    `json:"severity,omitempty"`
	Code     any    `json:"code,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}

type Change struct {
	Path    string `json:"path"`
	Text    string `json:"text"`
	Version int    `json:"version"`
}

type Checker struct {
	Binary            string
	Args              []string
	Root              string
	DiagnosticTimeout time.Duration
	Sandbox           sandbox.Backend
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type rpcClient struct {
	input    io.WriteCloser
	command  *exec.Cmd
	root     string
	messages chan rpcMessage
	readErr  chan error
	done     chan error

	writeMu   sync.Mutex
	nextID    atomic.Int64
	closeOnce sync.Once
}

func (c Checker) Analyze(ctx context.Context, files []string, changes []Change) ([]Diagnostic, error) {
	paths := append([]string(nil), files...)
	for _, change := range changes {
		paths = append(paths, change.Path)
	}
	resolved, err := c.forPaths(paths)
	if err != nil {
		return nil, err
	}
	c = resolved
	client, err := c.start(ctx)
	if err != nil {
		return nil, err
	}
	defer client.close()

	rootURI := pathURI(client.root)
	var initialize json.RawMessage
	if err := client.call(ctx, "initialize", map[string]any{
		"processId": os.Getpid(),
		"rootUri":   rootURI,
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"publishDiagnostics": map[string]any{"relatedInformation": true},
				"synchronization":    map[string]any{"didSave": false},
			},
		},
	}, &initialize, nil); err != nil {
		return nil, fmt.Errorf("initialize language server: %w", err)
	}
	if err := client.notify("initialized", map[string]any{}); err != nil {
		return nil, err
	}

	expected := make(map[string]int)
	versions := make(map[string]int)
	workspace, err := sandbox.NewWorkspace(client.root)
	if err != nil {
		return nil, err
	}
	for _, name := range files {
		path, err := workspacePath(client.root, name)
		if err != nil {
			return nil, err
		}
		file, err := workspace.OpenFile(name)
		if err != nil {
			return nil, fmt.Errorf("read LSP document %q: %w", name, err)
		}
		content, err := io.ReadAll(io.LimitReader(file, (16<<20)+1))
		closeErr := file.Close()
		if err != nil {
			return nil, fmt.Errorf("read LSP document %q: %w", name, err)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(content) > 16<<20 {
			return nil, fmt.Errorf("LSP document %q exceeds 16 MiB", name)
		}
		uri := pathURI(path)
		expected[uri] = 1
		versions[uri] = 1
		if err := client.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri": uri, "languageId": languageID(path), "version": 1, "text": string(content),
			},
		}); err != nil {
			return nil, err
		}
	}
	for _, change := range changes {
		path, err := workspacePath(client.root, change.Path)
		if err != nil {
			return nil, err
		}
		uri := pathURI(path)
		version := change.Version
		if version <= versions[uri] {
			version = versions[uri] + 1
		}
		if version <= 1 {
			version = 2
		}
		versions[uri] = version
		expected[uri] = version
		if err := client.notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": version},
			"contentChanges": []map[string]any{{"text": change.Text}},
		}); err != nil {
			return nil, err
		}
	}

	timeout := c.DiagnosticTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	diagnostics, err := client.collectDiagnostics(ctx, expected, timeout)
	if err != nil {
		return nil, err
	}
	var shutdown json.RawMessage
	if err := client.call(ctx, "shutdown", nil, &shutdown, diagnostics); err != nil {
		return nil, fmt.Errorf("shutdown language server: %w", err)
	}
	if err := client.notify("exit", nil); err != nil {
		return nil, err
	}
	client.finish(500 * time.Millisecond)
	return diagnostics, nil
}

// resolveWorkspaceRoot answers the canonical root a server session works
// from: absolute, and carrying whatever the bound sandbox policy says the
// workspace root is (a policy may resolve symlinks). Documents and results
// must agree on it, or the URIs in flight stop matching the workspace.
func resolveWorkspaceRoot(backend sandbox.Backend, workspace string) (sandbox.Backend, string, error) {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return nil, "", err
	}
	backend, err = sandbox.BindPolicy(backend, sandbox.Options{WorkspaceRoot: root})
	if err != nil {
		return nil, "", err
	}
	policy, _ := sandbox.BackendPolicy(backend)
	return backend, policy.WorkspaceRoot, nil
}

func (c Checker) start(ctx context.Context) (*rpcClient, error) {
	binary := c.Binary
	if binary == "" {
		binary = "gopls"
	}
	backend, root, err := resolveWorkspaceRoot(c.Sandbox, c.Root)
	if err != nil {
		return nil, err
	}
	directory, err := process.OpenPinnedDirectory(backend, root)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	command, err := process.NewCommand(ctx, process.Options{
		Path: binary, Args: c.Args, Dir: root, DirFile: directory,
		Sandbox: backend, RequireSandbox: true,
		WorkspaceReadOnly:   true,
		AdditionalReadPaths: []string{filepath.Dir(binary)},
		DenyNetwork:         true,
	})
	if err != nil {
		return nil, err
	}
	input, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start language server: %w", err)
	}
	client := &rpcClient{
		input: input, command: command, root: root,
		messages: make(chan rpcMessage, 32), readErr: make(chan error, 1), done: make(chan error, 1),
	}
	go client.readLoop(bufio.NewReader(output))
	go func() {
		client.done <- command.Wait()
	}()
	return client, nil
}

func (c *rpcClient) call(
	ctx context.Context,
	method string,
	params any,
	result any,
	diagnostics []Diagnostic,
) error {
	id := c.nextID.Add(1)
	if err := c.write(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	}); err != nil {
		return err
	}
	for {
		message, err := c.read(ctx)
		if err != nil {
			return err
		}
		if message.Method != "" {
			if err := c.handleServerMessage(message, &diagnostics); err != nil {
				return err
			}
			continue
		}
		var responseID int64
		if err := json.Unmarshal(message.ID, &responseID); err != nil || responseID != id {
			continue
		}
		if message.Error != nil {
			return fmt.Errorf("JSON-RPC %d: %s", message.Error.Code, message.Error.Message)
		}
		if result != nil && len(message.Result) != 0 {
			if err := json.Unmarshal(message.Result, result); err != nil {
				return err
			}
		}
		return nil
	}
}

func (c *rpcClient) notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *rpcClient) collectDiagnostics(
	ctx context.Context,
	expected map[string]int,
	timeout time.Duration,
) ([]Diagnostic, error) {
	if len(expected) == 0 {
		return nil, nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	seen := make(map[string]struct{})
	byURI := make(map[string][]Diagnostic)
	for len(seen) < len(expected) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return flattenDiagnostics(byURI), nil
		case err := <-c.readErr:
			return nil, err
		case message := <-c.messages:
			uri, version, values, published, err := c.diagnosticMessage(message)
			if err != nil {
				return nil, err
			}
			if published {
				byURI[uri] = values
				target, exists := expected[uri]
				if exists && (version == 0 || version >= target) {
					seen[uri] = struct{}{}
				}
			} else if err := c.handleServerMessage(message, nil); err != nil {
				return nil, err
			}
		}
	}
	return flattenDiagnostics(byURI), nil
}

func (c *rpcClient) diagnosticMessage(message rpcMessage) (string, int, []Diagnostic, bool, error) {
	if message.Method != "textDocument/publishDiagnostics" {
		return "", 0, nil, false, nil
	}
	var params struct {
		URI         string `json:"uri"`
		Version     int    `json:"version"`
		Diagnostics []struct {
			Range    Range           `json:"range"`
			Severity int             `json:"severity"`
			Code     json.RawMessage `json:"code"`
			Source   string          `json:"source"`
			Message  string          `json:"message"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(message.Params, &params); err != nil {
		return "", 0, nil, false, err
	}
	path := uriPath(params.URI)
	relative, err := filepath.Rel(c.root, path)
	if err != nil {
		relative = path
	}
	values := make([]Diagnostic, 0, len(params.Diagnostics))
	for _, item := range params.Diagnostics {
		var code any
		if len(item.Code) != 0 && string(item.Code) != "null" {
			if err := json.Unmarshal(item.Code, &code); err != nil {
				code = string(item.Code)
			}
		}
		values = append(values, Diagnostic{
			File: filepath.ToSlash(relative), Range: item.Range, Severity: item.Severity,
			Code: code, Source: item.Source, Message: item.Message,
		})
	}
	return params.URI, params.Version, values, true, nil
}

func (c *rpcClient) handleServerMessage(message rpcMessage, diagnostics *[]Diagnostic) error {
	if message.Method == "textDocument/publishDiagnostics" && diagnostics != nil {
		_, _, values, _, err := c.diagnosticMessage(message)
		*diagnostics = append(*diagnostics, values...)
		return err
	}
	if len(message.ID) != 0 && message.Method != "" {
		var id any
		if err := json.Unmarshal(message.ID, &id); err != nil {
			return err
		}
		return c.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": nil})
	}
	return nil
}

func (c *rpcClient) write(value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = fmt.Fprintf(c.input, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return err
}

func (c *rpcClient) read(ctx context.Context) (rpcMessage, error) {
	select {
	case <-ctx.Done():
		return rpcMessage{}, ctx.Err()
	case err := <-c.readErr:
		return rpcMessage{}, err
	case message := <-c.messages:
		return message, nil
	}
}

func (c *rpcClient) readLoop(reader *bufio.Reader) {
	for {
		message, err := readMessage(reader)
		if err != nil {
			c.readErr <- err
			return
		}
		c.messages <- message
	}
}

func readMessage(reader *bufio.Reader) (rpcMessage, error) {
	length := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return rpcMessage{}, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return rpcMessage{}, err
			}
		}
	}
	if length < 0 || length > 16<<20 {
		return rpcMessage{}, errors.New("invalid JSON-RPC Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		return rpcMessage{}, err
	}
	var message rpcMessage
	if err := json.Unmarshal(body, &message); err != nil {
		return rpcMessage{}, err
	}
	return message, nil
}

func (c *rpcClient) close() {
	c.closeOnce.Do(func() {
		_ = c.input.Close()
		if c.command.Process != nil {
			_ = c.command.Process.Kill()
		}
		<-c.done
	})
}

func (c *rpcClient) finish(timeout time.Duration) {
	c.closeOnce.Do(func() {
		_ = c.input.Close()
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-c.done:
		case <-timer.C:
			if c.command.Process != nil {
				_ = c.command.Process.Kill()
			}
			<-c.done
		}
	})
}

func flattenDiagnostics(byURI map[string][]Diagnostic) []Diagnostic {
	var result []Diagnostic
	for _, values := range byURI {
		result = append(result, values...)
	}
	return result
}

func workspacePath(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) {
		return "", errors.New("LSP document path must be a non-empty relative path")
	}
	path := filepath.Join(root, filepath.Clean(name))
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("LSP document path is outside workspace")
	}
	return path, nil
}

func pathURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func uriPath(uri string) string {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "file" {
		return uri
	}
	return filepath.FromSlash(parsed.Path)
}

func languageID(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".js":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".c", ".h":
		return "c"
	case ".cc", ".cpp", ".cxx", ".hh", ".hpp", ".hxx":
		return "cpp"
	case ".java":
		return "java"
	default:
		return "plaintext"
	}
}
