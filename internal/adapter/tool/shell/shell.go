package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// ForegroundTimeoutHint steers agents from a bounded read toward the session protocol.
const ForegroundTimeoutHint = "timed out; rerun with exec_command and continue via write_stdin"

// DefaultForegroundTimeout bounds a foreground read-only command that does not
// request an explicit timeout_ms. Foreground reads are bounded inspections —
// status output, greps, single-file compiles — while longer work belongs to
// the exec_command session protocol, whose wait windows are separately capped
// by maxProcessYield. Without this bound a hung command occupies the turn
// until external cancellation. Expiry reports ForegroundTimeoutHint so the
// model reruns the work as a polled session instead of retrying the read.
// Boundary tests lock the value.
const DefaultForegroundTimeout = 60 * time.Second

type Tool struct {
	workspace *sandbox.Workspace
	backend   sandbox.Backend
	readOnly  bool
}

type foregroundInput struct {
	Command     string   `json:"command"`
	CWD         string   `json:"cwd"`
	TimeoutMS   int64    `json:"timeout_ms"`
	Description string   `json:"description"`
	WritePaths  []string `json:"write_paths"`
}

type foregroundExecutor struct {
	tool    *Tool
	runtime interface {
		tool.OutcomeExecutor
		tool.DispositionProvider
	}
}

func RegisterWithManagerAndBackend(
	registry *tool.Registry,
	root string,
	manager *process.SessionManager,
	backend sandbox.Backend,
) error {
	if backend == nil {
		return errors.New("shell tools require an injected sandbox backend")
	}
	if manager == nil {
		return errors.New("shell tools require an injected session manager")
	}
	backend, err := sandbox.BindPolicy(backend, sandbox.Options{WorkspaceRoot: root})
	if err != nil {
		return err
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		return err
	}
	return registerWithBackend(registry, workspace, manager, backend)
}

func registerWithBackend(
	registry *tool.Registry,
	workspace *sandbox.Workspace,
	manager *process.SessionManager,
	backend sandbox.Backend,
) error {
	registry.SetSandboxBackend(backend)
	implementation := &Tool{
		workspace: workspace,
		backend:   backend,
		readOnly:  true,
	}
	executor, err := newForegroundExecutor(implementation)
	if err != nil {
		return err
	}
	if err := registry.Register(executor); err != nil {
		return err
	}
	return registerProcessProtocol(registry, workspace, backend, manager)
}

func (t *Tool) Descriptor() tool.Descriptor {
	description := "Run a read-only, network-isolated POSIX sh command. " +
		"The sandbox permits workspace reads and private temporary files, " +
		"but rejects workspace writes and network access. Quote shell " +
		"metacharacters with single quotes. Use $TMPDIR for compiler outputs " +
		"and caches; absolute /tmp remains denied. Use cwd instead of prepending cd. " +
		"Do not pipe verification commands through head or tail because POSIX " +
		"pipelines report the last command's status."
	description += " Commands run under POSIX sh, not Bash. Do not use Bash-only " +
		"syntax such as process substitution (<(...))."
	properties := map[string]any{
		"command": map[string]any{"type": "string", "minLength": float64(1)},
		"cwd":     map[string]any{"type": "string"},
		"timeout_ms": map[string]any{
			"type":        "integer",
			"description": "Hard deadline that kills the process group; defaults to 60000. Longer work belongs to exec_command sessions polled via write_stdin.",
		},
		"description": map[string]any{"type": "string"},
	}
	return tool.Descriptor{
		Name: "shell_read", Description: description, Visibility: tool.VisibleModel,
		DiscoveryTerms: []string{"read command", "inspect command", "只读命令", "查看命令"},
		IdentityKeys:   []string{"command", "cwd"},
		Capability:     tool.CapabilityRead, AccessMode: tool.AccessRead,
		ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{
			{Kind: "repo", ID: ".", Access: tool.AccessRead, Tree: true},
			{Kind: "process", ID: "workspace", Access: tool.AccessRead, Tree: true},
		}},
		ParallelPolicy:     tool.ParallelConcurrent,
		SandboxRequirement: tool.SandboxStrong, Availability: tool.AvailabilityAvailable,
		RepeatPolicy: tool.RepeatExecute,
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           properties,
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	}
}

func (t *Tool) ExpandArguments(
	_ context.Context,
	raw json.RawMessage,
) (json.RawMessage, error) {
	if t.readOnly {
		return raw, nil
	}
	return t.expandWriteGlobs(raw)
}

func (t *Tool) Execute(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	input, err := typed.DecodeStrict[foregroundInput](raw)
	if err != nil {
		return tool.Result{}, err
	}
	return t.execute(ctx, input)
}

func (t *Tool) execute(ctx context.Context, input foregroundInput) (tool.Result, error) {
	if token := unsupportedPOSIXShellSyntax(input.Command); token != "" {
		content, _ := json.Marshal(map[string]any{
			"status":          "rejected",
			"error_category":  "unsupported_shell_syntax",
			"shell_dialect":   "posix_sh",
			"syntax":          token,
			"required_action": "rewrite_without_process_substitution",
		})
		return tool.Result{
			Content: string(content),
			IsError: true,
			Metadata: map[string]any{
				"exit_code":       -1,
				"error_category":  "unsupported_shell_syntax",
				"shell_dialect":   "posix_sh",
				"syntax":          token,
				"required_action": "rewrite_without_process_substitution",
			},
		}, nil
	}
	directory, err := t.workspace.ResolveDirectory(input.CWD)
	if err != nil {
		return tool.Result{
			Content: fmt.Sprintf("invalid cwd %q: %v", input.CWD, err),
			IsError: true,
			Metadata: map[string]any{
				"exit_code": -1, "cwd_error": err.Error(),
			},
		}, nil
	}
	directoryFile, err := t.workspace.OpenDirectory(input.CWD)
	if err != nil {
		return tool.Result{
			Content: fmt.Sprintf("open cwd %q: %v", input.CWD, err),
			IsError: true,
			Metadata: map[string]any{
				"exit_code": -1, "cwd_error": err.Error(),
			},
		}, nil
	}
	defer directoryFile.Close()
	writePaths, err := t.resolveWritePaths(input.WritePaths)
	if err != nil {
		return tool.Result{}, fmt.Errorf("resolve shell write paths: %w", err)
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, foregroundTimeout(input))
	defer cancel()
	sandboxBackend, requireStrong := processSandbox(ctx, t.backend)
	started := time.Now()
	command := input.Command
	if requireStrong {
		command = wrapSandboxTempCommand(command)
	}
	result, err := process.Run(ctx, process.Options{
		Command: command, Dir: directory, PTY: false,
		DirFile: directoryFile,
		Sandbox: sandboxBackend, RequireSandbox: requireStrong,
		WorkspaceReadOnly:   true,
		WorkspaceWritePaths: writePaths,
		DenyNetwork:         true,
		OnOutput:            streamOutput(ctx),
		OnTeardown: func(duration time.Duration) {
			tool.ReportTeardown(ctx, tool.TeardownReport{Duration: duration})
		},
		OutputLimitBytes: process.ModelOutputLimitBytes,
	})
	durationMS := time.Since(started).Milliseconds()
	timedOut := errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(ctx.Err(), context.DeadlineExceeded)
	if err != nil && !timedOut {
		if recovery := t.recoverWorkspaceChange(err); recovery != nil {
			return tool.Result{}, recovery
		}
		return tool.Result{}, err
	}
	content := formatShellOutput(result.Stdout, result.Stderr)
	status := "completed"
	if timedOut {
		status = "timed_out"
	} else if result.ExitCode != 0 {
		status = "failed"
	}
	exitCode := result.ExitCode
	if content == "" && result.ExitCode != 0 {
		content = fmt.Sprintf("command exited with code %d", result.ExitCode)
	}
	if hint := sandboxPathHint(result.Stdout + "\n" + result.Stderr); hint != "" {
		if content != "" {
			content += "\n"
		}
		content += "[hint] " + hint
	}
	tail := content
	if len(tail) > 2<<10 {
		tail = tail[len(tail)-(2<<10):]
	}
	identity := tool.InvocationIdentityFrom(ctx)
	callID := identity.CallID
	metadata := map[string]any{
		"stdout": result.Stdout, "stderr": result.Stderr, "exit_code": result.ExitCode, "pty": false,
		"output_receipt": result.OutputReceipt,
		"workspace_write_scope": func() string {
			if len(input.WritePaths) == 0 {
				return "none"
			}
			return "exact"
		}(),
		"command_execution": map[string]any{
			"command": input.Command, "status": status, "exit_code": exitCode,
			"duration_ms": durationMS, "output_tail": tail, "call_id": callID,
			"stdout_bytes": result.OutputReceipt.Stdout.TotalBytes,
			"stderr_bytes": result.OutputReceipt.Stderr.TotalBytes,
			"omitted_bytes": result.OutputReceipt.Stdout.OmittedBytes +
				result.OutputReceipt.Stderr.OmittedBytes,
		},
	}
	if input.Description != "" {
		metadata["description"] = input.Description
	}
	if len(input.WritePaths) != 0 {
		metadata["write_paths"] = append([]string(nil), input.WritePaths...)
	}
	if timedOut {
		if content != "" {
			content += "\n"
		}
		content += ForegroundTimeoutHint
		metadata["timed_out"] = true
		metadata["timeout_hint"] = ForegroundTimeoutHint
		return tool.Result{Content: content, IsError: true, Metadata: metadata}, nil
	}
	return tool.Result{
		Content:  content,
		IsError:  result.ExitCode != 0,
		Metadata: metadata,
	}, nil
}

// foregroundTimeout resolves the effective deadline for one foreground read:
// an explicit timeout_ms wins; otherwise the documented default bounds the
// inspection so a hung command cannot occupy the turn indefinitely.
func foregroundTimeout(input foregroundInput) time.Duration {
	if input.TimeoutMS > 0 {
		return time.Duration(input.TimeoutMS) * time.Millisecond
	}
	return DefaultForegroundTimeout
}

func newForegroundExecutor(implementation *Tool) (tool.Executor, error) {
	executor, err := typed.Define(typed.Spec[foregroundInput, tool.Result]{
		Descriptor:  implementation.Descriptor(),
		Disposition: tool.DispositionWaitForTeardown,
		Run:         implementation.execute,
		Encode: func(result tool.Result) (tool.Result, error) {
			return result, nil
		},
	})
	if err != nil {
		return nil, err
	}
	runtime, ok := executor.(interface {
		tool.OutcomeExecutor
		tool.DispositionProvider
	})
	if !ok {
		return nil, errors.New("typed shell executor has no outcome runtime")
	}
	return &foregroundExecutor{tool: implementation, runtime: runtime}, nil
}

func (e *foregroundExecutor) Descriptor() tool.Descriptor {
	return e.runtime.Descriptor()
}

func (e *foregroundExecutor) TrustedBinding() tool.TrustedBinding {
	binding := tool.TrustedBindingFromDescriptor(e.runtime.Descriptor())
	binding.Capability = tool.CapabilityProcess
	binding.Effect = tool.EffectContract{
		Mode: tool.EffectFixed, Kind: tool.EffectProcessReadOnly,
		Risk: tool.RiskLow, Reversibility: tool.Reversible,
		WorkspaceTransaction: tool.TransactionNone,
		Approval:             tool.ApprovalPolicyDefault,
	}
	binding.Required.ProcessTree = controlmatrix.ProcessTreeGroupKill
	return binding
}

func (e *foregroundExecutor) ExecutionDisposition() tool.ExecutionDisposition {
	return e.runtime.ExecutionDisposition()
}

func (e *foregroundExecutor) Execute(
	ctx context.Context,
	raw json.RawMessage,
) (tool.Result, error) {
	return e.runtime.Execute(ctx, raw)
}

func (e *foregroundExecutor) ExecuteOutcome(
	ctx context.Context,
	raw json.RawMessage,
) (tool.Result, tool.Outcome, error) {
	return e.runtime.ExecuteOutcome(ctx, raw)
}

func (e *foregroundExecutor) ExpandArguments(
	ctx context.Context,
	raw json.RawMessage,
) (json.RawMessage, error) {
	return e.tool.ExpandArguments(ctx, raw)
}

func (t *Tool) recoverWorkspaceChange(err error) error {
	if !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var pathError *os.PathError
	if !errors.As(err, &pathError) {
		return nil
	}
	missingPath := canonicalMissingPath(pathError.Path)
	relative, relErr := filepath.Rel(t.workspace.Root(), missingPath)
	if relErr != nil || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil
	}
	return tool.Precondition(tool.WithRecoveryHint(
		fmt.Errorf("workspace changed during sandbox validation: %w", err),
		tool.RecoveryHint{
			ErrorCategory:  "workspace_changed",
			RequiredAction: "shell_read",
			Path:           filepath.ToSlash(relative),
			RetryOriginal:  true,
		},
	))
}

func canonicalMissingPath(path string) string {
	current := filepath.Clean(path)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return resolved
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return filepath.Clean(path)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(path)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func (t *Tool) resolveWritePaths(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if len(paths) > sandbox.MaxExactWorkspaceWritePaths {
		return nil, fmt.Errorf(
			"shell write paths exceed the %d-file limit",
			sandbox.MaxExactWorkspaceWritePaths,
		)
	}
	resolved := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		canonical, err := t.workspace.Resolve(path, sandbox.AllowMissing)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(canonical)
		if errors.Is(err, os.ErrNotExist) {
			parent, parentErr := os.Stat(filepath.Dir(canonical))
			if parentErr != nil {
				return nil, fmt.Errorf(
					"write path %q requires an existing parent directory: %w",
					path,
					parentErr,
				)
			}
			if !parent.IsDir() {
				return nil, fmt.Errorf(
					"write path %q parent is not a directory",
					path,
				)
			}
		} else if err != nil {
			return nil, err
		} else if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("write path %q is not a regular file", path)
		}
		if _, exists := seen[canonical]; exists {
			return nil, fmt.Errorf("duplicate shell write path %q", path)
		}
		seen[canonical] = struct{}{}
		resolved = append(resolved, canonical)
	}
	sort.Strings(resolved)
	return resolved, nil
}

// streamOutput bridges a running command's output to whoever is watching this
// tool call. It returns nil when nobody is, so an unobserved command pays nothing
// for the copy.
func streamOutput(ctx context.Context) func(process.Chunk) {
	observe := tool.OutputObserverFrom(ctx)
	if observe == nil {
		return nil
	}
	return func(chunk process.Chunk) {
		observe(tool.OutputChunk{
			Stream: string(chunk.Stream), Data: string(chunk.Data), Cursor: chunk.Cursor,
		})
	}
}

func unsupportedPOSIXShellSyntax(command string) string {
	var singleQuoted, doubleQuoted, escaped, comment bool
	for index := 0; index < len(command); index++ {
		current := command[index]
		if comment {
			if current == '\n' {
				comment = false
			}
			continue
		}
		if escaped {
			escaped = false
			continue
		}
		if current == '\\' && !singleQuoted {
			escaped = true
			continue
		}
		if current == '\'' && !doubleQuoted {
			singleQuoted = !singleQuoted
			continue
		}
		if current == '"' && !singleQuoted {
			doubleQuoted = !doubleQuoted
			continue
		}
		if singleQuoted {
			continue
		}
		if current == '#' &&
			(index == 0 || strings.ContainsRune(" \t\r\n;|&()", rune(command[index-1]))) {
			comment = true
			continue
		}
		if (current == '<' || current == '>') &&
			index+1 < len(command) && command[index+1] == '(' {
			return command[index : index+2]
		}
	}
	return ""
}

func formatShellOutput(stdout, stderr string) string {
	content := stdout
	if stderr == "" {
		return content
	}
	if content != "" {
		content += "\n"
	}
	return content + "[stderr]\n" + stderr
}

// wrapSandboxTempCommand remaps `cd /tmp` onto $TMPDIR. Seatbelt makes host
// /tmp look like "Not a directory"; agents still emit that path constantly.
func wrapSandboxTempCommand(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return command
	}
	const preamble = `cd(){ if [ "$#" -eq 1 ] && { [ "$1" = /tmp ] || [ "$1" = /tmp/ ]; }; then` +
		` command cd "${TMPDIR:-.}"; else command cd "$@"; fi; }; `
	return preamble + command
}

func sandboxPathHint(text string) string {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "go: command not found") ||
		strings.Contains(lower, "go: not found") {
		return "Go toolchain not on sandbox PATH; restart qcode from a shell where `which go` works (GOROOT/bin is injected automatically in current builds)"
	}
	if strings.Contains(lower, "could not create module cache") ||
		strings.Contains(lower, "mkdir /var: file exists") {
		return "Go module cache path hit macOS /var symlink under sandbox; upgrade/restart qcode so private temp uses /private/var realpath"
	}
	if !strings.Contains(lower, "not a directory") &&
		!strings.Contains(lower, "operation not permitted") &&
		!strings.Contains(lower, "permission denied") {
		return ""
	}
	if strings.Contains(lower, "/tmp") || strings.Contains(lower, "/var/folders") ||
		strings.Contains(lower, "outside") {
		return "sandbox blocks host /tmp and paths outside the workspace; use $TMPDIR or stay inside the workspace"
	}
	if strings.Contains(lower, "not a directory") {
		return "path denied by sandbox (often looks like 'Not a directory'); use workspace-relative paths and $TMPDIR"
	}
	if strings.Contains(lower, "operation not permitted") ||
		strings.Contains(lower, "permission denied") {
		return "sandbox denied this filesystem operation; use file_write or file_apply for workspace edits because they safely create missing parent directories"
	}
	return ""
}
