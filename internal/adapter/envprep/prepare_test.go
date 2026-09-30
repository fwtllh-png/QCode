package envprep

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/common/environment"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestPrepareMergesPlatformPATH(t *testing.T) {
	extra, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(t.Context(), Options{
		Sandbox: sandbox.Options{
			EnvironmentContract: environment.ContractV1, EnvironmentProfile: environment.ProfileNative,
			WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(),
		},
		SourceEnv: []string{"HOME=" + t.TempDir(), "LANG=C", "PATH=" + extra},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := envLookup(prepared.Sandbox.EnvironmentValues, "PATH")
	if !strings.Contains(got, extra) {
		t.Fatalf("prepared PATH omitted source dir: %q", got)
	}
	foundSourceDir := false
	for _, request := range prepared.Spec.Requests {
		if request.Name == "path-dir:"+extra {
			foundSourceDir = true
			if request.Namespace != environment.NamespaceHostToolchain {
				t.Fatalf("path dir namespace = %s", request.Namespace)
			}
		}
	}
	if !foundSourceDir {
		t.Fatalf("path-dir request missing: %+v", prepared.Spec.Requests)
	}
}

func TestPrepareIsolatedRewritesHomeAndPrivateTemp(t *testing.T) {
	home := t.TempDir()
	sandboxHome := t.TempDir()
	prepared, err := Prepare(t.Context(), Options{
		Sandbox: sandbox.Options{
			EnvironmentContract: environment.ContractV1, EnvironmentProfile: environment.ProfileIsolated,
			WorkspaceRoot: t.TempDir(), PrivateTemp: sandboxHome,
		},
		SourceEnv: []string{"HOME=" + home, "LANG=C", "PATH=/usr/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if envLookup(prepared.Sandbox.EnvironmentValues, "HOME") != sandboxHome ||
		envLookup(prepared.Sandbox.EnvironmentValues, "TMPDIR") != sandboxHome {
		t.Fatalf("isolated env = %v", prepared.Sandbox.EnvironmentValues)
	}
	if prepared.UserTemp != "" {
		t.Fatal("isolated must not resolve shared user temp")
	}
}

func TestPrepareNativeKeepsHostHomeAndPrivateTemp(t *testing.T) {
	home := t.TempDir()
	sandboxHome := t.TempDir()
	prepared, err := Prepare(t.Context(), Options{
		Sandbox: sandbox.Options{
			EnvironmentContract: environment.ContractV1, EnvironmentProfile: environment.ProfileNative,
			WorkspaceRoot: t.TempDir(), PrivateTemp: sandboxHome,
		},
		SourceEnv: []string{"HOME=" + home, "LANG=C"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if envLookup(prepared.Sandbox.EnvironmentValues, "HOME") != home {
		t.Fatalf("native HOME rewritten: %v", prepared.Sandbox.EnvironmentValues)
	}
	if envLookup(prepared.Sandbox.EnvironmentValues, "TMPDIR") != sandboxHome {
		t.Fatalf("native without shared temp must stay private: %v", prepared.Sandbox.EnvironmentValues)
	}
}

func TestPrepareSharedUserTempSetsSystemTemp(t *testing.T) {
	userTemp, err := UserTempDir()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	sandboxHome := t.TempDir()
	prepared, err := Prepare(t.Context(), Options{
		Sandbox: sandbox.Options{
			EnvironmentContract: environment.ContractV1, EnvironmentProfile: environment.ProfileNative,
			SharedUserTemp: true, WorkspaceRoot: t.TempDir(), PrivateTemp: sandboxHome,
		},
		SourceEnv: []string{"HOME=" + home, "LANG=C"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if envLookup(prepared.Sandbox.EnvironmentValues, "HOME") != home ||
		envLookup(prepared.Sandbox.EnvironmentValues, "TMPDIR") != userTemp {
		t.Fatalf("shared temp env = %v want HOME=%s TMPDIR=%s", prepared.Sandbox.EnvironmentValues, home, userTemp)
	}
	if !containsPath(prepared.Sandbox.HostWriteRoots, filepath.Clean(userTemp)) {
		t.Fatalf("shared temp missing from write roots: %v", prepared.Sandbox.HostWriteRoots)
	}
}

func TestPrepareGenericToolUsesOnlyDeclaredResources(t *testing.T) {
	sandboxHome := t.TempDir()
	configFile := filepath.Join(t.TempDir(), "tool.conf")
	if err := os.WriteFile(configFile, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(t.Context(), Options{
		Sandbox: sandbox.Options{
			EnvironmentContract: environment.ContractV1, EnvironmentProfile: environment.ProfileNative,
			SharedUserTemp: true, WorkspaceRoot: t.TempDir(), PrivateTemp: sandboxHome,
		},
		SourceEnv:    []string{"HOME=" + t.TempDir(), "LANG=C"},
		Declarations: genericRequests(configFile),
	})
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(sandboxHome, "cache", "generic")
	if envLookup(prepared.Sandbox.EnvironmentValues, "GENERIC_CACHE") != cache {
		t.Fatalf("generic cache env = %v", prepared.Sandbox.EnvironmentValues)
	}
	if !containsPath(prepared.Sandbox.HostReadFiles, configFile) ||
		prepared.Sandbox.PrivateTemp != sandboxHome {
		t.Fatalf("generic sandbox = %+v", prepared.Sandbox)
	}
	info, err := os.Stat(cache)
	if err != nil || !info.IsDir() {
		t.Fatalf("declared cache tree was not materialized: %v", err)
	}
	if envLookup(prepared.Sandbox.EnvironmentValues, "GOMODCACHE") != "" ||
		envLookup(prepared.Sandbox.EnvironmentValues, "GOCACHE") != "" {
		t.Fatal("generic tool must not inject Go cache variables")
	}
}

func TestPrepareUserDeclarations(t *testing.T) {
	sandboxHome := t.TempDir()
	configFile := filepath.Join(t.TempDir(), "tool.conf")
	if err := os.WriteFile(configFile, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(t.Context(), Options{
		Sandbox: sandbox.Options{
			EnvironmentContract: environment.ContractV1, EnvironmentProfile: environment.ProfileNative,
			WorkspaceRoot: t.TempDir(), PrivateTemp: sandboxHome,
		},
		SourceEnv: []string{"HOME=" + t.TempDir(), "LANG=C"},
		Declarations: []environment.ResourceRequest{
			{
				Name: "tool-config", Namespace: environment.NamespaceHostConfig,
				Access: environment.AccessRead, Path: configFile,
			},
			{
				Name: "tool-cache", Namespace: environment.NamespaceCache,
				Access: environment.AccessWrite, Path: "sandbox-home/cache/declared",
				Env: "DECLARED_CACHE", Tree: true,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(sandboxHome, "cache", "declared")
	if envLookup(prepared.Sandbox.EnvironmentValues, "DECLARED_CACHE") != cache ||
		!containsPath(prepared.Sandbox.HostReadFiles, configFile) {
		t.Fatalf("declaration sandbox = %+v", prepared.Sandbox)
	}
	byName := map[string]environment.ResourceRequest{}
	for _, request := range prepared.Spec.Requests {
		byName[request.Name] = request
	}
	if byName["tool-config"].Source != environment.SourceUserDeclaration {
		t.Fatalf("declaration source = %+v", byName["tool-config"])
	}
	if envLookup(prepared.Sandbox.EnvironmentValues, "GOMODCACHE") != "" {
		t.Fatal("declaration path must not require a Go adapter")
	}
}

func TestPrepareRejectsSharedTempOnIsolated(t *testing.T) {
	_, err := Prepare(t.Context(), Options{
		Sandbox: sandbox.Options{
			EnvironmentContract: environment.ContractV1, EnvironmentProfile: environment.ProfileIsolated,
			SharedUserTemp: true, WorkspaceRoot: t.TempDir(),
		},
	})
	if err == nil {
		t.Fatal("isolated shared temp must fail")
	}
}

func TestPrepareCapturesSourceOnceAndKeepsEmptySourceExplicit(t *testing.T) {
	sourceHome := t.TempDir()
	t.Setenv("HOME", sourceHome)
	t.Setenv("LANG", "C")
	options := Options{
		Sandbox: sandbox.Options{
			EnvironmentProfile: environment.ProfileNative,
			WorkspaceRoot:      t.TempDir(), PrivateTemp: t.TempDir(),
		},
	}
	prepared, err := Prepare(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if envLookup(prepared.Sandbox.EnvironmentValues, "HOME") != sourceHome {
		t.Fatal("sandbox projection did not retain the original HOME")
	}
	options.SourceEnv = []string{}
	prepared, err = Prepare(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if envLookup(prepared.Sandbox.EnvironmentValues, "HOME") != "" ||
		envLookup(prepared.Sandbox.EnvironmentValues, "LANG") != "" {
		t.Fatal("explicit empty source inherited host variables")
	}
}

func TestPrepareRejectsInvalidDeclarationAndMaterializationFailure(t *testing.T) {
	options := Options{
		Sandbox: sandbox.Options{
			EnvironmentProfile: environment.ProfileNative,
			WorkspaceRoot:      t.TempDir(), PrivateTemp: t.TempDir(),
		},
		SourceEnv: []string{},
		Declarations: []environment.ResourceRequest{{
			Name: "bad", Namespace: environment.NamespaceCredential, Access: environment.AccessRead,
		}},
	}
	if _, err := Prepare(t.Context(), options); err == nil {
		t.Fatal("invalid credential declaration accepted")
	}
	options.Declarations = nil
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	options.Sandbox.PrivateTemp = filepath.Join(file, "sandbox-home")
	if _, err := Prepare(t.Context(), options); err == nil {
		t.Fatal("failed private home materialization was ignored")
	}
}

func genericRequests(config string) []environment.ResourceRequest {
	return []environment.ResourceRequest{
		{
			Name: "generic-config", Namespace: environment.NamespaceHostConfig,
			Access: environment.AccessRead, Path: config,
			Source: "test-declaration", Required: true, Lifecycle: "source_version",
		},
		{
			Name: "generic-cache", Namespace: environment.NamespaceCache,
			Access: environment.AccessWrite, Path: "sandbox-home/cache/generic",
			Env: "GENERIC_CACHE", Tree: true, Source: "test-declaration",
			Required: true, Lifecycle: "workspace",
		},
	}
}

func envLookup(env []string, name string) string {
	prefix := name + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}
