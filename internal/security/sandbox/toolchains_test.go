package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPolicyInheritsPATHBinsWithoutNamedLanguageRoots(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	bin := filepath.Join(root, "host-tools", "bin")
	rustup := filepath.Join(root, "host-state", "rustup")
	for _, directory := range []string{workspace, bin, rustup} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"rustup", "cargo", "rustc"} {
		if err := os.WriteFile(
			filepath.Join(bin, name),
			[]byte("#!/bin/sh\n"),
			0o755,
		); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("RUSTUP_HOME", rustup)

	policy, err := BuildPolicy(Options{
		WorkspaceRoot: workspace,
		PrivateTemp:   private,
	})
	if err != nil {
		t.Fatal(err)
	}
	canonicalBin, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(policy.Toolchains.BinDirs, canonicalBin) {
		t.Fatalf("toolchain bins = %v, want %s", policy.Toolchains.BinDirs, canonicalBin)
	}
	if slices.Contains(policy.Toolchains.ReadRoots, rustup) ||
		slices.Contains(policy.HostReadRoots, rustup) {
		t.Fatalf(
			"named language root leaked: roots=%v host=%v",
			policy.Toolchains.ReadRoots,
			policy.HostReadRoots,
		)
	}
	for _, entry := range policy.Toolchains.Environment {
		if strings.HasPrefix(entry, "RUSTUP_HOME=") {
			t.Fatalf("named language env leaked: %v", policy.Toolchains.Environment)
		}
	}
}

func TestPolicyDoesNotExposePackageRootsFromPATHBins(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	nodeRoot := filepath.Join(root, "node")
	npmRoot := filepath.Join(root, "npm")
	private := filepath.Join(root, "private")
	for _, directory := range []string{
		workspace,
		filepath.Join(nodeRoot, "bin"),
		filepath.Join(npmRoot, "bin"),
		private,
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(nodeRoot, "bin", "node"): "#!/bin/sh\n",
		filepath.Join(npmRoot, "bin", "npm"):   "#!/bin/sh\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(
		"PATH",
		strings.Join(
			[]string{
				filepath.Join(nodeRoot, "bin"),
				filepath.Join(npmRoot, "bin"),
			},
			string(os.PathListSeparator),
		),
	)

	policy, err := BuildPolicy(Options{
		WorkspaceRoot: workspace,
		PrivateTemp:   private,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{nodeRoot, npmRoot} {
		want, err = filepath.EvalSymlinks(want)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(policy.Toolchains.ReadRoots, want) {
			t.Fatalf("package root leaked: %s in %v", want, policy.Toolchains.ReadRoots)
		}
	}
}

func TestPolicyFollowsOutOfDirPATHSymlinks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	private := filepath.Join(root, "private")
	bin := filepath.Join(root, "bin")
	cellar := filepath.Join(root, "Cellar", "tool", "1.0")
	realBin := filepath.Join(cellar, "bin")
	for _, directory := range []string{workspace, private, bin, realBin} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	real := filepath.Join(realBin, "tool")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(bin, "tool")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	policy, err := BuildPolicy(Options{
		WorkspaceRoot: workspace,
		PrivateTemp:   private,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(policy.Toolchains.ReadRoots, realBin) {
		t.Fatalf("resolved bin missing: %v", policy.Toolchains.ReadRoots)
	}
	if !slices.Contains(policy.Toolchains.ReadRoots, cellar) {
		t.Fatalf("resolved package root missing: %v", policy.Toolchains.ReadRoots)
	}
}

func TestDiscoverToolchainsDoesNotInjectGOROOT(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	installation := filepath.Join(root, "go", "libexec")
	bin := filepath.Join(root, "bin")
	for _, directory := range []string{
		filepath.Join(installation, "bin"), filepath.Join(installation, "src"),
		filepath.Join(installation, "pkg", "tool"), bin,
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	executable := filepath.Join(installation, "bin", "go")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(bin, "go")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GOROOT", "")
	exposure := discoverToolchains(filepath.Join(root, "workspace"), nil, nil)
	for _, entry := range exposure.Environment {
		if strings.HasPrefix(entry, "GOROOT=") {
			t.Fatalf("GOROOT leaked: %v", exposure.Environment)
		}
	}
}

func TestBuildPolicyRejectsLegacyContract(t *testing.T) {
	_, err := BuildPolicy(Options{
		WorkspaceRoot:       t.TempDir(),
		PrivateTemp:         t.TempDir(),
		EnvironmentContract: "legacy",
		SkipPATHReadRoots:   true,
	})
	if err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("legacy contract error = %v", err)
	}
}
