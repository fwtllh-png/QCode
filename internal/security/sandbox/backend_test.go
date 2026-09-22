package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
)

func TestProbeReportsExplicitPlatformBackendAndControls(t *testing.T) {
	capability := Probe()
	if capability.Platform != runtime.GOOS {
		t.Fatalf("platform = %q, want %q", capability.Platform, runtime.GOOS)
	}
	if capability.Backend == "" {
		t.Fatalf("capability = %+v", capability)
	}
	if capability.Available {
		if err := capability.Effective.Validate(); err != nil {
			t.Fatalf("available backend controls: %v", err)
		}
	}
}

func TestPolicyRejectsBroadAndSensitiveReadRoots(t *testing.T) {
	root := t.TempDir()
	for _, injected := range []string{"/", filepath.Dir(root)} {
		if _, err := BuildPolicy(Options{
			WorkspaceRoot: root, PrivateTemp: t.TempDir(), HostReadRoots: []string{injected},
		}); err == nil {
			t.Fatalf("host read root %q was accepted", injected)
		}
	}
	home, err := os.UserHomeDir()
	if err == nil {
		if _, err := BuildPolicy(Options{
			WorkspaceRoot: root, PrivateTemp: t.TempDir(), HostReadRoots: []string{home},
		}); err == nil {
			t.Fatal("user home was accepted as a host read root")
		}
	}
}

func TestExactWorkspaceWritePathLimitMatchesToolExpansion(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, MaxExactWorkspaceWritePaths+1)
	for index := range MaxExactWorkspaceWritePaths + 1 {
		path := fmt.Sprintf("file-%03d.txt", index)
		if err := os.WriteFile(filepath.Join(root, path), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	if _, err := validateExactWorkspaceWritePaths(
		workspace,
		true,
		paths[:MaxExactWorkspaceWritePaths],
	); err != nil {
		t.Fatalf("bounded exact writes were rejected: %v", err)
	}
	if _, err := validateExactWorkspaceWritePaths(
		workspace,
		true,
		paths,
	); err == nil {
		t.Fatal("write set beyond the limit was accepted")
	}
}

func TestExactWorkspaceWritePathsAllowMissingLeafWithExistingParent(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "generated"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("generated", "new.txt")
	resolved, err := validateExactWorkspaceWritePaths(
		workspace,
		true,
		[]string{path},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 ||
		resolved[0].path != filepath.Join(workspace.Root(), path) ||
		resolved[0].kind != writePathFile {
		t.Fatalf("resolved paths = %+v", resolved)
	}
	if trees, err := validateExactWorkspaceWritePaths(
		workspace,
		true,
		[]string{"generated"},
	); err != nil || len(trees) != 1 || trees[0].kind != writePathTree {
		t.Fatalf("existing write tree = %+v err=%v", trees, err)
	}
	if _, err := validateExactWorkspaceWritePaths(
		workspace,
		true,
		[]string{filepath.Join("missing", "new.txt")},
	); err == nil {
		t.Fatal("missing parent was accepted")
	}
}

func TestWorkspaceWriteTreesRejectRootAndProtectedPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateExactWorkspaceWritePaths(workspace, true, []string{"."}); err == nil {
		t.Fatal("workspace root write tree was accepted")
	}
	if _, err := validateExactWorkspaceWritePaths(workspace, true, []string{".git"}); err == nil {
		t.Fatal("protected write tree was accepted")
	}
}

func TestMaterializeMissingExactWritePathsCreatesOnlyDeclaredFiles(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace.Root(), "new.txt")
	pinned := []workspaceWritePath{{path: path, kind: writePathFile}}
	if err := materializeMissingExactWritePaths(workspace, pinned); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		t.Fatalf("materialized path info=%+v error=%v", info, err)
	}
	if err := materializeMissingExactWritePaths(workspace, pinned); err != nil {
		t.Fatalf("idempotent materialization failed: %v", err)
	}
	link := filepath.Join(workspace.Root(), "replaced.txt")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := materializeMissingExactWritePaths(
		workspace,
		[]workspaceWritePath{{path: link, kind: writePathFile}},
	); err == nil || !strings.Contains(err.Error(), "changed type") {
		t.Fatalf("symlink replacement error = %v", err)
	}
}

func TestExactWorkspaceWritesRejectControlPlaneAndWritableBase(t *testing.T) {
	root := t.TempDir()
	protected := filepath.Join(root, ".qcode", "state.json")
	if err := os.MkdirAll(filepath.Dir(protected), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(protected, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateExactWorkspaceWritePaths(
		workspace,
		true,
		[]string{".qcode/state.json"},
	); err == nil || !strings.Contains(err.Error(), "control-plane") {
		t.Fatalf("protected exact write error = %v", err)
	}
	ordinary := filepath.Join(root, "ordinary.txt")
	if err := os.WriteFile(ordinary, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateExactWorkspaceWritePaths(
		workspace,
		false,
		[]string{ordinary},
	); err == nil || !strings.Contains(err.Error(), "read-only workspace") {
		t.Fatalf("exact write on writable base error = %v", err)
	}
}

func TestBackendProfilesNeverAdmitHostRoot(t *testing.T) {
	root := t.TempDir()
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: root, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := seatbeltProfile(policy, "/bin/sh")
	if strings.Contains(profile, "(allow file-read*)") ||
		strings.Contains(profile, `(subpath "/")`) {
		t.Fatalf("seatbelt profile contains an unscoped root read:\n%s", profile)
	}
	if !strings.Contains(profile, "(allow file-read-metadata (literal") {
		t.Fatalf("seatbelt profile missing ancestor metadata grants:\n%s", profile)
	}
	importIndex := strings.Index(profile, `(import "system.sb")`)
	networkDenyIndex := strings.LastIndex(profile, "(deny network*)")
	if importIndex < 0 || networkDenyIndex < importIndex {
		t.Fatalf("Seatbelt profile does not override imported network rules:\n%s", profile)
	}
	for _, sensitive := range []string{".ssh", ".gnupg", "Keychains", ".aws"} {
		if !strings.Contains(profile, sensitive) {
			t.Fatalf("Seatbelt profile does not explicitly deny %s", sensitive)
		}
	}
	if runtime.GOOS == "darwin" {
		if err := seatbeltSystemProfileAudit.run(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPolicyInheritsPATHHostReadRoots(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "tools")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin")
	policy, err := BuildPolicy(Options{WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(bin)
	if err != nil {
		canonical = bin
	}
	if !slices.Contains(policy.HostReadRoots, filepath.Clean(canonical)) {
		t.Fatalf("PATH dir %q missing from HostReadRoots=%v", canonical, policy.HostReadRoots)
	}
}

func TestSeatbeltAllowsNetworkWhenConfigured(t *testing.T) {
	root := t.TempDir()
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: root, PrivateTemp: t.TempDir(),
		AllowNetwork: true, SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := seatbeltProfile(policy, "/bin/sh")
	if strings.Contains(profile, "(deny network*)") {
		t.Fatalf("network still denied:\n%s", profile)
	}
	if !strings.Contains(profile, "(allow network-outbound)") {
		t.Fatalf("network outbound missing:\n%s", profile)
	}
}

func TestSeatbeltHostWriteRootUsesSubpath(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: root, PrivateTemp: t.TempDir(),
		HostWriteRoots: []string{extra}, SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := seatbeltProfileForCommand(
		policy, "/bin/sh", true, nil, nil, nil, true, false,
	)
	found := false
	for _, writeRoot := range policy.HostWriteRoots {
		rule := "(allow file-write* (subpath " + seatbeltQuote(writeRoot) + "))"
		if strings.Contains(profile, rule) {
			found = true
		}
	}
	if !found {
		t.Fatalf("host write root subpath missing:\n%s\nroots=%v", profile, policy.HostWriteRoots)
	}
}

func TestBuildPolicyAcceptsExactGitconfigUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(path, []byte("[user]\n\tname = Fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(),
		HostReadFiles: []string{path}, SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(policy.HostReadFiles, path) {
		t.Fatalf("host read files = %v, want %s", policy.HostReadFiles, path)
	}
}

func TestBuildPolicyRejectsHomeCredentialFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".git-credentials", filepath.Join(".ssh", "config")} {
		path := filepath.Join(home, name)
		if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := BuildPolicy(Options{
			WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(),
			HostReadFiles: []string{path}, SkipPATHReadRoots: true,
		}); err == nil {
			t.Fatalf("accepted credential file %s", path)
		}
	}
}

func TestBuildPolicyRejectsHomeWriteRoot(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory")
	}
	if _, err := BuildPolicy(Options{
		WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(),
		HostWriteRoots: []string{home}, SkipPATHReadRoots: true,
	}); err == nil {
		t.Fatal("home write root was accepted")
	}
}

func TestBuildPolicyRecordsDeclaredEnvironmentNetworkAndValues(t *testing.T) {
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(),
		EnvironmentContract: "v1", SkipPATHReadRoots: true,
		EnvironmentNetwork: []EnvironmentNetworkTarget{{
			Host: "declared.example", Protocol: "https", Port: 443,
			Methods: []string{"CONNECT"},
		}},
		EnvironmentValues: []string{"GOROOT=/opt/go", "HOME=/sandbox-home"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.EnvironmentNetwork) != 1 ||
		policy.EnvironmentNetwork[0].Host != "declared.example" {
		t.Fatalf("environment network = %+v", policy.EnvironmentNetwork)
	}
	if !slices.Contains(policy.EnvironmentValues, "GOROOT=/opt/go") ||
		!slices.Contains(policy.EnvironmentValues, "HOME=/sandbox-home") {
		t.Fatalf("environment values = %v", policy.EnvironmentValues)
	}
}

func TestBuildPolicyInheritsToolchainExposureWithoutPATHScan(t *testing.T) {
	workspace := t.TempDir()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	exposure := ToolchainExposure{
		BinDirs:     []string{bin},
		ReadRoots:   []string{bin},
		Environment: []string{"GOROOT=/opt/go"},
	}
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: workspace, PrivateTemp: t.TempDir(),
		SkipPATHReadRoots: true, Toolchains: &exposure,
		EnvironmentValues: []string{"GOMODCACHE=/cache/go-mod"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.Toolchains.BinDirs) != 1 || policy.Toolchains.BinDirs[0] != bin {
		t.Fatalf("toolchain bin dirs = %v, want [%s]", policy.Toolchains.BinDirs, bin)
	}
	if len(policy.Toolchains.Environment) != 1 ||
		policy.Toolchains.Environment[0] != "GOROOT=/opt/go" {
		t.Fatalf("toolchain environment = %v", policy.Toolchains.Environment)
	}
	if !slices.Contains(policy.HostReadRoots, bin) {
		t.Fatalf("host read roots = %v, want %s", policy.HostReadRoots, bin)
	}
	if !slices.Contains(policy.EnvironmentValues, "GOMODCACHE=/cache/go-mod") {
		t.Fatalf("environment values = %v", policy.EnvironmentValues)
	}
}

func TestBuildPolicyDropsVanishedInheritedToolchainRoot(t *testing.T) {
	vanished := filepath.Join(t.TempDir(), "gone")
	exposure := ToolchainExposure{BinDirs: []string{vanished}}
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(),
		SkipPATHReadRoots: true, Toolchains: &exposure,
	})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(policy.HostReadRoots, vanished) {
		t.Fatalf(
			"host read roots = %v, must not contain vanished %s",
			policy.HostReadRoots, vanished,
		)
	}
	if len(policy.Toolchains.BinDirs) != 1 || policy.Toolchains.BinDirs[0] != vanished {
		t.Fatalf("toolchain bin dirs = %v, exposure must stay intact", policy.Toolchains.BinDirs)
	}
}

func TestRefuseUndeliveredManagedNetworkReportsUnsupported(t *testing.T) {
	err := refuseUndeliveredManagedNetwork(
		Policy{EnvironmentNetwork: []EnvironmentNetworkTarget{{
			Host: "example.test", Port: 443,
		}}},
		Command{},
	)
	if err == nil || !strings.Contains(err.Error(), "backend_capability_unsupported") {
		t.Fatalf("error = %v", err)
	}
	if err := refuseUndeliveredManagedNetwork(
		Policy{EnvironmentNetwork: []EnvironmentNetworkTarget{{
			Host: "example.test", Port: 443,
		}}},
		Command{DenyNetwork: true},
	); err != nil {
		t.Fatalf("deny-network should skip: %v", err)
	}
	if SupportsManagedNetworkProxy() {
		if err := refuseUndeliveredManagedNetwork(
			Policy{}, Command{SessionProxyPort: 9},
		); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildPolicyRecordsV1ContractWithoutIndependentCertDiscovery(t *testing.T) {
	for _, name := range []string{
		"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
	} {
		t.Setenv(name, "")
	}
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(),
		EnvironmentContract: "v1", SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if policy.EnvironmentContract != "v1" || len(policy.Toolchains.ReadFiles) != 0 {
		t.Fatalf("v1 policy = %+v", policy)
	}
}

func TestDiscoverToolchainsV1SkipsNamedLanguageProbes(t *testing.T) {
	exposure := discoverToolchains(t.TempDir(), nil, nil)
	for _, entry := range exposure.Environment {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "GOROOT", "RUSTUP_HOME":
			t.Fatalf("v1 discovered named language env: %v", exposure.Environment)
		}
	}
}

func TestSeatbeltCommandCanRestrictWorkspaceAndNetwork(t *testing.T) {
	root := t.TempDir()
	privateTemp := t.TempDir()
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: root, PrivateTemp: privateTemp,
		AllowNetwork: true, SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := seatbeltProfileForCommand(
		policy, "/bin/sh", true, nil, nil, nil, true, false,
	)
	workspaceWrite := "(allow file-write* (subpath " + seatbeltQuote(policy.WorkspaceRoot) + "))"
	if strings.Contains(profile, workspaceWrite) {
		t.Fatalf("read-only profile permits workspace writes:\n%s", profile)
	}
	privateWrite := "(allow file-write* (subpath " + seatbeltQuote(policy.PrivateTemp) + "))"
	if !strings.Contains(profile, privateWrite) {
		t.Fatalf("read-only profile blocks private temp writes:\n%s", profile)
	}
	if !strings.Contains(profile, "(deny network*)") ||
		strings.Contains(profile, "(allow network-outbound)") {
		t.Fatalf("network-restricted profile permits network:\n%s", profile)
	}
}

func TestSeatbeltCommandHidesWorkspaceControlPaths(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	hidden := filepath.Join(workspace.Root(), ".git")
	if err := os.Mkdir(hidden, 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: root, PrivateTemp: t.TempDir(),
		SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := validateWorkspaceHiddenPaths(workspace, []string{hidden})
	if err != nil {
		t.Fatal(err)
	}
	profile := seatbeltProfileForCommand(
		policy, "/bin/sh", true, nil, nil, paths, true, false,
	)
	rule := "(deny file-read* file-write* (subpath " + seatbeltQuote(hidden) + "))"
	if !strings.Contains(profile, rule) {
		t.Fatalf("hidden control path rule missing:\n%s", profile)
	}
}

func TestWorkspaceHiddenPathsRejectEscape(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateWorkspaceHiddenPaths(
		workspace,
		[]string{t.TempDir()},
	); err == nil {
		t.Fatal("outside hidden path was accepted")
	}
}

func TestSeatbeltManagedNetworkAllowsOnlyProxyPort(t *testing.T) {
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(),
		ManagedProxyPort: 43128, SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := seatbeltProfileForCommand(
		policy, "/bin/sh", true, nil, nil, nil, false, false,
	)
	if !strings.Contains(
		profile,
		`(allow network-outbound (remote ip "localhost:43128"))`,
	) || strings.Contains(profile, "\n(allow network-outbound)\n") ||
		strings.Contains(profile, `remote ip "localhost:*"`) {
		t.Fatalf("managed network profile is broader than the proxy port:\n%s", profile)
	}
}

func TestSeatbeltManagedNetworkCanAddExplicitLoopback(t *testing.T) {
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(),
		ManagedProxyPort: 43128, SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := seatbeltProfileForCommand(
		policy, "/bin/sh", true, nil, nil, nil, false, true,
	)
	for _, rule := range []string{
		`(allow network-outbound (remote ip "localhost:43128"))`,
		`(allow network-inbound (local ip "localhost:*"))`,
		`(allow network-outbound (remote ip "localhost:*"))`,
	} {
		if !strings.Contains(profile, rule) {
			t.Fatalf("managed loopback profile missing %q:\n%s", rule, profile)
		}
	}
	if strings.Contains(profile, "\n(allow network-outbound)\n") {
		t.Fatalf("managed loopback profile permits direct external network:\n%s", profile)
	}
}

func TestSeatbeltCanAllowLoopbackWithoutManagedProxy(t *testing.T) {
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(),
		SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := seatbeltProfileForCommand(
		policy, "/bin/sh", true, nil, nil, nil, false, true,
	)
	if !strings.Contains(profile, `remote ip "localhost:*"`) ||
		strings.Contains(profile, "\n(allow network-outbound)\n") {
		t.Fatalf("loopback-only profile is not confined:\n%s", profile)
	}
}

func TestSeatbeltExplicitLoopbackSupportsLocalFixtureServer(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("seatbelt is only available on macOS")
	}
	backend, err := NewPlatformBackend(Options{
		WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(),
		ManagedProxyPort: 43128, SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer CloseBackend(backend)
	prepared, err := backend.Prepare(t.Context(), Command{
		Path: "/usr/bin/python3",
		Args: []string{"/usr/bin/python3", "-c",
			`import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); s.listen(); c=socket.create_connection(s.getsockname()); a,_=s.accept(); c.sendall(b"x"); assert a.recv(1)==b"x"`},
		Dir: backend.(PolicyBackend).Policy().WorkspaceRoot,
		Env: []string{"PATH=/usr/bin:/bin"}, WorkspaceReadOnly: true,
		AllowLoopback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), prepared.Path, prepared.Args[1:]...)
	command.Dir = prepared.Dir
	command.Env = prepared.Env
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("loopback fixture failed: %v\n%s", err, output)
	}
}

func TestSeatbeltPreparedProxyPortReflectsCommandNetworkPolicy(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("seatbelt is only available on macOS")
	}
	backend, err := NewPlatformBackend(Options{
		WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(),
		ManagedProxyPort: 43128, SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer CloseBackend(backend)
	for _, test := range []struct {
		name        string
		denyNetwork bool
		wantPort    uint16
	}{
		{name: "managed", wantPort: 43128},
		{name: "denied", denyNetwork: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, err := backend.Prepare(t.Context(), Command{
				Path: "/bin/sh", Args: []string{"/bin/sh", "-c", "true"},
				Dir:               backend.(PolicyBackend).Policy().WorkspaceRoot,
				Env:               []string{"PATH=/usr/bin:/bin"},
				WorkspaceReadOnly: true, DenyNetwork: test.denyNetwork,
			})
			if err != nil {
				t.Fatal(err)
			}
			if prepared.PreparedProxyPort != test.wantPort ||
				prepared.PreparedNetworkDenied != test.denyNetwork {
				t.Fatalf(
					"prepared port=%d denied=%t, want port=%d denied=%t",
					prepared.PreparedProxyPort,
					prepared.PreparedNetworkDenied,
					test.wantPort,
					test.denyNetwork,
				)
			}
		})
	}
}

func TestSeatbeltCommandAllowsOnlyDeclaredWorkspaceFiles(t *testing.T) {
	root := t.TempDir()
	privateTemp := t.TempDir()
	declared := filepath.Join(root, "declared.txt")
	undeclared := filepath.Join(root, "undeclared.txt")
	for _, path := range []string{declared, undeclared} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: root, PrivateTemp: privateTemp,
		SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := seatbeltProfileForCommand(
		policy, "/bin/sh", true, nil,
		[]workspaceWritePath{{path: declared, kind: writePathFile}}, nil, true, false,
	)
	declaredWrite := "(allow file-write* (literal " + seatbeltQuote(declared) + "))"
	if !strings.Contains(profile, declaredWrite) {
		t.Fatalf("declared file grant missing:\n%s", profile)
	}
	undeclaredWrite := "(allow file-write* (literal " + seatbeltQuote(undeclared) + "))"
	if strings.Contains(profile, undeclaredWrite) {
		t.Fatalf("undeclared file grant present:\n%s", profile)
	}
	workspaceWrite := "(allow file-write* (subpath " + seatbeltQuote(root) + "))"
	if strings.Contains(profile, workspaceWrite) {
		t.Fatalf("workspace-wide write grant present:\n%s", profile)
	}
}

func TestSeatbeltProfileAddsOnlyApprovedReadPath(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	privateTemp := filepath.Join(parent, "private")
	approved := filepath.Join(parent, "approved.txt")
	for _, directory := range []string{workspace, privateTemp} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(approved, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: workspace, PrivateTemp: privateTemp,
		SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := validateAdditionalReadPaths(policy, []string{approved})
	if err != nil {
		t.Fatal(err)
	}
	profile := seatbeltProfileForCommand(
		policy, "/bin/sh", true, paths, nil, nil, true, false,
	)
	rule := "(allow file-read* (literal " + seatbeltQuote(paths[0]) + "))"
	if !strings.Contains(profile, rule) {
		t.Fatalf("approved read grant missing:\n%s", profile)
	}
}

func TestSeatbeltShellHereDocumentGrantIsNarrow(t *testing.T) {
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(),
		SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	shellProfile := seatbeltProfile(policy, "/bin/sh")
	for _, rule := range []string{
		`(allow file-write* (literal "/var/tmp"))`,
		`(allow file-write* (regex #"^/var/tmp/sh-thd-[0-9]+$"))`,
		`(allow file-read* (regex #"^/var/tmp/sh-thd-[0-9]+$"))`,
		`(allow file-write* (literal "/private/var/tmp"))`,
		`(allow file-write* (regex #"^/private/var/tmp/sh-thd-[0-9]+$"))`,
		`(allow file-read-metadata (subpath "/private/var/tmp"))`,
		`(allow file-read* (regex #"^/private/var/tmp/sh-thd-[0-9]+$"))`,
		`(allow file-write* (literal "/private/tmp"))`,
		`(allow file-write* (regex #"^/private/tmp/sh-thd-[0-9]+$"))`,
		`(allow file-read* (regex #"^/private/tmp/sh-thd-[0-9]+$"))`,
	} {
		if !strings.Contains(shellProfile, rule) {
			t.Fatalf("shell profile missing narrow heredoc rule %q:\n%s", rule, shellProfile)
		}
	}
	for _, broadRule := range []string{
		`(allow file-write* (subpath "/var/tmp"))`,
		`(allow file-write* (subpath "/private/var/tmp"))`,
		`(allow file-write* (subpath "/private/tmp"))`,
		`(allow file-read* (subpath "/private/var/tmp"))`,
	} {
		if strings.Contains(shellProfile, broadRule) {
			t.Fatalf("shell profile broadly permits host temp writes:\n%s", shellProfile)
		}
	}
	nonShellProfile := seatbeltProfile(policy, "/usr/bin/python3")
	if strings.Contains(nonShellProfile, `sh-thd-`) ||
		strings.Contains(nonShellProfile, `(allow file-write* (literal "/private/tmp"))`) {
		t.Fatalf("non-shell profile inherited heredoc permissions:\n%s", nonShellProfile)
	}
}

func TestBackendsPreserveDescriptorRelativeWorkingDirectory(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	input := Command{
		Path: "sh", Args: []string{"sh", "-lc", "pwd"},
		Dir: workspace.Root(), DirectoryFD: 3,
		Env:               []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"},
		WorkspaceReadOnly: true,
		AllowLoopback:     true,
	}
	policy, err := BuildPolicy(Options{WorkspaceRoot: workspace.Root(), PrivateTemp: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" {
		seatbelt, err := (&seatbeltBackend{
			workspace: workspace, policy: policy,
			capability: Capability{},
		}).Prepare(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		if seatbelt.DirectoryFD != 3 || !seatbelt.PreparedLoopbackAllowed {
			t.Fatalf("seatbelt command = %+v", seatbelt)
		}
	}
}

func TestRequireStrongFailsClosedForMissingAndPartialBackends(t *testing.T) {
	if err := RequireControls(
		nil,
		DefaultProcessRequirements(),
	); !IsUnavailable(err) {
		t.Fatalf("nil backend error = %v", err)
	}
	backend := &unavailableBackend{capability: Capability{
		Platform: "fixture", Backend: "partial",
		Available: true,
	}}
	if err := RequireControls(
		backend,
		DefaultProcessRequirements(),
	); !IsUnavailable(err) {
		t.Fatalf("partial backend error = %v", err)
	}
}

func TestRequiredControlsUseEffectiveMatrix(t *testing.T) {
	claimedStrong := &unavailableBackend{capability: Capability{
		Platform: "fixture", Backend: "claimed-strong",
		Available: true,
		Effective: controlmatrix.Matrix{
			FilesystemRead:  controlmatrix.FilesystemReadUnrestricted,
			FilesystemWrite: controlmatrix.FilesystemWriteUnrestricted,
			Network:         controlmatrix.NetworkDirect,
			ProcessTree:     controlmatrix.ProcessTreeUnmanaged,
			CrossProcess:    controlmatrix.CrossProcessUnrestricted,
			Syscall:         controlmatrix.SyscallUnrestricted,
			IPC:             controlmatrix.IPCUnrestricted,
			PathIdentity:    controlmatrix.PathIdentityLexical,
			ArtifactOrigin:  controlmatrix.ArtifactOriginUnverifiedPath,
			DurableRecovery: controlmatrix.DurableRecoveryMemoryOnly,
		},
	}}
	if err := RequireControls(
		claimedStrong,
		DefaultProcessRequirements(),
	); !IsUnavailable(err) {
		t.Fatalf("claimed strong backend error = %v", err)
	}
	partial := claimedStrong.capability
	partial.Effective.FilesystemRead = controlmatrix.FilesystemReadDeclaredRoots
	partial.Effective.Network = controlmatrix.NetworkDenied
	if err := RequireControls(
		&unavailableBackend{capability: partial},
		controlmatrix.Requirements{
			FilesystemRead: controlmatrix.FilesystemReadDeclaredRoots,
			Network:        controlmatrix.NetworkDenied,
		},
	); err != nil {
		t.Fatalf("partial backend with sufficient controls was rejected: %v", err)
	}
}

func TestPolicyCannotInventUnavailableControls(t *testing.T) {
	capability := Capability{
		Platform: "fixture", Backend: "weak", Available: true,
		Effective: controlmatrix.Matrix{
			FilesystemRead:  controlmatrix.FilesystemReadUnrestricted,
			FilesystemWrite: controlmatrix.FilesystemWriteUnrestricted,
			Network:         controlmatrix.NetworkDirect,
			ProcessTree:     controlmatrix.ProcessTreeUnmanaged,
			CrossProcess:    controlmatrix.CrossProcessUnrestricted,
			Syscall:         controlmatrix.SyscallUnrestricted,
			IPC:             controlmatrix.IPCUnrestricted,
			PathIdentity:    controlmatrix.PathIdentityLexical,
			ArtifactOrigin:  controlmatrix.ArtifactOriginUnverifiedPath,
			DurableRecovery: controlmatrix.DurableRecoveryMemoryOnly,
		},
	}
	policy := Policy{}
	effective := EffectiveControls(capability, policy)
	if effective.Network != controlmatrix.NetworkDirect {
		t.Fatalf("policy invented network isolation: %+v", effective)
	}
	prepared := CommandControls(capability, policy, Command{
		WorkspaceReadOnly: true,
		DenyNetwork:       true,
	})
	if prepared.Network != controlmatrix.NetworkDirect ||
		prepared.FilesystemWrite != controlmatrix.FilesystemWriteUnrestricted {
		t.Fatalf("command invented unavailable controls: %+v", prepared)
	}
}

func TestDarwinRuntimeRootsIncludeDeveloperTools(t *testing.T) {
	roots := platformRuntimeRoots("darwin")
	for _, want := range []string{
		"/Library/Developer/CommandLineTools",
		"/Applications/Xcode.app/Contents/Developer",
	} {
		if !slices.Contains(roots, want) {
			t.Fatalf("darwin runtime roots missing %s: %v", want, roots)
		}
	}
	if runtime.GOOS != "darwin" {
		return
	}
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(), SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := seatbeltProfile(policy, "/usr/bin/git")
	if !strings.Contains(profile, "CommandLineTools") {
		t.Fatalf("seatbelt profile missing CommandLineTools read:\n%s", profile)
	}
}

func TestValidateWorkspaceLinksAllowsLinksContainedInWorkspace(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "node_modules", "esbuild", "bin", "esbuild")
	second := filepath.Join(
		root,
		"node_modules",
		"@esbuild",
		"darwin-arm64",
		"bin",
		"esbuild",
	)
	if err := os.MkdirAll(filepath.Dir(first), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(second), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Skipf("hard links are unavailable: %v", err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateWorkspaceLinks(t.Context(), workspace); err != nil {
		t.Fatalf("validateWorkspaceLinks() rejected contained hard links: %v", err)
	}
}

func TestValidateWorkspaceLinksAllowsDanglingSymlinkWithinWorkspace(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "node_modules", "package")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../packages/missing", link); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateWorkspaceLinks(t.Context(), workspace); err != nil {
		t.Fatalf("validateWorkspaceLinks() rejected contained dangling link: %v", err)
	}
}

func TestValidateWorkspaceLinksRejectsDanglingSymlinkOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "package")
	if err := os.Symlink(filepath.Join(outside, "missing"), link); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	err = validateWorkspaceLinks(t.Context(), workspace)
	if err == nil || !strings.Contains(err.Error(), "escapes the sandbox") {
		t.Fatalf("validateWorkspaceLinks() error = %v", err)
	}
}

func TestValidateWorkspaceLinksRejectsLinkOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	insidePath := filepath.Join(root, "linked")
	outsidePath := filepath.Join(outside, "outside")
	if err := os.WriteFile(outsidePath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outsidePath, insidePath); err != nil {
		t.Skipf("hard links are unavailable: %v", err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	err = validateWorkspaceLinks(t.Context(), workspace)
	if err == nil || !strings.Contains(err.Error(), "hard links outside the workspace") {
		t.Fatalf("validateWorkspaceLinks() error = %v", err)
	}
}

func TestValidateWorkspaceLinksHonorsCancellation(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := validateWorkspaceLinks(ctx, workspace); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled validation error = %v", err)
	}
}

func TestSystemProfileAuditCachesByStat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "system.sb")
	if err := os.WriteFile(
		path, []byte("(define (seatbelt-ext))"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	audit := &systemProfileAudit{path: path}
	if err := audit.run(); err != nil {
		t.Fatal(err)
	}

	// A cached verdict short-circuits without reopening the file.
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	if err := audit.run(); err != nil {
		t.Fatalf("cached audit reopened the profile: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	// A content change invalidates the cached verdict.
	if err := os.WriteFile(
		path, []byte("(allow file-read*)"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := audit.run(); err == nil {
		t.Fatal("stale audit verdict survived a content change")
	}

	// Failed audits are never cached.
	if err := os.WriteFile(
		path, []byte("(define (seatbelt-ext))"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := audit.run(); err != nil {
		t.Fatalf("audit did not recover after content was fixed: %v", err)
	}
}

func TestPolicyExecutableReadableModelsReadRoots(t *testing.T) {
	workspace := t.TempDir()
	host := t.TempDir()
	policy, err := BuildPolicy(Options{
		WorkspaceRoot: workspace, PrivateTemp: t.TempDir(),
		HostReadRoots: []string{host}, SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(workspace, "bin", "tool")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	covered := filepath.Join(t.TempDir(), "covered-tool")
	if err := os.WriteFile(covered, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !policy.ExecutableReadable(inside, nil) {
		t.Fatal("workspace executable reported unreadable")
	}
	if !policy.ExecutableReadable(filepath.Join(host, "git"), nil) {
		t.Fatal("host read root executable reported unreadable")
	}
	if policy.ExecutableReadable(outside, nil) {
		t.Fatal("outside executable reported readable")
	}
	if !policy.ExecutableReadable(covered, []string{filepath.Dir(covered)}) {
		t.Fatal("additional read path executable reported unreadable")
	}
	if policy.ExecutableReadable(covered, nil) {
		t.Fatal("additional read paths widened the model without declaration")
	}
}

func TestRefuseUndeliveredManagedNetworkAcceptsWorkspaceChannel(t *testing.T) {
	// The stable workspace channel delivers the declared environment
	// network without a per-command session.
	if err := refuseUndeliveredManagedNetwork(Policy{
		ManagedProxyPort: 9,
		EnvironmentNetwork: []EnvironmentNetworkTarget{{
			Host: "goproxy.example", Port: 443,
		}},
	}, Command{}); err != nil {
		t.Fatalf("workspace channel must satisfy delivery: %v", err)
	}
	if err := refuseUndeliveredManagedNetwork(Policy{
		EnvironmentNetwork: []EnvironmentNetworkTarget{{
			Host: "goproxy.example", Port: 443,
		}},
	}, Command{}); err == nil {
		t.Fatal("delivery without a managed channel must fail closed")
	}
}

func TestWritePathTypeSwapFailsClosed(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "generated"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	// Validation approves a missing leaf as a FILE.
	pinned, err := validateExactWorkspaceWritePaths(
		workspace, true, []string{filepath.Join("generated", "new.txt")},
	)
	if err != nil || len(pinned) != 1 || pinned[0].kind != writePathFile {
		t.Fatalf("pinned = %+v err = %v", pinned, err)
	}
	// The profile must render the pinned literal grant even though the path
	// is now a directory: no fresh Stat may widen the grant. pinned paths are
	// workspace-canonical (/private/var on macOS), so derive from pinned.
	swapped := pinned[0].path
	if err := os.Mkdir(swapped, 0o700); err != nil {
		t.Fatal(err)
	}
	profile := seatbeltProfileForCommand(
		Policy{WorkspaceRoot: workspace.Root(), PrivateTemp: filepath.Join(root, "..", "absent")},
		"/bin/sh", true, nil, pinned, nil, true, false,
	)
	literalGrant := "(allow file-write* (literal " + seatbeltQuote(swapped) + "))"
	subpathGrant := "(allow file-write* (subpath " + seatbeltQuote(swapped) + "))"
	if !strings.Contains(profile, literalGrant) || strings.Contains(profile, subpathGrant) {
		t.Fatalf("pinned file grant widened to a subtree:\n%s", profile)
	}
	// Materialization must fail closed on the type swap.
	if err := materializeMissingExactWritePaths(workspace, pinned); err == nil ||
		!strings.Contains(err.Error(), "no longer a directory") &&
			!strings.Contains(err.Error(), "changed type") {
		t.Fatalf("type swap error = %v", err)
	}
}

func TestWriteTreeDriftFailsClosedAndStaysClassified(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "generated")
	if err := os.Mkdir(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := validateExactWorkspaceWritePaths(workspace, true, []string{"generated"})
	if err != nil || len(pinned) != 1 || pinned[0].kind != writePathTree {
		t.Fatalf("pinned = %+v err = %v", pinned, err)
	}
	// The OS-level guarantee for protected entries inside the tree is the
	// profile deny (see TestWriteTreeProfileDeniesProtectedSubpaths); the
	// materialization guarantee is type fail-closed: the tree turning into
	// a file or a symlink must never execute with subtree grants.
	canonicalTree := pinned[0].path
	if err := os.Remove(canonicalTree); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonicalTree, []byte("swapped"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := materializeMissingExactWritePaths(workspace, pinned); err == nil ||
		!strings.Contains(err.Error(), "no longer a directory") {
		t.Fatalf("tree-to-file swap error = %v", err)
	}
	if err := os.Remove(canonicalTree); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, canonicalTree); err != nil {
		t.Fatal(err)
	}
	if err := materializeMissingExactWritePaths(workspace, pinned); err == nil ||
		!strings.Contains(err.Error(), "changed type") {
		t.Fatalf("tree-to-symlink swap error = %v", err)
	}
}

func TestWriteTreeProfileDeniesProtectedSubpaths(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "generated")
	if err := os.Mkdir(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	profile := seatbeltProfileForCommand(
		Policy{WorkspaceRoot: root, PrivateTemp: filepath.Join(root, "..", "absent")},
		"/bin/sh", true, nil,
		[]workspaceWritePath{{path: tree, kind: writePathTree}},
		nil, true, false,
	)
	deny := "(deny file-write* (subpath " + seatbeltQuote(filepath.Join(tree, ".git")) + "))"
	if !strings.Contains(profile, deny) {
		t.Fatalf("protected subpath deny missing:\n%s", profile)
	}
	// Exact-file grants must not carry subpath denies.
	fileProfile := seatbeltProfileForCommand(
		Policy{WorkspaceRoot: root, PrivateTemp: filepath.Join(root, "..", "absent")},
		"/bin/sh", true, nil,
		[]workspaceWritePath{{path: filepath.Join(root, "out.txt"), kind: writePathFile}},
		nil, true, false,
	)
	if strings.Contains(fileProfile, "subpath "+seatbeltQuote(filepath.Join(root, "out.txt", ".git"))) {
		t.Fatalf("file grant carries a tree deny:\n%s", fileProfile)
	}
}

func TestAllowNetworkWithLoopbackChoosesLoopbackBranch(t *testing.T) {
	// Documented precedence: when both a managed proxy port / loopback and a
	// broad network policy exist, the narrower loopback branch wins and
	// external egress is dropped for that command.
	root := t.TempDir()
	profile := seatbeltProfileForCommand(
		Policy{
			WorkspaceRoot: root, PrivateTemp: filepath.Join(root, "..", "absent"),
			AllowNetwork: true,
		},
		"/bin/sh", true, nil, nil, nil, false, true,
	)
	if strings.Contains(profile, "\n(allow network-outbound)\n") {
		t.Fatalf("broad network grant leaked into a loopback command:\n%s", profile)
	}
	if !strings.Contains(profile, `remote ip "localhost:*"`) {
		t.Fatalf("loopback grant missing:\n%s", profile)
	}
}

func TestInjectedRootsMayNotTouchPrivateTemp(t *testing.T) {
	workspace := t.TempDir()
	privateTemp := t.TempDir()
	inside := filepath.Join(privateTemp, "session")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	netrcFile := filepath.Join(privateTemp, "netrc")
	if err := os.WriteFile(netrcFile, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, options := range map[string]Options{
		"read root inside temp": {
			WorkspaceRoot: workspace, PrivateTemp: privateTemp,
			HostReadRoots: []string{inside},
		},
		"read root containing temp": {
			WorkspaceRoot: workspace, PrivateTemp: privateTemp,
			HostReadRoots: []string{privateTemp},
		},
		"write root inside temp": {
			WorkspaceRoot: workspace, PrivateTemp: privateTemp,
			HostWriteRoots: []string{inside},
		},
		"read file inside temp": {
			WorkspaceRoot: workspace, PrivateTemp: privateTemp,
			HostReadFiles: []string{netrcFile},
		},
	} {
		if _, err := BuildPolicy(options); err == nil ||
			!strings.Contains(err.Error(), "private sandbox temp") {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
}

func TestSensitiveCredentialDenylistCoversCommonStores(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("home is unavailable")
	}
	for _, path := range []string{
		filepath.Join(home, ".npmrc"),
		filepath.Join(home, ".kube", "config"),
		filepath.Join(home, ".docker", "config.json"),
		filepath.Join(home, ".config", "gh", "hosts.yml"),
	} {
		if err := validateSensitivePath(path); err == nil {
			t.Fatalf("sensitive path %q was accepted", path)
		}
	}
	if err := validateSensitivePath(filepath.Join(home, "ordinary.conf")); err != nil {
		t.Fatalf("ordinary config rejected: %v", err)
	}
}

func TestSystemProfileAuditRejectsOversizedProfile(t *testing.T) {
	oversized := filepath.Join(t.TempDir(), "system.sb")
	if err := os.WriteFile(
		oversized, make([]byte, maxSystemProfileBytes+1), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := auditSeatbeltSystemProfileAt(oversized); err == nil ||
		!strings.Contains(err.Error(), "audit limit") {
		t.Fatalf("oversized profile error = %v", err)
	}
}
