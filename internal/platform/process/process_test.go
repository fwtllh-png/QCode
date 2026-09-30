//go:build darwin

package process

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/common/tracecontext"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestToolchainSearchPathOrdersAndDedupes(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	host := t.TempDir()
	t.Setenv(
		"PATH",
		strings.Join([]string{"", host, host, first}, string(os.PathListSeparator)),
	)
	ordered := ToolchainSearchPath([]string{"PATH=" + strings.Join([]string{second, first, "", host, host, first}, string(os.PathListSeparator))})
	if len(ordered) == 0 || ordered[0] != second {
		t.Fatalf("toolchain bins must resolve first: %v", ordered)
	}
	seen := make(map[string]bool)
	for _, entry := range ordered {
		if entry == "" {
			t.Fatalf("empty PATH entry survived: %v", ordered)
		}
		if seen[entry] {
			t.Fatalf("duplicate entry %q survived: %v", entry, ordered)
		}
		seen[entry] = true
	}
	if !seen[host] || !seen[first] {
		t.Fatalf("host entries lost: %v", ordered)
	}
}

func TestObservedBufferArchivesCompleteOutputBeyondRetention(t *testing.T) {
	var archived bytes.Buffer
	archive := &archiveState{append: func(chunk Chunk) error {
		_, err := archived.Write(chunk.Data)
		return err
	}}
	buffer := newObservedBuffer(StreamStdout, 8, nil, archive)
	for _, value := range []string{"abcd", "efgh", "ijkl"} {
		if _, err := buffer.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if archived.String() != "abcdefghijkl" {
		t.Fatalf("archive = %q", archived.String())
	}
	if output := buffer.String(); !strings.HasPrefix(output, "abcd\n...") ||
		!strings.HasSuffix(output, "ijkl") {
		t.Fatalf("bounded output = %q", output)
	}
	if receipt := buffer.Receipt(); receipt.OmittedBytes != 4 {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestRunCapturesStreamsAndExitCode(t *testing.T) {
	result, err := Run(t.Context(), Options{
		Command: "printf out; printf err >&2; exit 7",
		Dir:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "out" || result.Stderr != "err" || result.ExitCode != 7 {
		t.Fatalf("result = %+v", result)
	}
	if result.OutputReceipt.Stdout.TotalBytes != 3 ||
		result.OutputReceipt.Stderr.TotalBytes != 3 ||
		result.OutputReceipt.Stdout.Truncated() ||
		result.OutputReceipt.Stderr.Truncated() {
		t.Fatalf("output receipt = %+v", result.OutputReceipt)
	}
}

func TestRunBoundsStreamsAndArchivesCompleteOutput(t *testing.T) {
	const (
		produced = 2048
		limit    = 1024
	)
	type archiveValue struct {
		mu     sync.Mutex
		totals map[Stream]uint64
	}
	archive := &archiveValue{totals: make(map[Stream]uint64)}
	var streamed sync.Map
	result, err := Run(t.Context(), Options{
		Command: `(dd if=/dev/zero bs=2048 count=1 2>/dev/null | tr '\000' x); ` +
			`(dd if=/dev/zero bs=2048 count=1 2>/dev/null | tr '\000' y >&2)`,
		Dir:              t.TempDir(),
		OutputLimitBytes: limit,
		OnOutput: func(chunk Chunk) {
			value, _ := streamed.LoadOrStore(chunk.Stream, new(uint64))
			*value.(*uint64) = chunk.Cursor
		},
		ArchiveOutput: func(chunk Chunk) error {
			archive.mu.Lock()
			archive.totals[chunk.Stream] += uint64(len(chunk.Data))
			archive.mu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []Stream{StreamStdout, StreamStderr} {
		var receipt StreamReceipt
		if stream == StreamStdout {
			receipt = result.OutputReceipt.Stdout
		} else {
			receipt = result.OutputReceipt.Stderr
		}
		if receipt != (StreamReceipt{
			TotalBytes: produced, RetainedBytes: limit, OmittedBytes: produced - limit,
		}) {
			t.Fatalf("%s receipt = %+v", stream, receipt)
		}
		value, ok := streamed.Load(stream)
		if !ok || *value.(*uint64) != produced {
			t.Fatalf("%s streamed cursor = %v", stream, value)
		}
		archive.mu.Lock()
		archived := archive.totals[stream]
		archive.mu.Unlock()
		if archived != produced {
			t.Fatalf("%s archived bytes = %d", stream, archived)
		}
	}
	if len(result.Stdout) > limit+128 || len(result.Stderr) > limit+128 {
		t.Fatalf("bounded result lengths = stdout:%d stderr:%d", len(result.Stdout), len(result.Stderr))
	}
}

func TestRunBoundsPTYMergedOutput(t *testing.T) {
	result, err := Run(t.Context(), Options{
		Command:          `dd if=/dev/zero bs=2048 count=1 2>/dev/null | tr '\000' z`,
		Dir:              t.TempDir(),
		PTY:              true,
		OutputLimitBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.OutputReceipt.Stdout.TotalBytes != 2048 ||
		result.OutputReceipt.Stdout.RetainedBytes != 1024 ||
		result.OutputReceipt.Stdout.OmittedBytes != 1024 ||
		result.OutputReceipt.Stderr.TotalBytes != 0 ||
		len(result.Stdout) > 1024+128 {
		t.Fatalf("PTY result = %+v", result)
	}
}

func TestRunReportsArchiveFailureWithoutLosingBoundedResult(t *testing.T) {
	result, err := Run(t.Context(), Options{
		Command: "printf retained",
		Dir:     t.TempDir(),
		ArchiveOutput: func(Chunk) error {
			return errors.New("archive unavailable")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "retained" ||
		result.OutputReceipt.ArchiveError != "archive unavailable" {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunRejectsNegativeOutputLimit(t *testing.T) {
	_, err := Run(t.Context(), Options{
		Command: "true", Dir: t.TempDir(), OutputLimitBytes: -1,
	})
	if err == nil || !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestRunCancellationRetainsBoundedOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	result, err := Run(ctx, Options{
		Command: `while :; do printf '0123456789abcdef'; done`,
		Dir:     t.TempDir(), OutputLimitBytes: 1024,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v", err)
	}
	if result.OutputReceipt.Stdout.RetainedBytes > 1024 ||
		len(result.Stdout) > 1024+128 {
		t.Fatalf("result was not bounded: %+v", result.OutputReceipt.Stdout)
	}
}

func TestRunPTYAndCancellation(t *testing.T) {
	result, err := Run(t.Context(), Options{Command: "printf terminal", Dir: t.TempDir(), PTY: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Stdout, "terminal") || result.ExitCode != 0 {
		t.Fatalf("result = %+v", result)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, err = Run(ctx, Options{Command: "sleep 30 & wait", Dir: t.TempDir()})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestRunSanitizesRegularAndPTYEnvironments(t *testing.T) {
	t.Setenv("QCODE_API_KEY", "must-not-reach-child")
	t.Setenv("UNRELATED_SECRET_TOKEN", "must-not-reach-child")
	for _, pty := range []bool{false, true} {
		result, err := Run(t.Context(), Options{
			Command: `printf 'path=%s api=%s token=%s' "$PATH" "$QCODE_API_KEY" "$UNRELATED_SECRET_TOKEN"`,
			Dir:     t.TempDir(), PTY: pty,
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(result.Stdout, "must-not-reach-child") ||
			!strings.Contains(result.Stdout, "path=") {
			t.Fatalf("PTY=%t output = %q", pty, result.Stdout)
		}
	}
}

func TestTraceContextOnlyReachesTrustedRuntimeHelpers(t *testing.T) {
	ctx, err := tracecontext.NewRoot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := tracecontext.Current(ctx)
	regular, err := NewCommand(ctx, Options{
		Command: "true",
		Dir:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if environmentValue(regular.Env, tracecontext.EnvironmentTraceParent) != "" {
		t.Fatal("ordinary user command received internal trace context")
	}
	trusted, err := NewCommand(ctx, Options{
		Command:              "true",
		Dir:                  t.TempDir(),
		TrustedRuntimeHelper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	carrier := map[string]string{
		tracecontext.HeaderTraceParent: environmentValue(
			trusted.Env,
			tracecontext.EnvironmentTraceParent,
		),
		tracecontext.HeaderTraceState: environmentValue(
			trusted.Env,
			tracecontext.EnvironmentTraceState,
		),
	}
	extracted, err := tracecontext.ExtractMap(context.Background(), carrier)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := tracecontext.Current(extracted)
	if !ok || got.TraceID != want.TraceID || got.SpanID != want.SpanID {
		t.Fatalf("want=%+v got=%+v", want, got)
	}
}

func TestRunUsesInjectedStrongSandboxBackend(t *testing.T) {
	root := t.TempDir()
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	backend := &recordingBackend{root: root}
	result, err := Run(t.Context(), Options{
		Command:        "printf sandboxed",
		Dir:            root,
		DirFile:        directory,
		Env:            []string{"LANG=C"},
		Sandbox:        backend,
		RequireSandbox: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "sandboxed" || result.ExitCode != 0 {
		t.Fatalf("result = %+v", result)
	}
	if !filepath.IsAbs(backend.command.Path) ||
		backend.command.Args[0] != backend.command.Path ||
		backend.command.Dir != root ||
		!slices.Contains(backend.command.Env, "LANG=C") {
		t.Fatalf("prepared command = %+v", backend.command)
	}
}

func TestStructuredCommandUsesSanitizedEnvironmentAndSandbox(t *testing.T) {
	t.Setenv("QCODE_API_KEY", "must-not-reach-child")
	root := t.TempDir()
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	backend := &recordingBackend{root: root}
	result, err := Run(t.Context(), Options{
		Path: "sh",
		Args: []string{"-c", `printf '%s' "$QCODE_API_KEY"`},
		Dir:  root, DirFile: directory, Sandbox: backend, RequireSandbox: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "" || result.ExitCode != 0 {
		t.Fatalf("result = %+v", result)
	}
	if !filepath.IsAbs(backend.command.Path) ||
		backend.command.Args[0] != backend.command.Path ||
		!slices.Equal(
			backend.command.Args[1:],
			[]string{"-c", `printf '%s' "$QCODE_API_KEY"`},
		) {
		t.Fatalf("prepared structured command = %+v", backend.command)
	}
	for _, entry := range backend.command.Env {
		if strings.Contains(entry, "QCODE_API_KEY") {
			t.Fatalf("secret environment reached backend: %q", entry)
		}
	}
}

func TestV1DropsHostLanguageEnvironmentUnlessExtraOrPrepared(t *testing.T) {
	t.Setenv("GOPROXY", "https://proxy.internal.example")
	t.Setenv("GOROOT", "/host/go")
	t.Setenv("LANG", "C")
	root := t.TempDir()
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	backend := &recordingBackend{
		root:                root,
		environmentContract: "v1",
		environmentValues:   []string{"GOROOT=/prepared/go", "LANG=C", "PATH=/usr/bin:/bin"},
	}
	result, err := Run(t.Context(), Options{
		Command: "printf ok", Dir: root, DirFile: directory,
		Env:     []string{"GOPROXY=http://127.0.0.1:9"},
		Sandbox: backend, RequireSandbox: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "ok" || result.ExitCode != 0 {
		t.Fatalf("result = %+v", result)
	}
	if environmentValue(backend.command.Env, "GOPROXY") != "http://127.0.0.1:9" {
		t.Fatalf("extra GOPROXY lost: %v", backend.command.Env)
	}
	if environmentValue(backend.command.Env, "GOROOT") != "/prepared/go" {
		t.Fatalf("prepared GOROOT lost: %v", backend.command.Env)
	}
	if environmentValue(backend.command.Env, "LANG") != "C" {
		t.Fatalf("LANG dropped: %v", backend.command.Env)
	}
}

func TestSandboxEnvironmentDoesNotRewriteHomeOrCaches(t *testing.T) {
	privateTemp := filepath.Join(t.TempDir(), "private")
	environment := applyManagedProxyEnvironment([]string{
		"HOME=/host/home",
		"TMPDIR=/host/tmpdir",
		"GOCACHE=/host/cache",
		"LANG=C",
	}, sandbox.Policy{
		PrivateTemp:         privateTemp,
		EnvironmentContract: "v1",
	}, true)
	if environmentValue(environment, "HOME") != "/host/home" ||
		environmentValue(environment, "TMPDIR") != "/host/tmpdir" ||
		environmentValue(environment, "GOCACHE") != "/host/cache" ||
		environmentValue(environment, "LANG") != "C" {
		t.Fatalf("home rewrite leaked: %v", environment)
	}
	for _, name := range []string{"GOTMPDIR", "GOMODCACHE"} {
		if environmentValue(environment, name) != "" {
			t.Fatalf("injected %s=%q", name, environmentValue(environment, name))
		}
	}
}

func TestV1ShellUsesNonLoginCommand(t *testing.T) {
	root := t.TempDir()
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	backend := &recordingBackend{root: root, environmentContract: "v1"}
	result, err := Run(t.Context(), Options{
		Command: "printf v1", Dir: root, DirFile: directory,
		Sandbox: backend, RequireSandbox: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "v1" || result.ExitCode != 0 {
		t.Fatalf("result = %+v", result)
	}
	if len(backend.command.Args) < 2 || backend.command.Args[1] != "-c" {
		t.Fatalf("v1 shell args = %+v", backend.command.Args)
	}
	for _, arg := range backend.command.Args {
		if arg == "-lc" {
			t.Fatalf("v1 still used a login shell: %+v", backend.command.Args)
		}
	}
}

func TestRunFailsClosedWithoutStrongSandbox(t *testing.T) {
	_, err := Run(t.Context(), Options{
		Command: "true", Dir: t.TempDir(), RequireSandbox: true,
	})
	if !sandbox.IsUnavailable(err) ||
		!strings.Contains(err.Error(), sandbox.ErrUnavailableCode) {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestRunPropagatesAndVerifiesReadOnlyRestrictions(t *testing.T) {
	root := t.TempDir()
	writePath := filepath.Join(root, "generated.txt")
	if err := os.WriteFile(writePath, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	directoryFile, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directoryFile.Close()
	backend := &recordingBackend{root: root}
	result, err := Run(t.Context(), Options{
		Command: "printf ok", Dir: root, DirFile: directoryFile,
		Sandbox: backend, RequireSandbox: true,
		WorkspaceReadOnly: true, DenyNetwork: true,
		WorkspaceWritePaths: []string{writePath},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "ok" || !backend.command.WorkspaceReadOnly ||
		!backend.command.DenyNetwork ||
		!slices.Equal(backend.command.WorkspaceWritePaths, []string{writePath}) {
		t.Fatalf("result=%+v command=%+v", result, backend.command)
	}
	if environmentValue(backend.command.Env, "GIT_OPTIONAL_LOCKS") != "0" ||
		environmentValue(backend.command.Env, "PYTHONDONTWRITEBYTECODE") != "" {
		t.Fatalf("read-only environment = %v", backend.command.Env)
	}
}

func TestRunRejectsBackendThatDoesNotAcknowledgeExactWritePaths(t *testing.T) {
	root := t.TempDir()
	writePath := filepath.Join(root, "generated.txt")
	if err := os.WriteFile(writePath, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	directoryFile, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directoryFile.Close()
	_, err = Run(t.Context(), Options{
		Command: "true", Dir: root, DirFile: directoryFile,
		Sandbox: &recordingBackend{
			root: root, ignoreWritePaths: true,
		},
		RequireSandbox:      true,
		WorkspaceReadOnly:   true,
		WorkspaceWritePaths: []string{writePath},
	})
	if err == nil || !strings.Contains(err.Error(), "exact workspace write paths") {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestRunRejectsBackendThatDoesNotAcknowledgeRestrictions(t *testing.T) {
	root := t.TempDir()
	directoryFile, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directoryFile.Close()
	_, err = Run(t.Context(), Options{
		Command: "true", Dir: root, DirFile: directoryFile,
		Sandbox:           &recordingBackend{root: root, ignoreRestrictions: true},
		RequireSandbox:    true,
		WorkspaceReadOnly: true, DenyNetwork: true,
	})
	denial, ok := sandbox.DenialFromError(err)
	if !ok || denial.ReasonCode != sandbox.ReasonRestrictionUnenforced {
		t.Fatalf("Run() denial = %+v error=%v", denial, err)
	}
}

func TestRunVerifiesEffectiveExecutionAuthority(t *testing.T) {
	root := t.TempDir()
	directoryFile, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directoryFile.Close()
	ctx, err := sandbox.WithExecutionAuthority(t.Context(), sandbox.ExecutionAuthority{
		EffectiveControls: testNetworkControls(securitymodel.NetworkDenied),
		Digest:            strings.Repeat("a", 64), Enforcement: "strong",
		WorkspaceRoot: root, AllowNetwork: false, AllowProcess: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Run(ctx, Options{
		Command: "true", Dir: root, DirFile: directoryFile,
		Sandbox: &recordingBackend{
			root: root, ignoreAuthority: true,
		},
		RequireSandbox:    true,
		WorkspaceReadOnly: true, DenyNetwork: true,
	})
	if err == nil || !strings.Contains(err.Error(), "execution authority") {
		t.Fatalf("unverified authority error = %v", err)
	}
	result, err := Run(ctx, Options{
		Command: "printf ok", Dir: root, DirFile: directoryFile,
		Sandbox:           &recordingBackend{root: root},
		RequireSandbox:    true,
		WorkspaceReadOnly: true, DenyNetwork: true,
	})
	if err != nil || result.Stdout != "ok" {
		t.Fatalf("verified authority result=%+v error=%v", result, err)
	}
}

func TestRunRejectsPreparedControlsBelowAuthority(t *testing.T) {
	root := t.TempDir()
	directoryFile, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directoryFile.Close()
	ctx, err := sandbox.WithExecutionAuthority(t.Context(), sandbox.ExecutionAuthority{
		EffectiveControls: testNetworkControls(securitymodel.NetworkDenied),
		Digest:            strings.Repeat("e", 64), Enforcement: "strong",
		WorkspaceRoot: root, AllowProcess: true,
		RequiredControls: securitymodel.RequiredControls{
			Network: securitymodel.NetworkDenied,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	weaker := testControlMatrix()
	weaker.Network = securitymodel.NetworkDirect
	_, err = Run(ctx, Options{
		Command: "true", Dir: root, DirFile: directoryFile,
		Sandbox: &recordingBackend{
			root: root, preparedControls: &weaker,
		},
		RequireSandbox:    true,
		WorkspaceReadOnly: true, DenyNetwork: true,
	})
	denial, ok := sandbox.DenialFromError(err)
	if !ok || denial.Resource != "required_controls" {
		t.Fatalf("weaker prepared controls denial = %+v error=%v", denial, err)
	}
}

func TestRunRejectsProcessBroaderThanEffectiveAuthority(t *testing.T) {
	root := t.TempDir()
	ctx, err := sandbox.WithExecutionAuthority(t.Context(), sandbox.ExecutionAuthority{
		EffectiveControls: testNetworkControls(securitymodel.NetworkDenied),
		Digest:            strings.Repeat("b", 64), Enforcement: "strong",
		WorkspaceRoot: root, AllowNetwork: false, AllowProcess: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewCommand(ctx, Options{
		Command: "true", Dir: root,
		Sandbox: &recordingBackend{root: root}, RequireSandbox: true,
	})
	denial, ok := sandbox.DenialFromError(err)
	if !ok || denial.ReasonCode != sandbox.ReasonWorkspaceTreeDenied ||
		denial.Amendable() {
		t.Fatalf("broader process denial = %+v error=%v", denial, err)
	}
}

func TestRunProducesAmendableTypedPathDenial(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "result.txt")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, err := sandbox.WithExecutionAuthority(t.Context(), sandbox.ExecutionAuthority{
		EffectiveControls: testNetworkControls(securitymodel.NetworkDirect),
		Digest:            strings.Repeat("c", 64), Enforcement: "strong",
		WorkspaceRoot: root, AllowNetwork: true, AllowProcess: true,
		ReadPaths: []string{root},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewCommand(ctx, Options{
		Command: "true", Dir: root,
		Sandbox: &recordingBackend{root: root}, RequireSandbox: true,
		WorkspaceReadOnly: true, WorkspaceWritePaths: []string{path},
	})
	denial, ok := sandbox.DenialFromError(err)
	if !ok || denial.Operation != sandbox.DenialWrite ||
		denial.Resource != path || !denial.Amendable() {
		t.Fatalf("path denial = %+v error=%v", denial, err)
	}
}

func TestRunAppliesApprovedAdditionalReadPath(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "approved.txt")
	if err := os.WriteFile(path, []byte("approved"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	directoryFile, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directoryFile.Close()
	ctx, err := sandbox.WithExecutionAuthority(t.Context(), sandbox.ExecutionAuthority{
		EffectiveControls: testNetworkControls(securitymodel.NetworkDenied),
		Digest:            strings.Repeat("d", 64), Enforcement: "strong",
		WorkspaceRoot: root, AllowNetwork: false, AllowProcess: true,
		ReadPaths: []string{root, path},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(ctx, Options{
		Path: "cat", Args: []string{path}, Dir: root, DirFile: directoryFile,
		Sandbox: &recordingBackend{root: root}, RequireSandbox: true,
		WorkspaceReadOnly: true, AdditionalReadPaths: []string{path},
		DenyNetwork: true,
	})
	if err != nil || result.Stdout != "approved" {
		t.Fatalf("Run() result=%+v error=%v", result, err)
	}
}

func TestRunInjectsOnlyVerifiedManagedProxy(t *testing.T) {
	root := t.TempDir()
	directoryFile, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directoryFile.Close()
	ctx, err := sandbox.WithExecutionAuthority(t.Context(), sandbox.ExecutionAuthority{
		EffectiveControls: testNetworkControls(securitymodel.NetworkProxyTargets),
		Digest:            strings.Repeat("e", 64), Enforcement: "strong",
		WorkspaceRoot: root, AllowNetwork: true, AllowProcess: true,
		ReadPaths: []string{root}, ManagedProxyPort: 43128,
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &recordingBackend{root: root, proxyPort: 43128}
	_, err = Run(ctx, Options{
		Command: "true", Dir: root, DirFile: directoryFile,
		Sandbox: backend, RequireSandbox: true,
		WorkspaceReadOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if environmentValue(backend.command.Env, "HTTPS_PROXY") !=
		"http://127.0.0.1:43128" ||
		environmentValue(backend.command.Env, "NO_PROXY") != "" {
		t.Fatalf("managed proxy environment = %v", backend.command.Env)
	}
}

func TestRunAllowsDeniedNetworkAuthorityOnManagedProxyBackend(t *testing.T) {
	root := t.TempDir()
	directoryFile, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directoryFile.Close()
	ctx, err := sandbox.WithExecutionAuthority(t.Context(), sandbox.ExecutionAuthority{
		EffectiveControls: testNetworkControls(securitymodel.NetworkDenied),
		Digest:            strings.Repeat("e", 64), Enforcement: "strong",
		WorkspaceRoot: root, AllowNetwork: false, AllowProcess: true,
		ReadPaths: []string{root},
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &recordingBackend{root: root, proxyPort: 43128}
	if _, err := Run(ctx, Options{
		Command: "true", Dir: root, DirFile: directoryFile,
		Sandbox: backend, RequireSandbox: true,
		WorkspaceReadOnly: true,
	}); err != nil {
		t.Fatal(err)
	}
	if !backend.command.DenyNetwork {
		t.Fatal("denied network authority did not constrain the sandbox command")
	}
}

func TestRunRejectsNetworkAuthorityWithoutManagedProxyBinding(t *testing.T) {
	root := t.TempDir()
	ctx, err := sandbox.WithExecutionAuthority(t.Context(), sandbox.ExecutionAuthority{
		EffectiveControls: testNetworkControls(securitymodel.NetworkDirect),
		Digest:            strings.Repeat("e", 64), Enforcement: "strong",
		WorkspaceRoot: root, AllowNetwork: true, AllowProcess: true,
		ReadPaths: []string{root},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Run(ctx, Options{
		Command: "true", Dir: root,
		Sandbox:        &recordingBackend{root: root, proxyPort: 43128},
		RequireSandbox: true, WorkspaceReadOnly: true,
	})
	denial, ok := sandbox.DenialFromError(err)
	if !ok || denial.Resource != "managed_proxy" ||
		denial.ReasonCode != sandbox.ReasonAuthorityUnverified {
		t.Fatalf("managed proxy denial = %+v error=%v", denial, err)
	}
}

func TestRunAllowsNetworkDeniedCommandWithStaleManagedProxyAuthority(t *testing.T) {
	root := t.TempDir()
	directoryFile, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directoryFile.Close()
	ctx, err := sandbox.WithExecutionAuthority(t.Context(), sandbox.ExecutionAuthority{
		EffectiveControls: testNetworkControls(securitymodel.NetworkProxyTargets),
		Digest:            strings.Repeat("e", 64), Enforcement: "strong",
		WorkspaceRoot: root, AllowNetwork: true, AllowProcess: true,
		ReadPaths: []string{root}, ManagedProxyPort: 43129,
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &recordingBackend{root: root, proxyPort: 43128}
	if _, err := Run(ctx, Options{
		Command: "true", Dir: root, DirFile: directoryFile,
		Sandbox: backend, RequireSandbox: true,
		WorkspaceReadOnly: true, DenyNetwork: true,
	}); err != nil {
		t.Fatal(err)
	}
	if !backend.command.DenyNetwork || backend.command.PreparedProxyPort != 0 {
		t.Fatalf("network-denied command = %+v", backend.command)
	}
}

func TestRunAllowsLoopbackOnlyAuthorityOnManagedProxyBackend(t *testing.T) {
	root := t.TempDir()
	directoryFile, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directoryFile.Close()
	ctx, err := sandbox.WithExecutionAuthority(t.Context(), sandbox.ExecutionAuthority{
		EffectiveControls: testNetworkControls(securitymodel.NetworkLoopbackAny),
		Digest:            strings.Repeat("f", 64), Enforcement: "strong",
		WorkspaceRoot: root, AllowNetwork: true, AllowProcess: true,
		ReadPaths: []string{root}, AllowLoopback: true,
		NetworkTargets: nil,
		RequiredControls: securitymodel.RequiredControls{
			Network: securitymodel.NetworkLoopbackAny,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &recordingBackend{root: root, proxyPort: 43128}
	_, err = Run(ctx, Options{
		Command: "true", Dir: root, DirFile: directoryFile,
		Sandbox: backend, RequireSandbox: true,
		WorkspaceReadOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !backend.command.AllowLoopback {
		t.Fatal("loopback-only authority was not bound to the sandbox command")
	}
	if !backend.command.LoopbackOnly {
		t.Fatal("loopback-only authority retained the workspace proxy grant")
	}
	for _, entry := range backend.command.Env {
		if proxyEnvironmentEntry(entry) {
			t.Fatalf("loopback-only command inherited proxy environment %q", entry)
		}
	}
}

func TestRunBindsApprovedLoopbackToSandboxCommand(t *testing.T) {
	root := t.TempDir()
	directoryFile, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directoryFile.Close()
	ctx, err := sandbox.WithExecutionAuthority(t.Context(), sandbox.ExecutionAuthority{
		EffectiveControls: testNetworkControls(securitymodel.NetworkProxyTargets),
		Digest:            strings.Repeat("f", 64), Enforcement: "strong",
		WorkspaceRoot: root, AllowNetwork: true, AllowProcess: true,
		ReadPaths: []string{root}, ManagedProxyPort: 43128,
		AllowLoopback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &recordingBackend{root: root, proxyPort: 43128}
	_, err = Run(ctx, Options{
		Command: "true", Dir: root, DirFile: directoryFile,
		Sandbox: backend, RequireSandbox: true,
		WorkspaceReadOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !backend.command.AllowLoopback {
		t.Fatal("approved loopback was not bound to the sandbox command")
	}
}

type recordingBackend struct {
	command             sandbox.Command
	root                string
	proxyPort           uint16
	environmentContract string
	environmentValues   []string
	preparedControls    *securitymodel.Controls
	ignoreRestrictions  bool
	ignoreWritePaths    bool
	ignoreAuthority     bool
}

func (b *recordingBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "fixture", Backend: "recording", ManagedProxy: b.proxyPort != 0,
		Available: true,
		Effective: testControlMatrix(),
	}
}

func (b *recordingBackend) Prepare(_ context.Context, command sandbox.Command) (sandbox.Command, error) {
	b.command = command
	command.PreparedPolicyID = "fixture-policy"
	var err error
	command.PreparedControls, err = sandbox.CommandControls(
		b.Capability(), b.Policy(), command,
	)
	if err != nil {
		return sandbox.Command{}, err
	}
	if b.preparedControls != nil {
		command.PreparedControls = *b.preparedControls
	}
	if !b.ignoreAuthority {
		command.PreparedAuthorityDigest = command.AuthorityDigest
	}
	if !b.ignoreRestrictions {
		command.PreparedReadOnly = command.WorkspaceReadOnly
		command.PreparedReadPaths = append(
			[]string(nil),
			command.AdditionalReadPaths...,
		)
		command.PreparedNetworkDenied = command.DenyNetwork
		command.PreparedLoopbackAllowed = command.AllowLoopback && !command.DenyNetwork
		command.PreparedProxyPort = sandbox.ApplySessionProxyPort(b.Policy(), command).ManagedProxyPort
		if !b.ignoreWritePaths {
			command.PreparedWritePaths = append(
				[]string(nil), command.WorkspaceWritePaths...,
			)
		}
	}
	return command, nil
}

func testControlMatrix() securitymodel.Controls {
	return securitymodel.Controls{
		FilesystemRead:  securitymodel.FilesystemReadDeclaredRoots,
		FilesystemWrite: securitymodel.FilesystemWriteExactPaths,
		Network:         securitymodel.NetworkDenied,
		ProcessTree:     securitymodel.ProcessTreeGroupKill,
		CrossProcess:    securitymodel.CrossProcessRestricted,
		Syscall:         securitymodel.SyscallDenyDangerous,
		IPC:             securitymodel.IPCUnixOnly,
		PathIdentity:    securitymodel.PathIdentityDescriptorRelative,
		ArtifactOrigin:  securitymodel.ArtifactOriginVerifiedManifest,
		DurableRecovery: securitymodel.DurableRecoveryExternalJournal,
	}
}

func (b *recordingBackend) Policy() sandbox.Policy {
	return sandbox.Policy{
		Version: 1, ID: "fixture-policy", WorkspaceRoot: b.root,
		PrivateTemp: b.root, ManagedProxyPort: b.proxyPort,
		EnvironmentContract: b.environmentContract,
		EnvironmentValues:   recordingEnvironment(b.environmentValues),
	}
}

func TestEnsurePlatformToolchainPATHPrependsPlatformDirectories(t *testing.T) {
	bin := t.TempDir()
	tool := filepath.Join(bin, "qcode-probe-tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin:/bin")
	env := ensurePlatformToolchainPATH([]string{"PATH=" + bin, "LANG=C"})
	path := environmentValue(env, "PATH")
	canonical, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, canonical+string(os.PathListSeparator)) {
		t.Fatalf("PATH=%q, want platform directory %q prepended", path, canonical)
	}
	// The prepended PATH must resolve arbitrary platform tools, not only
	// one hardcoded language toolchain.
	cmd := exec.Command("sh", "-c", "command -v qcode-probe-tool")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("probe tool via injected PATH: %v\n%s", err, out)
	} else if strings.TrimSpace(string(out)) != tool &&
		strings.TrimSpace(string(out)) != canonical+"/qcode-probe-tool" {
		t.Fatalf("resolved %q, want %q", out, tool)
	}
}

func TestPreparedToolchainExposurePrependsPathAndEnv(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	env := prependPATH([]string{"PATH=/usr/bin:/bin", "LANG=C"}, first, second)
	env = setEnvironmentValue(env, "TOOLCHAIN_HOME", "/host/toolchain")
	path := environmentValue(env, "PATH")
	wantPrefix := strings.Join(
		[]string{first, second, "/usr/bin", "/bin"},
		string(os.PathListSeparator),
	)
	if path != wantPrefix {
		t.Fatalf("PATH = %q, want %q", path, wantPrefix)
	}
	if got := environmentValue(env, "TOOLCHAIN_HOME"); got != "/host/toolchain" {
		t.Fatalf("TOOLCHAIN_HOME = %q", got)
	}
}

func recordingEnvironment(values []string) []string {
	if environmentValue(values, "PATH") != "" {
		return append([]string(nil), values...)
	}
	return append([]string{"PATH=/usr/bin:/bin"}, values...)
}

func testNetworkControls(network securitymodel.Network) securitymodel.Controls {
	controls := testControlMatrix()
	controls.Network = network
	return controls
}

// A command that prints for a while used to be invisible until it exited. The
// observer has to see output before the command finishes, not after.
func TestOutputArrivesBeforeTheCommandFinishes(t *testing.T) {
	var (
		mu     sync.Mutex
		chunks []Chunk
		early  = make(chan struct{})
		once   sync.Once
	)
	result, err := Run(t.Context(), Options{
		Command: `printf "first\n"; sleep 0.2; printf "second\n"`,
		Dir:     t.TempDir(),
		OnOutput: func(chunk Chunk) {
			mu.Lock()
			chunks = append(chunks, chunk)
			mu.Unlock()
			once.Do(func() { close(early) })
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-early:
	default:
		t.Fatal("no chunk was delivered while the command was running")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(chunks) < 2 {
		t.Fatalf("chunks = %+v, want the two prints delivered separately", chunks)
	}
	var streamed strings.Builder
	for _, chunk := range chunks {
		if chunk.Stream != StreamStdout {
			t.Fatalf("chunk stream = %q, want stdout", chunk.Stream)
		}
		streamed.Write(chunk.Data)
	}
	if streamed.String() != result.Stdout {
		t.Fatalf("streamed %q but result has %q", streamed.String(), result.Stdout)
	}
	// The cursor counts bytes of the stream, so a consumer can tell it missed some.
	if last := chunks[len(chunks)-1]; last.Cursor != uint64(len(result.Stdout)) {
		t.Fatalf("final cursor = %d, want %d", last.Cursor, len(result.Stdout))
	}
}

// stderr has to be distinguishable: "compiling" and "error:" belong in different
// places even when they interleave.
func TestChunksSayWhichStreamTheyCameFrom(t *testing.T) {
	var (
		mu       sync.Mutex
		byStream = map[Stream]string{}
	)
	if _, err := Run(t.Context(), Options{
		Command: `printf "out\n"; printf "err\n" 1>&2`,
		Dir:     t.TempDir(),
		OnOutput: func(chunk Chunk) {
			mu.Lock()
			byStream[chunk.Stream] += string(chunk.Data)
			mu.Unlock()
		},
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if byStream[StreamStdout] != "out\n" || byStream[StreamStderr] != "err\n" {
		t.Fatalf("streams = %+v", byStream)
	}
}

// Chunks are handed out as copies: exec reuses the read buffer, so an observer
// that keeps a chunk must not find it rewritten underneath.
func TestChunksSurviveLaterReads(t *testing.T) {
	var (
		mu   sync.Mutex
		kept [][]byte
	)
	if _, err := Run(t.Context(), Options{
		Command: `for index in 1 2 3 4 5; do printf "line-$index\n"; sleep 0.02; done`,
		Dir:     t.TempDir(),
		OnOutput: func(chunk Chunk) {
			mu.Lock()
			kept = append(kept, chunk.Data)
			mu.Unlock()
		},
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(kept) < 2 {
		t.Skipf("the shell batched its writes into %d chunk(s)", len(kept))
	}
	for index, data := range kept {
		if !strings.Contains(string(data), "line-") {
			t.Fatalf("chunk %d was overwritten: %q", index, data)
		}
	}
}

// An unobserved command must not pay for streaming, and must still report
// everything it printed.
func TestOutputIsCompleteWithoutAnObserver(t *testing.T) {
	result, err := Run(t.Context(), Options{
		Command: `printf "one\ntwo\n"`, Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "one\ntwo\n" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
}
