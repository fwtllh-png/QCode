package process

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/fwtllh-png/QCode/internal/observability/tracecontext"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type Options struct {
	Command              string
	Path                 string
	Args                 []string
	Dir                  string
	DirFile              *os.File
	PTY                  bool
	Env                  []string
	Stdin                io.Reader
	Sandbox              sandbox.Backend
	RequireSandbox       bool
	WorkspaceReadOnly    bool
	AdditionalReadPaths  []string
	WorkspaceWritePaths  []string
	WorkspaceHiddenPaths []string
	DenyNetwork          bool
	// TrustedRuntimeHelper is reserved for QCode-owned helper processes.
	// Arbitrary user commands never receive internal W3C trace context.
	TrustedRuntimeHelper bool
	NetworkHost          string
	NetworkProtocol      string
	NetworkPort          uint16
	SessionProxyPort     uint16
	// OnOutput, when set, is called with each chunk as the process produces it,
	// before the command finishes. A caller that only wants the final Result can
	// leave it nil; a caller that has to show progress on a command that runs for
	// a minute cannot wait for Result to exist.
	//
	// It is called from the reader goroutines, so it must be cheap and must not
	// block: a slow observer holds up the pipe and thereby the process itself.
	OnOutput func(Chunk)
	// OnTeardown reports how long cancellation spent terminating and reaping the
	// process group. It is called at most once.
	OnTeardown func(time.Duration)
	// OutputLimitBytes bounds retained bytes independently for stdout and
	// stderr. Zero selects DefaultOutputLimitBytes.
	OutputLimitBytes int
	// ArchiveOutput receives every complete chunk even when the returned
	// head/tail result is truncated.
	ArchiveOutput OutputArchive
}

// Stream names which of a process's two output streams a chunk came from. PTY
// execution merges them, and reports everything as StreamStdout, because that is
// what a terminal does.
type Stream string

const (
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
)

// Chunk is one piece of output as it was read, with the number of bytes of that
// stream delivered through the end of this chunk. The cursor lets an observer
// notice it missed something rather than silently rendering a gap.
type Chunk struct {
	Stream Stream
	Data   []byte
	Cursor uint64
}

type Result struct {
	Stdout        string
	Stderr        string
	ExitCode      int
	OutputReceipt OutputReceipt
}

func OpenPinnedDirectory(backend sandbox.Backend, directory string) (*os.File, error) {
	policy, ok := sandbox.BackendPolicy(backend)
	if !ok || policy.WorkspaceRoot == "" {
		return nil, errors.New("sandbox backend has no workspace policy")
	}
	workspace, err := sandbox.NewWorkspace(policy.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(policy.WorkspaceRoot, directory)
	if err != nil {
		return nil, err
	}
	return workspace.OpenDirectory(relative)
}

func Run(ctx context.Context, options Options) (Result, error) {
	if options.OutputLimitBytes < 0 {
		return Result{}, errors.New("process output limit must not be negative")
	}
	command, err := NewCommand(ctx, options)
	if err != nil {
		return Result{}, err
	}
	limit := options.OutputLimitBytes
	if limit == 0 {
		limit = DefaultOutputLimitBytes
	}
	var archive *archiveState
	if options.ArchiveOutput != nil {
		archive = &archiveState{append: options.ArchiveOutput}
	}
	if options.PTY {
		return runPTY(
			ctx,
			command,
			options.OnOutput,
			options.OnTeardown,
			archive,
			limit,
		)
	}
	stdout := newObservedBuffer(StreamStdout, limit, options.OnOutput, archive)
	stderr := newObservedBuffer(StreamStderr, limit, options.OnOutput, archive)
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return Result{}, err
	}
	err, teardown := waitAndCancel(ctx, command)
	if teardown > 0 && options.OnTeardown != nil {
		options.OnTeardown(teardown)
	}
	result := Result{
		Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: ExitCode(err),
		OutputReceipt: OutputReceipt{
			Stdout: stdout.Receipt(), Stderr: stderr.Receipt(),
			ArchiveError: archive.errorString(),
		},
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}

func NewCommand(ctx context.Context, options Options) (*exec.Cmd, error) {
	readPaths, err := canonicalReadPaths(options.AdditionalReadPaths)
	if err != nil {
		return nil, err
	}
	options.AdditionalReadPaths = readPaths
	executionAuthority, authorityBound := sandbox.ExecutionAuthorityFromContext(ctx)
	if authorityBound {
		// A caller may deliberately request a stricter, network-denied command
		// than its enclosing authority. Apply that reduction before comparing
		// managed-proxy bindings: this command cannot use the proxy.
		if !executionAuthority.AllowNetwork {
			options.DenyNetwork = true
		}
		if err := validateExecutionAuthority(options, executionAuthority); err != nil {
			return nil, err
		}
	}
	environment, err := SanitizedEnvironment(options.Env)
	if err != nil {
		return nil, err
	}
	if options.TrustedRuntimeHelper {
		environment = append(
			environment,
			tracecontext.Environment(ctx)...,
		)
	}
	policy, hasPolicy := sandbox.BackendPolicy(options.Sandbox)
	if hasPolicy {
		environment = dropHostLanguageEnvironment(environment, options.Env)
		environment = applyPreparedEnvironment(
			environment, policy.EnvironmentValues, options.Env,
		)
		environment = applyPreparedEnvironment(
			environment, policy.Toolchains.Environment, options.Env,
		)
		if policy.PrivateTemp != "" {
			if policy.EnvironmentProfile == "isolated" {
				environment = setEnvironmentValue(environment, "HOME", policy.PrivateTemp)
			}
			if !policy.SharedUserTemp {
				environment = setEnvironmentValue(environment, "TMPDIR", policy.PrivateTemp)
				environment = setEnvironmentValue(environment, "TMP", policy.PrivateTemp)
				environment = setEnvironmentValue(environment, "TEMP", policy.PrivateTemp)
			}
		}
		environment = prependPATH(environment, policy.Toolchains.BinDirs...)
	} else {
		environment = ensurePlatformToolchainPATH(environment)
	}
	environment = ensureGitToolchain(environment)
	if options.WorkspaceReadOnly {
		environment = setEnvironmentValue(environment, "GIT_OPTIONAL_LOCKS", "0")
		environment = setEnvironmentValue(environment, "PYTHONDONTWRITEBYTECODE", "1")
	}
	if strings.TrimSpace(options.Dir) == "" {
		return nil, errors.New("child process directory is required")
	}
	if (options.Command == "") == (options.Path == "") {
		return nil, errors.New("exactly one of shell command or executable path is required")
	}
	commandSpec := sandbox.Command{
		Dir: options.Dir, Env: environment,
		WorkspaceReadOnly:   options.WorkspaceReadOnly,
		AdditionalReadPaths: append([]string(nil), options.AdditionalReadPaths...),
		WorkspaceWritePaths: append([]string(nil), options.WorkspaceWritePaths...),
		WorkspaceHiddenPaths: append(
			[]string(nil),
			options.WorkspaceHiddenPaths...,
		),
		DenyNetwork: options.DenyNetwork,
		AllowLoopback: authorityBound &&
			executionAuthority.AllowLoopback &&
			!options.DenyNetwork,
		LoopbackOnly:     authorityBound && executionAuthority.LoopbackOnly(),
		SessionProxyPort: options.SessionProxyPort,
	}
	if authorityBound {
		commandSpec.AuthorityDigest = executionAuthority.Digest
	}
	if options.DirFile != nil {
		commandSpec.DirectoryFD = 3
	}
	if options.Path != "" {
		if strings.IndexByte(options.Path, 0) >= 0 {
			return nil, errors.New("executable path contains NUL")
		}
		commandSpec.Path = options.Path
		commandSpec.Args = append([]string{options.Path}, options.Args...)
	} else {
		commandSpec.Path = "sh"
		commandSpec.Args = []string{"sh", "-c", options.Command}
	}
	if options.RequireSandbox {
		required := sandbox.DefaultProcessRequirements()
		if authorityBound && !executionAuthority.RequiredControls.IsZero() {
			required = executionAuthority.RequiredControls
		}
		if err := sandbox.RequireControls(options.Sandbox, required); err != nil {
			return nil, sandbox.Denied(sandbox.Denial{
				Backend: backendName(options.Sandbox), Operation: sandbox.DenialProcess,
				Resource: "required_controls", ReasonCode: sandbox.ReasonBackendUnavailable,
			}, err)
		}
		if options.DirFile == nil {
			return nil, errors.New("strong sandbox execution requires a pinned workspace cwd descriptor")
		}
	}
	if len(options.WorkspaceWritePaths) != 0 && !options.WorkspaceReadOnly {
		return nil, errors.New("workspace write paths require a read-only workspace base")
	}
	if (options.WorkspaceReadOnly || options.DenyNetwork ||
		len(options.WorkspaceWritePaths) != 0) && options.Sandbox == nil {
		return nil, errors.New("process restrictions require a sandbox backend")
	}
	if options.Sandbox != nil {
		resolved, resolveErr := sandbox.ResolveExecutable(commandSpec.Path, environment)
		if resolveErr != nil {
			return nil, resolveErr
		}
		commandSpec.Path = resolved
		commandSpec.Args[0] = resolved
		if options.RequireSandbox && !hasPolicy {
			return nil, errors.New("strong sandbox backend has no prepared policy identity")
		}
		policy = sandbox.ApplySessionProxyPort(policy, commandSpec)
		environment = applyManagedProxyEnvironment(environment, policy, options.DenyNetwork)
		commandSpec.Env = environment
		commandSpec, err = options.Sandbox.Prepare(ctx, commandSpec)
		if err != nil {
			return nil, sandbox.WithDenialBackend(
				err,
				options.Sandbox.Capability().Backend,
			)
		}
		if options.RequireSandbox &&
			(commandSpec.PreparedPolicyID == "" ||
				commandSpec.PreparedPolicyID != policy.ID) {
			return nil, errors.New("sandbox backend returned an unverified prepared policy")
		}
		if authorityBound {
			if executionAuthority.EffectiveControls !=
				(controlmatrix.Matrix{}) {
				commandSpec.PreparedControls.ArtifactOrigin =
					executionAuthority.EffectiveControls.ArtifactOrigin
				commandSpec.PreparedControls.DurableRecovery =
					executionAuthority.EffectiveControls.DurableRecovery
			}
			if err := executionAuthority.RequiredControls.SatisfiedBy(
				commandSpec.PreparedControls,
			); err != nil {
				return nil, sandbox.Denied(sandbox.Denial{
					Backend:    options.Sandbox.Capability().Backend,
					Operation:  sandbox.DenialProcess,
					Resource:   "required_controls",
					ReasonCode: sandbox.ReasonBackendUnavailable,
				}, err)
			}
		}
		if authorityBound &&
			commandSpec.PreparedAuthorityDigest != executionAuthority.Digest {
			return nil, sandbox.Denied(sandbox.Denial{
				Backend:   options.Sandbox.Capability().Backend,
				Operation: sandbox.DenialProcess, Resource: executionAuthority.Digest,
				ReasonCode: sandbox.ReasonAuthorityUnverified,
			}, errors.New("sandbox backend returned an unverified execution authority"))
		}
		if options.WorkspaceReadOnly && !commandSpec.PreparedReadOnly {
			return nil, unenforcedRestriction(options.Sandbox, "workspace_read_only")
		}
		if !slices.Equal(options.AdditionalReadPaths, commandSpec.PreparedReadPaths) {
			return nil, unenforcedRestriction(options.Sandbox, "additional_read_paths")
		}
		if !slices.Equal(options.WorkspaceWritePaths, commandSpec.PreparedWritePaths) {
			return nil, errors.New("sandbox backend did not enforce exact workspace write paths")
		}
		if !slices.Equal(options.WorkspaceHiddenPaths, commandSpec.PreparedHiddenPaths) {
			return nil, errors.New("sandbox backend did not enforce hidden workspace paths")
		}
		if options.DenyNetwork && !commandSpec.PreparedNetworkDenied {
			return nil, errors.New("sandbox backend did not enforce network isolation")
		}
		expectedLoopback := authorityBound &&
			executionAuthority.AllowLoopback &&
			!options.DenyNetwork
		if commandSpec.PreparedLoopbackAllowed != expectedLoopback {
			return nil, unenforcedRestriction(options.Sandbox, "loopback_network")
		}
		expectedProxyPort := policy.ManagedProxyPort
		if options.DenyNetwork {
			expectedProxyPort = 0
		}
		if commandSpec.SessionProxyPort != 0 {
			expectedProxyPort = commandSpec.SessionProxyPort
		}
		if commandSpec.PreparedProxyPort != expectedProxyPort {
			return nil, unenforcedRestriction(options.Sandbox, "managed_network_proxy")
		}
	}
	if commandSpec.Path == "" || len(commandSpec.Args) == 0 {
		return nil, errors.New("sandbox returned an invalid command")
	}
	command, err := commandForSpec(ctx, commandSpec, options.DirFile)
	if err != nil {
		return nil, err
	}
	configureProcessGroup(command, options.PTY)
	command.Stdin = options.Stdin
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		return terminateProcessGroup(command.Process)
	}
	command.WaitDelay = 2 * time.Second
	return command, nil
}

func validateExecutionAuthority(
	options Options,
	authority sandbox.ExecutionAuthority,
) error {
	if err := authority.Validate(); err != nil {
		return err
	}
	switch authority.Enforcement {
	case "strong":
		if !options.RequireSandbox || options.Sandbox == nil {
			return sandbox.Denied(sandbox.Denial{
				Operation: sandbox.DenialProcess, Resource: "strong_sandbox",
				ReasonCode: sandbox.ReasonEnforcementMismatch,
			}, errors.New("strong execution authority requires strong sandbox enforcement"))
		}
	case "none":
		if options.RequireSandbox {
			return sandbox.Denied(sandbox.Denial{
				Operation: sandbox.DenialProcess, Resource: "unsandboxed",
				ReasonCode: sandbox.ReasonEnforcementMismatch,
			}, errors.New("unsandboxed execution authority cannot request a strong sandbox"))
		}
	}
	if !authority.AllowProcess {
		return sandbox.Denied(sandbox.Denial{
			Operation: sandbox.DenialProcess, Resource: "process",
			ReasonCode: sandbox.ReasonProcessNotAuthorized,
		}, nil)
	}
	if !authority.WorkspaceBaseWrite && !options.WorkspaceReadOnly {
		return sandbox.Denied(sandbox.Denial{
			Operation: sandbox.DenialWrite, Resource: authority.WorkspaceRoot,
			ReasonCode: sandbox.ReasonWorkspaceTreeDenied,
		}, nil)
	}
	if path, denied := authority.DeniedReadPath(options.AdditionalReadPaths); denied {
		return sandbox.Denied(sandbox.Denial{
			Operation: sandbox.DenialRead, Resource: path,
			ReasonCode: sandbox.ReasonPathReadNotAuthorized,
		}, nil)
	}
	if path, denied := authority.DeniedWritePath(options.WorkspaceWritePaths); denied {
		return sandbox.Denied(sandbox.Denial{
			Operation: sandbox.DenialWrite, Resource: path,
			ReasonCode: sandbox.ReasonPathWriteNotAuthorized,
		}, nil)
	}
	if options.NetworkHost != "" &&
		!slices.Contains(authority.NetworkTargets, networkTarget(options)) {
		return sandbox.Denied(sandbox.Denial{
			Operation: sandbox.DenialNetwork, Resource: options.NetworkHost,
			Protocol: options.NetworkProtocol, Port: options.NetworkPort,
			ReasonCode: sandbox.ReasonNetworkNotAuthorized,
		}, nil)
	}
	if !authority.AllowNetwork && !options.DenyNetwork {
		policyValue, policyBound := sandbox.BackendPolicy(options.Sandbox)
		if !policyBound || policyValue.AllowNetwork {
			return errors.New("process network access exceeds the effective permission profile")
		}
	}
	if policyValue, ok := sandbox.BackendPolicy(options.Sandbox); ok {
		// When this command is network-denied, the sandbox cannot reach the
		// managed proxy. A stale enclosing proxy port is therefore irrelevant to
		// this execution and must not block a local command.
		// Loopback-only authority likewise does not claim that proxy: localhost
		// bind/connect is a seatbelt grant, not a proxy-routed destination.
		if !options.DenyNetwork &&
			authority.ManagedProxyPort != policyValue.ManagedProxyPort &&
			!authority.LoopbackOnly() {
			return sandbox.Denied(sandbox.Denial{
				Operation: sandbox.DenialNetwork, Resource: "managed_proxy",
				ReasonCode: sandbox.ReasonAuthorityUnverified,
			}, errors.New("managed proxy does not match the effective permission profile"))
		}
	}
	return nil
}

func canonicalReadPaths(paths []string) ([]string, error) {
	canonical := make([]string, 0, len(paths))
	for _, path := range paths {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, err
		}
		absolute, err := filepath.Abs(resolved)
		if err != nil {
			return nil, err
		}
		canonical = append(canonical, filepath.Clean(absolute))
	}
	slices.Sort(canonical)
	return slices.Compact(canonical), nil
}

func networkTarget(options Options) string {
	return strings.ToLower(options.NetworkProtocol) + "://" +
		net.JoinHostPort(
			strings.ToLower(options.NetworkHost),
			strconv.Itoa(int(options.NetworkPort)),
		)
}

func backendName(backend sandbox.Backend) string {
	if backend == nil {
		return "none"
	}
	return backend.Capability().Backend
}

func unenforcedRestriction(backend sandbox.Backend, resource string) error {
	return sandbox.Denied(sandbox.Denial{
		Backend: backendName(backend), Operation: sandbox.DenialProcess,
		Resource: resource, ReasonCode: sandbox.ReasonRestrictionUnenforced,
	}, nil)
}

func applyManagedProxyEnvironment(
	environment []string,
	policy sandbox.Policy,
	denyNetwork bool,
) []string {
	result := make([]string, 0, len(environment)+6)
	for _, entry := range environment {
		if proxyEnvironmentEntry(entry) {
			continue
		}
		result = append(result, entry)
	}
	if policy.ManagedProxyPort != 0 && !denyNetwork {
		proxyURL := "http://127.0.0.1:" + strconv.Itoa(int(policy.ManagedProxyPort))
		result = append(result,
			"HTTP_PROXY="+proxyURL, "HTTPS_PROXY="+proxyURL,
			"http_proxy="+proxyURL, "https_proxy="+proxyURL,
			"NO_PROXY=", "no_proxy=",
		)
	}
	return result
}

func proxyEnvironmentEntry(entry string) bool {
	name, _, _ := strings.Cut(entry, "=")
	switch strings.ToUpper(name) {
	case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
		return true
	default:
		return false
	}
}

// ensurePlatformToolchainPATH makes the platform's canonical tool
// directories resolvable for children that run without a sandbox policy
// (for example MCP stdio servers). It is language-agnostic: the same
// platform PATH sources the sandbox discovery uses, so go, git, cargo, or
// python installations behave identically. Sandboxed commands get their
// PATH from the policy's discovered toolchain exposure instead.
func ensurePlatformToolchainPATH(environment []string) []string {
	return prependPATH(environment, sandbox.PlatformPATHDirectories()...)
}

func prependPATH(environment []string, directories ...string) []string {
	if len(directories) == 0 {
		return environment
	}
	pathValue := environmentValue(environment, "PATH")
	parts := filepath.SplitList(pathValue)
	seen := make(map[string]bool, len(parts)+len(directories))
	for _, part := range parts {
		seen[filepath.Clean(part)] = true
	}
	var prefix []string
	for _, directory := range directories {
		if directory == "" || !filepath.IsAbs(directory) {
			continue
		}
		clean := filepath.Clean(directory)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		prefix = append(prefix, clean)
	}
	if len(prefix) == 0 {
		return environment
	}
	newPath := strings.Join(append(prefix, parts...), string(os.PathListSeparator))
	result := make([]string, 0, len(environment)+1)
	replaced := false
	for _, entry := range environment {
		if strings.HasPrefix(entry, "PATH=") {
			result = append(result, "PATH="+newPath)
			replaced = true
			continue
		}
		result = append(result, entry)
	}
	if !replaced {
		result = append(result, "PATH="+newPath)
	}
	return result
}

func environmentValue(environment []string, name string) string {
	prefix := name + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			return entry[len(prefix):]
		}
	}
	return ""
}

func setEnvironmentValue(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	replaced := false
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			if !replaced {
				result = append(result, prefix+value)
				replaced = true
			}
			continue
		}
		result = append(result, entry)
	}
	if !replaced {
		result = append(result, prefix+value)
	}
	return result
}

// observedBuffer retains bounded head/tail output while forwarding complete
// chunks to live observers and an optional durable archive.
type observedBuffer struct {
	buffer  *headTailBuffer
	stream  Stream
	observe func(Chunk)
	archive *archiveState
	cursor  uint64
}

func newObservedBuffer(
	stream Stream,
	limit int,
	observe func(Chunk),
	archive *archiveState,
) *observedBuffer {
	return &observedBuffer{
		buffer: newHeadTailBuffer(limit), stream: stream, observe: observe, archive: archive,
	}
}

func (o *observedBuffer) Write(data []byte) (int, error) {
	count, err := o.buffer.Write(data)
	if count > 0 {
		o.cursor += uint64(count)
		if o.observe != nil || o.archive != nil {
			// Readers reuse their slice. Both consumers receive a stable copy for
			// the duration of their synchronous callback.
			chunkData := append([]byte(nil), data[:count]...)
			chunk := Chunk{Stream: o.stream, Data: chunkData, Cursor: o.cursor}
			o.archive.write(chunk)
			if o.observe != nil {
				o.observe(chunk)
			}
		}
	}
	return count, err
}

func (o *observedBuffer) String() string { return o.buffer.String() }

func (o *observedBuffer) Receipt() StreamReceipt { return o.buffer.Receipt() }

func runPTY(
	ctx context.Context,
	command *exec.Cmd,
	observe func(Chunk),
	observeTeardown func(time.Duration),
	archive *archiveState,
	limit int,
) (Result, error) {
	terminal, err := pty.Start(command)
	if err != nil {
		return Result{}, err
	}
	// A pty merges the two streams, so everything it produces is reported as
	// stdout rather than guessed at.
	output := newObservedBuffer(StreamStdout, limit, observe, archive)
	copyDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(output, terminal)
		copyDone <- copyErr
	}()
	waitErr, teardown := waitAndCancel(ctx, command)
	if teardown > 0 && observeTeardown != nil {
		observeTeardown(teardown)
	}
	_ = terminal.Close()
	copyErr := <-copyDone
	if copyErr != nil &&
		!errors.Is(copyErr, syscall.EIO) &&
		!errors.Is(copyErr, os.ErrClosed) &&
		ctx.Err() == nil {
		return Result{}, copyErr
	}
	result := Result{
		Stdout: output.String(), ExitCode: ExitCode(waitErr),
		OutputReceipt: OutputReceipt{
			Stdout: output.Receipt(), ArchiveError: archive.errorString(),
		},
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}

func waitAndCancel(ctx context.Context, command *exec.Cmd) (error, time.Duration) {
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		return err, 0
	case <-ctx.Done():
		started := time.Now()
		_ = terminateProcessGroup(command.Process)
		<-done
		return ctx.Err(), time.Since(started)
	}
}

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}
