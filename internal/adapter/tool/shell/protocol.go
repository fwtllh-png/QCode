package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/platform/tokenestimate"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

const (
	defaultExecYield       = 10 * time.Second
	defaultInteractionWait = 5 * time.Second
	maxProcessYield        = 30 * time.Second
	defaultOutputTokens    = 4096
	maxOutputTokens        = 10_000
	// maxProcessTimeout caps a declared process deadline. A session whose
	// deadline exceeds a working day is indistinguishable from a leaked
	// process: longer work must be structured as a polled session with
	// explicit write_stdin wait windows instead of one unbounded deadline.
	// Boundary tests lock the value.
	maxProcessTimeout = 24 * time.Hour
)

type execCommandInput struct {
	Command        string                       `json:"command"`
	CWD            string                       `json:"cwd"`
	TTY            bool                         `json:"tty"`
	YieldTimeMS    int64                        `json:"yield_time_ms"`
	TimeoutMS      int64                        `json:"timeout_ms"`
	OutputTokens   int                          `json:"output_tokens"`
	Rows           uint16                       `json:"rows"`
	Cols           uint16                       `json:"cols"`
	Description    string                       `json:"description"`
	WritePaths     []string                     `json:"write_paths"`
	NetworkTargets []tool.DeclaredNetworkTarget `json:"network_targets"`
	AllowLoopback  bool                         `json:"allow_loopback"`
	Verification   string                       `json:"verification"`
	CoveredPaths   []string                     `json:"covered_paths"`
}

type writeStdinInput struct {
	SessionID    string `json:"session_id"`
	Chars        string `json:"chars"`
	YieldTimeMS  int64  `json:"yield_time_ms"`
	OutputTokens int    `json:"output_tokens"`
	Rows         uint16 `json:"rows"`
	Cols         uint16 `json:"cols"`
	Signal       string `json:"signal"`
	Close        bool   `json:"close"`
}

type commandProtocol struct {
	workspace *sandbox.Workspace
	backend   sandbox.Backend
	manager   *process.SessionManager
}

type protocolExecutor struct {
	protocol                   *commandProtocol
	runtime                    outcomeRuntime
	expand                     bool
	validateMissingWriteParent bool
}

func (e *protocolExecutor) TrustedBinding() tool.TrustedBinding {
	binding := tool.TrustedBindingFromDescriptor(e.runtime.Descriptor())
	binding.Capability = tool.CapabilityProcess
	binding.ValidateMissingWriteParent = e.validateMissingWriteParent
	binding.Required.ProcessTree = controlmatrix.ProcessTreeGroupKill
	binding.ProducesVerificationEvidence = true
	return binding
}

type outcomeRuntime interface {
	tool.OutcomeExecutor
	tool.DispositionProvider
}

func registerProcessProtocol(
	registry *tool.Registry,
	workspace *sandbox.Workspace,
	backend sandbox.Backend,
	manager *process.SessionManager,
) error {
	if manager == nil {
		return errors.New("process session manager is required")
	}
	protocol := &commandProtocol{
		workspace: workspace,
		backend:   backend,
		manager:   manager,
	}
	execRuntime, err := typed.Define(typed.Spec[execCommandInput, tool.Result]{
		Descriptor:  execCommandDescriptor(),
		Disposition: tool.DispositionDetached,
		Validate: func(input execCommandInput) error {
			if _, err := processYield(input.YieldTimeMS, defaultExecYield); err != nil {
				return err
			}
			if err := validateVerification(input); err != nil {
				return err
			}
			return validateNetworkTargets(input.NetworkTargets)
		},
		Run:     protocol.execCommand,
		Encode:  identityResult,
		Outcome: processOutcome,
	})
	if err != nil {
		return err
	}
	execOutcome, ok := execRuntime.(outcomeRuntime)
	if !ok {
		return errors.New("exec_command typed runtime is incomplete")
	}
	if err := registry.Register(
		&protocolExecutor{
			protocol:                   protocol,
			runtime:                    execOutcome,
			expand:                     true,
			validateMissingWriteParent: true,
		}); err != nil {
		return err
	}
	writeRuntime, err := typed.Define(typed.Spec[writeStdinInput, tool.Result]{
		Descriptor:  writeStdinDescriptor(),
		Disposition: tool.DispositionWaitForTeardown,
		Validate: func(input writeStdinInput) error {
			if strings.TrimSpace(input.SessionID) == "" {
				return errors.New(
					"session_id is required: an empty id means the session " +
						"has already ended; its final output was returned by " +
						"the last poll and remains available through " +
						"turn_history or result_get. Do not retry with an " +
						"empty session_id",
				)
			}
			_, yieldErr := processYield(
				input.YieldTimeMS,
				defaultInteractionWait,
			)
			return yieldErr
		},
		Run:     protocol.writeStdin,
		Encode:  identityResult,
		Outcome: processOutcome,
	})
	if err != nil {
		return err
	}
	writeOutcome, ok := writeRuntime.(outcomeRuntime)
	if !ok {
		return errors.New("write_stdin typed runtime is incomplete")
	}
	return registry.Register(
		&protocolExecutor{protocol: protocol, runtime: writeOutcome})

}

func identityResult(result tool.Result) (tool.Result, error) {
	return result, nil
}

func processOutcome(result tool.Result) tool.Outcome {
	outcome := tool.OutcomeFromResult(result)
	if result.Metadata == nil {
		return outcome
	}
	if outcome.Facts == nil {
		outcome.Facts = &tool.OutcomeFacts{}
	}
	outcome.Facts.ProcessSession = &tool.ProcessSessionFact{
		SessionID:       stringMetadata(result.Metadata, "session_id"),
		SourceSessionID: stringMetadata(result.Metadata, "source_session_id"),
		Terminated:      boolMetadata(result.Metadata, "terminated"),
		Cursor:          uint64Metadata(result.Metadata, "cursor"),
		Running:         boolMetadata(result.Metadata, "running"),
		ExitCode:        intMetadata(result.Metadata, "exit_code"),
		TimedOut:        boolMetadata(result.Metadata, "timed_out"),
		TTY:             boolMetadata(result.Metadata, "tty"),
		Archived:        boolMetadata(result.Metadata, "archived"),
		PendingBytes:    intMetadata(result.Metadata, "pending_bytes"),
		OmittedBytes:    intMetadata(result.Metadata, "omitted_bytes"),
	}
	return outcome
}

func stringMetadata(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return value
}

func boolMetadata(metadata map[string]any, key string) bool {
	value, _ := metadata[key].(bool)
	return value
}

func intMetadata(metadata map[string]any, key string) int {
	value, _ := metadata[key].(int)
	return value
}

func uint64Metadata(metadata map[string]any, key string) uint64 {
	value, _ := metadata[key].(uint64)
	return value
}

func execCommandDescriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "exec_command",
		Description: "Run a local POSIX sh command. Returns output when it exits " +
			"within yield-time, otherwise a session_id for write_stdin. This " +
			"applies to TTY and non-TTY commands; the first sample never waits " +
			"for a process that outlives yield-time. yield-time_ms defaults to " +
			"10000 and must not exceed 30000. timeout_ms, when set, kills the " +
			"process group; it does not keep the first sample blocked. " +
			"If the command starts a server or daemon it never exits: verify " +
			"its startup output and close the session instead of polling for exit. " +
			"The workspace is read-only by default. write_paths permits only exact " +
			"regular files whose parent directories already exist; it does not permit " +
			"mkdir. To create files in missing directories, use file_write or " +
			"file_apply directly because they safely create parent directories. " +
			"Use $TMPDIR for compiler outputs and caches; absolute /tmp remains denied. " +
			"Use cwd instead of prepending cd. Do not pipe verification commands " +
			"through head or tail because POSIX pipelines report the last command's " +
			"status; use output_tokens to bound output. To record validation evidence, " +
			"declare verification (test, build, lint, or check) and exact workspace-relative " +
			"covered_paths. Declared verification uses POSIX set -e; use && to chain checks. " +
			"Only a natural exit on unchanged inputs can pass; running " +
			"or terminated processes never count as passed verification. Git metadata " +
			"is protected: use the dedicated git_add, git_commit, git_switch, " +
			"git_fetch, git_pull, and git_push tools for Git mutations. " +
			"Commands that access the network must declare every destination in " +
			"network_targets; use method CONNECT for HTTPS targets. Undeclared " +
			"egress is denied by the local managed proxy. Set allow_loopback only " +
			"when the command binds or connects to a local development server; do " +
			"not put localhost or port 0 in network_targets for an ephemeral local " +
			"listener.",
		DiscoveryTerms: []string{
			"run command", "terminal", "build", "执行命令", "终端", "编译", "运行测试",
		},
		Visibility: tool.VisibleModel,
		Capability: tool.CapabilityProcess,
		AccessMode: tool.AccessRead,
		ResourceResolver: tool.ResourceResolver{
			Templates: []tool.ResourceTemplate{
				{Kind: "repo", ID: ".", Access: tool.AccessRead, Tree: true},
				{Kind: "process", ID: "workspace", Access: tool.AccessRead, Tree: true},
			},
			PathsField:          "write_paths",
			NetworkTargetsField: "network_targets",
			LoopbackField:       "allow_loopback",
			ReadPathsField:      "covered_paths",
		},
		ParallelPolicy:     tool.ParallelConcurrent,
		SandboxRequirement: tool.SandboxStrong,
		Availability:       processProtocolAvailability(),
		UnavailableReason:  processProtocolUnavailableReason(),
		RepeatPolicy:       tool.RepeatExecute,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "minLength": 1},
				"cwd":     map[string]any{"type": "string"},
				"tty":     map[string]any{"type": "boolean"},
				"yield_time_ms": map[string]any{
					"type":        "integer",
					"description": "Maximum time the first sample waits for exit. Still-running commands return session_id for write_stdin.",
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"description": "Hard deadline that kills the process group; must not exceed 86400000. Omit only when write_stdin will poll or close a still-running session.",
				},
				"output_tokens": map[string]any{"type": "integer"},
				"rows":          map[string]any{"type": "integer"},
				"cols":          map[string]any{"type": "integer"},
				"description":   map[string]any{"type": "string"},
				"verification": map[string]any{
					"type": "string", "enum": []any{"test", "build", "lint", "check"},
				},
				"covered_paths": map[string]any{
					"type": "array", "minItems": 1,
					"items": map[string]any{"type": "string", "minLength": 1},
				},
				"write_paths": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string"},
				},
				"write_globs": map[string]any{
					"type": "array",
				},
				"network_targets": tool.NetworkTargetsInputSchema(),
				"allow_loopback": map[string]any{
					"type":        "boolean",
					"description": "Permit localhost bind/connect for local development servers and fixtures",
				},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	}
}

func validateNetworkTargets(targets []tool.DeclaredNetworkTarget) error {
	return tool.ValidateDeclaredNetworkTargets(targets)
}

func writeStdinDescriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "write_stdin",
		Description: "Continue an exec_command session: poll output, write chars, " +
			"resize its TTY, signal it, or close it. The call returns as soon " +
			"as new output arrives or the process exits. yield_time_ms " +
			"defaults to 5000 and must not exceed 30000; while a session " +
			"stays silent and running, an undeclared wait keeps extending " +
			"in windows up to the 30000 cap before reporting still-running, " +
			"so long silent builds do not need one poll per window. Declare " +
			"yield_time_ms for an exact bounded wait.",
		DiscoveryTerms: []string{"process output", "terminal input", "进程输出", "终端输入"},
		Visibility:     tool.VisibleModel,
		Capability:     tool.CapabilityProcess,
		AccessMode:     tool.AccessWrite,
		ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
			Kind: "session", Field: "session_id", Access: tool.AccessWrite,
		}}},
		ParallelPolicy:     tool.ParallelConcurrent,
		SandboxRequirement: tool.SandboxStrong,
		Availability:       processProtocolAvailability(),
		UnavailableReason:  processProtocolUnavailableReason(),
		RepeatPolicy:       tool.RepeatExecute,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id": map[string]any{"type": "string"},
				"chars":      map[string]any{"type": "string"},
				"yield_time_ms": map[string]any{
					"type": "integer",
					"description": "Exact maximum wait in ms for new output. " +
						"Omit to keep waiting up to the 30000 cap while the " +
						"session stays silent.",
				},
				"output_tokens": map[string]any{"type": "integer"},
				"rows":          map[string]any{"type": "integer"},
				"cols":          map[string]any{"type": "integer"},
				"signal": map[string]any{
					"type": "string",
					"enum": []any{"INT", "TERM", "KILL"},
				},
				"close": map[string]any{"type": "boolean"},
			},
			"required":             []string{"session_id"},
			"additionalProperties": false,
		},
	}
}

func processProtocolAvailability() tool.Availability {
	if runtime.GOOS == "windows" {
		return tool.AvailabilityUnavailable
	}
	return tool.AvailabilityAvailable
}

func processProtocolUnavailableReason() string {
	if runtime.GOOS == "windows" {
		return "local process sessions are unavailable on this platform"
	}
	return ""
}

func (e *protocolExecutor) Descriptor() tool.Descriptor {
	return e.runtime.Descriptor()
}

func (e *protocolExecutor) ExecutionDisposition() tool.ExecutionDisposition {
	return e.runtime.ExecutionDisposition()
}

func (e *protocolExecutor) Execute(
	ctx context.Context,
	raw json.RawMessage,
) (tool.Result, error) {
	return e.runtime.Execute(ctx, raw)
}

func (e *protocolExecutor) ExecuteOutcome(
	ctx context.Context,
	raw json.RawMessage,
) (tool.Result, tool.Outcome, error) {
	return e.runtime.ExecuteOutcome(ctx, raw)
}

func (e *protocolExecutor) ExpandArguments(
	ctx context.Context,
	raw json.RawMessage,
) (json.RawMessage, error) {
	if !e.expand {
		return raw, nil
	}
	expanded, err := (&Tool{workspace: e.protocol.workspace}).ExpandArguments(ctx, raw)
	if err != nil {
		return nil, err
	}
	var input execCommandInput
	if err := json.Unmarshal(expanded, &input); err != nil {
		return nil, err
	}
	if err := validateNetworkTargets(input.NetworkTargets); err != nil {
		return nil, err
	}
	return expanded, nil
}

func (p *commandProtocol) execCommand(
	ctx context.Context,
	input execCommandInput,
) (tool.Result, error) {
	started := time.Now()
	if token := unsupportedPOSIXShellSyntax(input.Command); token != "" {
		return unsupportedSyntaxResult(token), nil
	}
	yield, err := processYield(input.YieldTimeMS, defaultExecYield)
	if err != nil {
		return tool.Result{}, err
	}
	timeout, err := processTimeout(input.TimeoutMS)
	if err != nil {
		return tool.Result{}, err
	}
	outputTokens, err := processOutputTokens(input.OutputTokens)
	if err != nil {
		return tool.Result{}, err
	}
	if (input.Rows == 0) != (input.Cols == 0) {
		return tool.Result{}, errors.New("rows and cols must be supplied together")
	}
	directory, err := p.workspace.ResolveDirectory(input.CWD)
	if err != nil {
		return tool.Result{}, fmt.Errorf("resolve cwd %q: %w", input.CWD, err)
	}
	directoryFile, err := p.workspace.OpenDirectory(input.CWD)
	if err != nil {
		return tool.Result{}, fmt.Errorf("open cwd %q: %w", input.CWD, err)
	}
	defer directoryFile.Close()
	writePaths, err := (&Tool{workspace: p.workspace}).resolveWritePaths(
		input.WritePaths,
	)
	if err != nil {
		return tool.Result{}, fmt.Errorf("resolve command write paths: %w", err)
	}
	sandboxBackend, requireStrong := processSandbox(ctx, p.backend)
	command := input.Command
	if input.Verification != "" {
		command = "set -e\n" + command
	}
	if requireStrong {
		command = wrapSandboxTempCommand(command)
	}
	identity := tool.InvocationIdentityFrom(ctx)
	threadID := strings.TrimSpace(identity.ThreadID)
	if threadID == "" {
		threadID = strings.TrimSpace(identity.SessionID)
	}
	if threadID == "" {
		threadID = strings.TrimSpace(identity.CallID)
	}
	if threadID == "" {
		return tool.Result{}, errors.New("exec_command requires a thread identity")
	}
	evidence, err := p.prepareVerification(input)
	if err != nil {
		return tool.Result{}, err
	}
	id, err := p.manager.Create(
		context.WithoutCancel(ctx),
		process.SessionOptions{
			Command:             command,
			Dir:                 directory,
			DirFile:             directoryFile,
			ThreadID:            threadID,
			TurnID:              identity.TurnID,
			CallID:              identity.CallID,
			Rows:                input.Rows,
			Cols:                input.Cols,
			PTY:                 input.TTY,
			Timeout:             timeout,
			Sandbox:             sandboxBackend,
			RequireSandbox:      requireStrong,
			WorkspaceReadOnly:   true,
			WorkspaceWritePaths: writePaths,
			DenyNetwork:         len(input.NetworkTargets) == 0 && !input.AllowLoopback,
			DetachFromCaller:    true,
		},
	)
	if err != nil {
		return tool.Result{}, err
	}
	wait, output, err := p.waitInitial(
		ctx,
		id,
		threadID,
		yield,
		int(tokenestimate.BytesForTokens(uint64(outputTokens))),
	)
	if err != nil {
		teardownStarted := time.Now()
		closeErr := p.manager.Close(id, threadID)
		tool.ReportTeardown(ctx, tool.TeardownReport{
			Duration: time.Since(teardownStarted),
		})
		return tool.Result{}, errors.Join(err, closeErr)
	}
	wait.Data = output.String()
	result := sessionResult(id, wait, outputTokens)
	attachVerification(&result, evidence, wait)
	status := "running"
	if !wait.Running {
		status = "completed"
		if wait.ExitCode != 0 || wait.Terminated {
			status = "failed"
		}
	} else {
		result.Metadata["error_category"] = "process_still_running"
		result.Metadata["required_action"] = "write_stdin"
		result.Metadata["retry_original"] = false
	}
	result.Metadata["command_execution"] = map[string]any{
		"command": input.Command, "status": status,
		"exit_code": wait.ExitCode, "duration_ms": time.Since(started).Milliseconds(),
		"output_tail": result.Content,
	}
	if omitted := output.Omitted(); omitted > 0 {
		result.Metadata["omitted_bytes"] = omitted
	}
	if input.Description != "" {
		result.Metadata["description"] = input.Description
	}
	if len(input.WritePaths) != 0 {
		result.Metadata["write_paths"] = append(
			[]string(nil),
			input.WritePaths...,
		)
	}
	if receipts := declaredEgressReceipts(input.NetworkTargets); len(receipts) != 0 {
		result.Metadata["egress_receipts"] = receipts
	}
	if !wait.Running {
		_ = p.manager.Close(id, threadID)
		delete(result.Metadata, "session_id")
	}
	return result, nil
}

func declaredEgressReceipts(targets []tool.DeclaredNetworkTarget) []egress.Receipt {
	receipts := make([]egress.Receipt, 0, len(targets))
	for _, target := range targets {
		methods := target.Methods
		if len(methods) == 0 {
			methods = []string{""}
		}
		for _, method := range methods {
			receipts = append(receipts, egress.Receipt{
				At: time.Now().UTC(), Source: "process",
				Host: target.Host, Protocol: target.Protocol, Port: target.Port,
				Method: strings.ToUpper(method), Decision: "authorized",
			})
		}
	}
	return receipts
}

func (p *commandProtocol) waitInitial(
	ctx context.Context,
	id string,
	threadID string,
	yield time.Duration,
	outputLimit int,
) (process.SessionWait, *processOutputAccumulator, error) {
	deadline := time.Now().Add(yield)
	combined := newProcessOutputAccumulator(outputLimit)
	var aggregate process.SessionWait
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			wait, err := p.manager.WaitNext(ctx, id, threadID, 0)
			if err != nil {
				return process.SessionWait{}, combined, err
			}
			combined.WriteString(wait.Data)
			aggregate = wait
			aggregate.TimedOut = wait.Running
			return aggregate, combined, nil
		}
		wait, err := p.manager.WaitNext(
			ctx,
			id,
			threadID,
			remaining,
		)
		if err != nil {
			return process.SessionWait{}, combined, err
		}
		combined.WriteString(wait.Data)
		aggregate = wait
		if !wait.Running || wait.TimedOut {
			return aggregate, combined, nil
		}
	}
}

// waitSessionOutput continues a session with quiet-window semantics: it
// returns when the process exits, when a full window passes without new
// output after some was delivered, or at the public yield cap. A silent
// running session keeps extending toward that cap — it has nothing to
// report, and returning early would only manufacture a re-poll round
// trip. While output keeps arriving inside a window the call keeps
// collecting, so chatty builds batch into one result instead of one model
// round trip per chunk. A caller-declared yield_time_ms bypasses this loop
// and is honored exactly.
func (p *commandProtocol) waitSessionOutput(
	ctx context.Context,
	id string,
	threadID string,
	window time.Duration,
	outputLimit int,
) (process.SessionWait, *processOutputAccumulator, error) {
	deadline := time.Now().Add(maxProcessYield)
	combined := newProcessOutputAccumulator(outputLimit)
	var aggregate process.SessionWait
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			wait, err := p.manager.WaitNext(ctx, id, threadID, 0)
			if err != nil {
				return process.SessionWait{}, combined, err
			}
			combined.WriteString(wait.Data)
			aggregate = wait
			aggregate.Data = combined.String()
			aggregate.TimedOut = wait.Running
			return aggregate, combined, nil
		}
		hadData := combined.total > 0
		wait, err := p.manager.WaitNext(
			ctx, id, threadID, min(window, remaining),
		)
		if err != nil {
			return process.SessionWait{}, combined, err
		}
		combined.WriteString(wait.Data)
		aggregate = wait
		aggregate.Data = combined.String()
		if !wait.Running {
			aggregate.TimedOut = false
			return aggregate, combined, nil
		}
		if wait.Data == "" {
			if hadData {
				aggregate.TimedOut = true
				return aggregate, combined, nil
			}
			continue
		}
	}
}

type processOutputAccumulator struct {
	limit int
	head  []byte
	tail  []byte
	total int
}

func newProcessOutputAccumulator(limit int) *processOutputAccumulator {
	return &processOutputAccumulator{limit: max(1, limit)}
}

func (b *processOutputAccumulator) WriteString(value string) {
	data := []byte(value)
	b.total += len(data)
	headLimit := b.limit * 3 / 4
	if remaining := headLimit - len(b.head); remaining > 0 {
		take := min(remaining, len(data))
		b.head = append(b.head, data[:take]...)
		data = data[take:]
	}
	tailLimit := b.limit - headLimit
	if len(data) >= tailLimit {
		b.tail = append(b.tail[:0], data[len(data)-tailLimit:]...)
		return
	}
	if overflow := len(b.tail) + len(data) - tailLimit; overflow > 0 {
		copy(b.tail, b.tail[overflow:])
		b.tail = b.tail[:len(b.tail)-overflow]
	}
	b.tail = append(b.tail, data...)
}

func (b *processOutputAccumulator) Omitted() int {
	return max(0, b.total-b.limit)
}

func (b *processOutputAccumulator) String() string {
	if b.Omitted() == 0 {
		return string(append(append([]byte(nil), b.head...), b.tail...))
	}
	return string(b.head) + fmt.Sprintf(
		"\n[output truncated: %d bytes omitted]\n",
		b.Omitted(),
	) + string(b.tail)
}

// sessionLookupHint marks session-not-found failures so the model stops
// re-polling a dead or mistyped session: the final output of an ended
// session is durable and reachable through turn_history or result_get.
func sessionLookupHint(err error) error {
	if err == nil || !errors.Is(err, process.ErrSessionNotFound) {
		return err
	}
	return tool.WithRecoveryHint(err, tool.RecoveryHint{
		ErrorCategory:  "process_session_not_found",
		RequiredAction: "use_turn_history",
		RetryOriginal:  false,
	})
}

func (p *commandProtocol) writeStdin(
	ctx context.Context,
	input writeStdinInput,
) (tool.Result, error) {
	yield, err := processYield(input.YieldTimeMS, defaultInteractionWait)
	if err != nil {
		return tool.Result{}, err
	}
	outputTokens, err := processOutputTokens(input.OutputTokens)
	if err != nil {
		return tool.Result{}, err
	}
	identity := tool.InvocationIdentityFrom(ctx)
	threadID := identity.ThreadID
	if input.Close {
		if err := sessionLookupHint(p.manager.Close(input.SessionID, threadID)); err != nil {
			return tool.Result{}, err
		}
		return tool.Result{
			Content: "closed",
			Metadata: map[string]any{
				"session_id":        input.SessionID,
				"source_session_id": input.SessionID,
				"terminated":        true,
				"running":           false,
				"closed":            true,
			},
		}, nil
	}
	if (input.Rows == 0) != (input.Cols == 0) {
		return tool.Result{}, errors.New("rows and cols must be supplied together")
	}
	if input.Rows != 0 {
		if err := sessionLookupHint(p.manager.Resize(
			input.SessionID,
			threadID,
			input.Rows,
			input.Cols,
		)); err != nil {
			return tool.Result{}, err
		}
	}
	if input.Signal != "" {
		signal, err := parseSignal(input.Signal)
		if err != nil {
			return tool.Result{}, err
		}
		if err := sessionLookupHint(p.manager.Signal(input.SessionID, threadID, signal)); err != nil {
			return tool.Result{}, err
		}
	}
	if input.Chars != "" {
		if err := sessionLookupHint(p.manager.Write(
			input.SessionID,
			threadID,
			[]byte(input.Chars),
		)); err != nil {
			return tool.Result{}, err
		}
	}
	var wait process.SessionWait
	var collected *processOutputAccumulator
	if input.YieldTimeMS > 0 {
		wait, err = p.manager.WaitNext(
			ctx,
			input.SessionID,
			threadID,
			yield,
		)
	} else {
		wait, collected, err = p.waitSessionOutput(
			ctx, input.SessionID, threadID, yield,
			int(tokenestimate.BytesForTokens(uint64(outputTokens))),
		)
	}
	if err != nil {
		return tool.Result{}, sessionLookupHint(err)
	}
	result := sessionResult(input.SessionID, wait, outputTokens)
	if collected != nil {
		if omitted := collected.Omitted(); omitted > 0 {
			result.Metadata["omitted_bytes"] = omitted
		}
	}
	if wait.TimedOut && wait.Running {
		result.Metadata["error_category"] = "process_still_running"
		result.Metadata["required_action"] = "write_stdin"
		result.Metadata["retry_original"] = false
	}
	if !wait.Running {
		teardownStarted := time.Now()
		closeErr := p.manager.Close(input.SessionID, threadID)
		tool.ReportTeardown(ctx, tool.TeardownReport{
			Duration: time.Since(teardownStarted),
		})
		if closeErr != nil {
			return result, closeErr
		}
		delete(result.Metadata, "session_id")
	}
	return result, nil
}

func processYield(value int64, fallback time.Duration) (time.Duration, error) {
	if value < 0 {
		return 0, errors.New("yield_time must not be negative")
	}
	if value == 0 {
		return fallback, nil
	}
	yield := time.Duration(value) * time.Millisecond
	if yield > maxProcessYield {
		return 0, fmt.Errorf("yield-time exceeds %s", maxProcessYield)
	}
	return yield, nil
}

func processTimeout(value int64) (time.Duration, error) {
	if value < 0 {
		return 0, errors.New("timeout must not be negative")
	}
	timeout := time.Duration(value) * time.Millisecond
	if timeout > maxProcessTimeout {
		return 0, fmt.Errorf("timeout exceeds %s", maxProcessTimeout)
	}
	return timeout, nil
}

func processOutputTokens(value int) (int, error) {
	if value == 0 {
		return defaultOutputTokens, nil
	}
	if value < 1 || value > maxOutputTokens {
		return 0, fmt.Errorf(
			"output_tokens must be between 1 and %d",
			maxOutputTokens,
		)
	}
	return value, nil
}

func sessionResult(
	id string,
	wait process.SessionWait,
	outputTokens int,
) tool.Result {
	content, omitted := limitProcessOutput(wait.Data, outputTokens*4)
	metadata := map[string]any{
		"session_id":        id,
		"source_session_id": id,
		"terminated":        wait.Terminated,
		"cursor":            wait.Cursor,
		"running":           wait.Running,
		"exit_code":         wait.ExitCode,
		"timed_out":         wait.TimedOut,
		"tty":               wait.TTY,
	}
	if omitted > 0 {
		metadata["omitted_bytes"] = omitted
	}
	if wait.Archived {
		metadata["archived"] = true
		metadata["pending_bytes"] = wait.Pending
	}
	return tool.Result{
		Content:  content,
		IsError:  !wait.Running && (wait.ExitCode != 0 || wait.Terminated),
		Metadata: metadata,
	}
}

func limitProcessOutput(value string, limit int) (string, int) {
	if limit <= 0 || len(value) <= limit {
		return value, 0
	}
	head := limit * 3 / 4
	tail := limit - head
	omitted := len(value) - limit
	return value[:head] + fmt.Sprintf(
		"\n[output truncated: %d bytes omitted]\n",
		omitted,
	) + value[len(value)-tail:], omitted
}

func unsupportedSyntaxResult(token string) tool.Result {
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
	}
}
