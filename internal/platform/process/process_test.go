//go:build !windows

package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/observability/tracecontext"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

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

func TestSanitizedEnvironmentRejectsExplicitSecretsAndUnknownNames(t *testing.T) {
	for _, extra := range [][]string{
		{"API_TOKEN=value"},
		{"CUSTOM_UNREVIEWED=value"},
		{"malformed"},
	} {
		if _, err := SanitizedEnvironment(extra); err == nil {
			t.Fatalf("SanitizedEnvironment(%q) succeeded", extra)
		}
	}
	environment, err := SanitizedEnvironment([]string{"LANG=C"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(environment, "LANG=C") {
		t.Fatalf("environment = %q", environment)
	}
}

func TestSanitizedEnvironmentPreservesGoModulePolicy(t *testing.T) {
	values := map[string]string{
		"GOPROXY":   "https://proxy.internal.example|direct",
		"GOPRIVATE": "code.internal.example",
		"GONOPROXY": "code.internal.example",
		"GOSUMDB":   "sum.golang.org",
		"GONOSUMDB": "code.internal.example",
		"GOVCS":     "public:git|hg,private:all",
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
	environment, err := SanitizedEnvironment(nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range values {
		entry := name + "=" + value
		if !slices.Contains(environment, entry) {
			t.Fatalf("environment omitted %q: %q", entry, environment)
		}
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

func TestSandboxEnvironmentForcesPrivateTempVariables(t *testing.T) {
	privateTemp := filepath.Join(t.TempDir(), "private")
	environment := sandboxEnvironment([]string{
		"HOME=/host/home",
		"TMPDIR=/host/tmpdir",
		"TMP=/host/tmp",
		"TEMP=/host/temp",
		"LANG=C",
	}, sandbox.Policy{PrivateTemp: privateTemp}, true)

	for _, name := range []string{"HOME", "TMPDIR", "TMP", "TEMP"} {
		if got := environmentValue(environment, name); got != privateTemp {
			t.Fatalf("%s=%q, want private temp %q", name, got, privateTemp)
		}
	}
	if got := environmentValue(environment, "LANG"); got != "C" {
		t.Fatalf("LANG=%q, want C", got)
	}
}

func TestRunPinsWorkingDirectoryToDescriptor(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	directory := filepath.Join(root, "dir")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "marker"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "marker"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	directoryFile, err := workspace.OpenDirectory("dir")
	if err != nil {
		t.Fatal(err)
	}
	defer directoryFile.Close()
	if err := os.Rename(directory, filepath.Join(root, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, directory); err != nil {
		t.Fatal(err)
	}

	backend := &recordingBackend{root: root}
	result, err := Run(t.Context(), Options{
		Command: "cat marker", Dir: directory, DirFile: directoryFile,
		Sandbox: backend, RequireSandbox: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "inside" || result.ExitCode != 0 {
		t.Fatalf("descriptor cwd result = %+v", result)
	}
	if backend.command.DirectoryFD != 3 {
		t.Fatalf("prepared directory fd = %d, want 3", backend.command.DirectoryFD)
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
		environmentValue(backend.command.Env, "PYTHONDONTWRITEBYTECODE") != "1" {
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
		Digest: strings.Repeat("a", 64), Enforcement: "strong",
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
		Digest: strings.Repeat("e", 64), Enforcement: "strong",
		WorkspaceRoot: root, AllowProcess: true,
		RequiredControls: controlmatrix.Requirements{
			Network: controlmatrix.NetworkDenied,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	weaker := testControlMatrix()
	weaker.Network = controlmatrix.NetworkDirect
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
		Digest: strings.Repeat("b", 64), Enforcement: "strong",
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
		Digest: strings.Repeat("c", 64), Enforcement: "strong",
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
		Digest: strings.Repeat("d", 64), Enforcement: "strong",
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
		Digest: strings.Repeat("e", 64), Enforcement: "strong",
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
		Digest: strings.Repeat("e", 64), Enforcement: "strong",
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
		Digest: strings.Repeat("e", 64), Enforcement: "strong",
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
		Digest: strings.Repeat("e", 64), Enforcement: "strong",
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
		Digest: strings.Repeat("f", 64), Enforcement: "strong",
		WorkspaceRoot: root, AllowNetwork: true, AllowProcess: true,
		ReadPaths: []string{root}, AllowLoopback: true,
		NetworkTargets: []string{"loopback://localhost:0"},
		RequiredControls: controlmatrix.Requirements{
			Network: controlmatrix.NetworkLoopbackExact,
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
		Digest: strings.Repeat("f", 64), Enforcement: "strong",
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
	command            sandbox.Command
	root               string
	proxyPort          uint16
	preparedControls   *controlmatrix.Matrix
	ignoreRestrictions bool
	ignoreWritePaths   bool
	ignoreAuthority    bool
}

func (b *recordingBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "fixture", Backend: "recording",
		Available: true,
		Effective: testControlMatrix(),
	}
}

func (b *recordingBackend) Prepare(_ context.Context, command sandbox.Command) (sandbox.Command, error) {
	b.command = command
	command.PreparedPolicyID = "fixture-policy"
	command.PreparedControls = sandbox.CommandControls(
		b.Capability(), b.Policy(), command,
	)
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
		command.PreparedProxyPort = sandbox.CommandNetworkPolicy(b.Policy(), command).ManagedProxyPort
		if !b.ignoreWritePaths {
			command.PreparedWritePaths = append(
				[]string(nil), command.WorkspaceWritePaths...,
			)
		}
	}
	return command, nil
}

func testControlMatrix() controlmatrix.Matrix {
	return controlmatrix.Matrix{
		FilesystemRead:  controlmatrix.FilesystemReadDeclaredRoots,
		FilesystemWrite: controlmatrix.FilesystemWriteExactPaths,
		Network:         controlmatrix.NetworkDenied,
		ProcessTree:     controlmatrix.ProcessTreeGroupKill,
		CrossProcess:    controlmatrix.CrossProcessRestricted,
		Syscall:         controlmatrix.SyscallDenyDangerous,
		IPC:             controlmatrix.IPCUnixOnly,
		PathIdentity:    controlmatrix.PathIdentityDescriptorRelative,
		ArtifactOrigin:  controlmatrix.ArtifactOriginVerifiedManifest,
		DurableRecovery: controlmatrix.DurableRecoveryExternalJournal,
	}
}

func (b *recordingBackend) Policy() sandbox.Policy {
	return sandbox.Policy{
		Version: 1, ID: "fixture-policy", WorkspaceRoot: b.root,
		PrivateTemp: b.root, ManagedProxyPort: b.proxyPort,
	}
}

func TestEnsureGoToolchainPrependsGOROOTBin(t *testing.T) {
	root := runtime.GOROOT()
	if root == "" {
		t.Skip("no GOROOT")
	}
	bin := filepath.Join(root, "bin")
	if _, err := os.Stat(filepath.Join(bin, "go")); err != nil {
		t.Skip("GOROOT/bin/go missing")
	}
	env := ensureGoToolchain([]string{"PATH=/usr/bin:/bin", "LANG=C"})
	path := environmentValue(env, "PATH")
	if !strings.HasPrefix(path, bin+string(os.PathListSeparator)) &&
		!strings.Contains(path, bin) {
		t.Fatalf("PATH=%q, want GOROOT/bin prepended", path)
	}
	if environmentValue(env, "GOROOT") == "" {
		t.Fatal("expected GOROOT to be set")
	}
	// Minimal PATH + injected GOROOT/bin must resolve go.
	cmd := exec.Command("sh", "-c", "command -v go && go env GOROOT")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go via injected PATH: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), root) {
		t.Fatalf("output=%q", out)
	}
}

func TestEnsureToolchainsAppliesGenericExposure(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	env := ensureToolchains(
		[]string{"PATH=/usr/bin:/bin", "LANG=C"},
		sandbox.ToolchainExposure{
			BinDirs:     []string{first, second},
			Environment: []string{"TOOLCHAIN_HOME=/host/toolchain"},
		},
	)
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

func TestShellRestoresSelectedGitToolchainAfterLoginProfile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS login shell behavior")
	}
	const git = "/Library/Developer/CommandLineTools/usr/bin/git"
	if _, err := os.Stat(git); err != nil {
		t.Skip("Command Line Tools Git is unavailable")
	}
	result, err := Run(t.Context(), Options{
		Command: `printf '%s|%s\n' "$1" "$2"; command -v git`,
		Dir:     t.TempDir(),
		Env:     []string{"PATH=/usr/bin:/bin", "LANG=C"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 ||
		!strings.HasPrefix(result.Stdout, "|\n") ||
		!strings.Contains(result.Stdout, filepath.Dir(git)+"/git") {
		t.Fatalf("result = %+v", result)
	}
}
