package wire

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/config"
	envcontract "github.com/fwtllh-png/QCode/internal/environment"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestDefaultEnvironmentIsV1NativeWithoutSharedTempOrAuth(t *testing.T) {
	defaults := config.Defaults().Execution.Environment
	if defaults.Contract != config.EnvironmentContractV1 ||
		defaults.Profile != config.EnvironmentProfileNative ||
		defaults.SharedUserTemp ||
		len(defaults.AuthServices) != 0 {
		t.Fatalf("product default = %+v", defaults)
	}
	if envcontract.ChildProfile(defaults.Profile) != envcontract.ProfileIsolated {
		t.Fatal("child profile must stay isolated after the default switch")
	}
}

func TestDefaultNativeDoesNotOpenHostHomeRoot(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		t.Skip("host home is required")
	}
	defaults := config.Defaults().Execution.Environment
	sandboxHome := t.TempDir()
	options, _, err := bindEnvironmentSandbox(sandbox.Options{
		WorkspaceRoot:       t.TempDir(),
		PrivateTemp:         sandboxHome,
		EnvironmentContract: defaults.Contract,
		EnvironmentProfile:  defaults.Profile,
		SharedUserTemp:      defaults.SharedUserTemp,
	}, defaults, "", sandboxHome)
	if err != nil {
		t.Fatal(err)
	}
	if options.SharedUserTemp {
		t.Fatal("default native opened shared_user_temp")
	}
	if got := environmentEntryValue(options.EnvironmentValues, "HOME"); got != "" && got != home {
		t.Fatalf("default native HOME = %q want host %q", got, home)
	}
	if environmentEntryValue(options.EnvironmentValues, "TMPDIR") != sandboxHome {
		t.Fatalf("default native temp leaked off sandbox-home: %v", options.EnvironmentValues)
	}
	for _, root := range options.HostReadRoots {
		if filepath.Clean(root) == filepath.Clean(home) {
			t.Fatalf("default native opened host home root %q", root)
		}
	}
	for _, root := range options.HostWriteRoots {
		if filepath.Clean(root) == filepath.Clean(home) {
			t.Fatalf("default native granted host home write %q", root)
		}
	}
	pathValue := environmentEntryValue(options.EnvironmentValues, "PATH")
	if homebrew := "/opt/homebrew/bin"; dirExists(homebrew) {
		canonical, err := filepath.EvalSymlinks(homebrew)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(pathValue, canonical) {
			t.Fatalf("default native PATH omitted Homebrew: %q", pathValue)
		}
		found := false
		for _, root := range options.HostReadRoots {
			if filepath.Clean(root) == filepath.Clean(canonical) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("default native omitted Homebrew read root %q roots=%v", canonical, options.HostReadRoots)
		}
	}
	if gitconfig := filepath.Join(home, ".gitconfig"); fileExists(gitconfig) {
		found := false
		for _, path := range options.HostReadFiles {
			if filepath.Clean(path) == filepath.Clean(gitconfig) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("default native omitted host gitconfig %q files=%v", gitconfig, options.HostReadFiles)
		}
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func TestDefaultChildStaysIsolatedWhenParentIsNative(t *testing.T) {
	defaults := config.Defaults().Execution.Environment
	childHome := t.TempDir()
	options, _, err := bindEnvironmentSandbox(sandbox.Options{
		WorkspaceRoot:       t.TempDir(),
		PrivateTemp:         childHome,
		EnvironmentContract: defaults.Contract,
		EnvironmentProfile:  envcontract.ChildProfile(defaults.Profile),
		SharedUserTemp:      false,
	}, defaults, "", childHome)
	if err != nil {
		t.Fatal(err)
	}
	if options.EnvironmentProfile != envcontract.ProfileIsolated || options.SharedUserTemp {
		t.Fatalf("child options = %+v", options)
	}
	if environmentEntryValue(options.EnvironmentValues, "HOME") != childHome {
		t.Fatalf("child HOME = %q want %q env=%v",
			environmentEntryValue(options.EnvironmentValues, "HOME"),
			childHome, options.EnvironmentValues)
	}
}

func TestDefaultContractCompilesSpecifiedPackageAgainstHost(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go executable is required")
	}
	defaults := config.Defaults().Execution.Environment
	workspace := writeP6SampleModule(t)
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	sandboxHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	options, _, err := bindEnvironmentSandbox(sandbox.Options{
		WorkspaceRoot:       workspace,
		PrivateTemp:         sandboxHome,
		EnvironmentContract: defaults.Contract,
		EnvironmentProfile:  defaults.Profile,
		SharedUserTemp:      defaults.SharedUserTemp,
	}, defaults, "", sandboxHome)
	if err != nil {
		t.Fatal(err)
	}

	host := exec.Command("go", "test", "-count=1", ".")
	host.Dir = workspace
	host.Env = append(os.Environ(), "GOPROXY=off", "GOTOOLCHAIN=local")
	hostOut, hostErr := host.CombinedOutput()
	if hostErr != nil {
		t.Fatalf("host go test failed: %v\n%s", hostErr, hostOut)
	}
	if !strings.Contains(string(hostOut), "ok") &&
		!strings.Contains(string(hostOut), "PASS") {
		t.Fatalf("host go test omitted ok: %s", hostOut)
	}

	backend, err := newPlatformBackend(options)
	if err != nil {
		t.Skipf("platform sandbox unavailable: %v", err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skipf("platform sandbox unavailable: %v", err)
	}
	directory, err := os.Open(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })
	result, err := process.Run(t.Context(), process.Options{
		Command:        "go test -count=1 .",
		Dir:            workspace,
		DirFile:        directory,
		Env:            []string{"GOPROXY=off", "GOTOOLCHAIN=local"},
		Sandbox:        backend,
		RequireSandbox: true,
		DenyNetwork:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 ||
		(!strings.Contains(result.Stdout+result.Stderr, "ok") &&
			!strings.Contains(result.Stdout+result.Stderr, "PASS")) {
		t.Fatalf(
			"sandboxed go test drifted from host: exit=%d stdout=%q stderr=%q host=%s",
			result.ExitCode, result.Stdout, result.Stderr, hostOut,
		)
	}
}

func writeP6SampleModule(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod": `module qcode.local/p6sample

go 1.22
`,
		"sample.go": `package sample

func Answer() int { return 42 }
`,
		"sample_test.go": `package sample

import "testing"

func TestAnswer(t *testing.T) {
	if Answer() != 42 {
		t.Fatalf("answer = %d", Answer())
	}
}
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
