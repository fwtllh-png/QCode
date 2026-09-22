package environment

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	envcontract "github.com/fwtllh-png/QCode/internal/environment"
)

func TestPrepareMergesPlatformPATH(t *testing.T) {
	extra, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(t.Context(), Options{
		Contract: envcontract.ContractV1, Profile: envcontract.ProfileNative,
		WorkspaceRoot: t.TempDir(), SandboxHome: t.TempDir(),
		SourceEnv: []string{"HOME=" + t.TempDir(), "LANG=C", "PATH=" + extra},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := envLookup(prepared.Env, "PATH")
	if !strings.Contains(got, extra) {
		t.Fatalf("prepared PATH omitted source dir: %q", got)
	}
	foundSourceDir := false
	for _, request := range prepared.Spec.Requests {
		if request.Name == "path-dir:"+extra {
			foundSourceDir = true
			if request.Namespace != envcontract.NamespaceHostToolchain {
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
		Contract: envcontract.ContractV1, Profile: envcontract.ProfileIsolated,
		WorkspaceRoot: t.TempDir(), SandboxHome: sandboxHome,
		SourceEnv: []string{"HOME=" + home, "LANG=C", "PATH=/usr/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if envLookup(prepared.Env, "HOME") != sandboxHome ||
		envLookup(prepared.Env, "TMPDIR") != sandboxHome {
		t.Fatalf("isolated env = %v", prepared.Env)
	}
	if prepared.UserTemp != "" {
		t.Fatal("isolated must not resolve shared user temp")
	}
}

func TestPrepareNativeKeepsHostHomeAndPrivateTemp(t *testing.T) {
	home := t.TempDir()
	sandboxHome := t.TempDir()
	prepared, err := Prepare(t.Context(), Options{
		Contract: envcontract.ContractV1, Profile: envcontract.ProfileNative,
		WorkspaceRoot: t.TempDir(), SandboxHome: sandboxHome,
		SourceEnv: []string{"HOME=" + home, "LANG=C"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if envLookup(prepared.Env, "HOME") != home {
		t.Fatalf("native HOME rewritten: %v", prepared.Env)
	}
	if envLookup(prepared.Env, "TMPDIR") != sandboxHome {
		t.Fatalf("native without shared temp must stay private: %v", prepared.Env)
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
		Contract: envcontract.ContractV1, Profile: envcontract.ProfileNative,
		SharedUserTemp: true, WorkspaceRoot: t.TempDir(), SandboxHome: sandboxHome,
		SourceEnv: []string{"HOME=" + home, "LANG=C"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if envLookup(prepared.Env, "HOME") != home ||
		envLookup(prepared.Env, "TMPDIR") != userTemp {
		t.Fatalf("shared temp env = %v want HOME=%s TMPDIR=%s", prepared.Env, home, userTemp)
	}
	if !containsPath(prepared.WritePaths, userTemp) {
		t.Fatalf("shared temp missing from write paths: %v", prepared.WritePaths)
	}
}

func TestPrepareGenericToolUsesOnlyDeclaredResources(t *testing.T) {
	sandboxHome := t.TempDir()
	configFile := filepath.Join(t.TempDir(), "tool.conf")
	if err := os.WriteFile(configFile, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(t.Context(), Options{
		Contract: envcontract.ContractV1, Profile: envcontract.ProfileNative,
		SharedUserTemp: true, WorkspaceRoot: t.TempDir(), SandboxHome: sandboxHome,
		SourceEnv:   []string{"HOME=" + t.TempDir(), "LANG=C"},
		Discoverers: []Discoverer{genericTool{config: configFile}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(sandboxHome, "cache", "generic")
	if envLookup(prepared.Env, "GENERIC_CACHE") != cache {
		t.Fatalf("generic cache env = %v", prepared.Env)
	}
	if !containsPath(prepared.ReadPaths, configFile) ||
		!containsPath(prepared.WritePaths, cache) {
		t.Fatalf("generic paths read=%v write=%v", prepared.ReadPaths, prepared.WritePaths)
	}
	info, err := os.Stat(cache)
	if err != nil || !info.IsDir() {
		t.Fatalf("declared cache tree was not materialized: %v", err)
	}
	if envLookup(prepared.Env, "GOMODCACHE") != "" ||
		envLookup(prepared.Env, "GOCACHE") != "" {
		t.Fatal("generic tool must not inject Go cache variables")
	}
}

func TestPrepareUserDeclarationsWorkWithoutDiscoverers(t *testing.T) {
	sandboxHome := t.TempDir()
	configFile := filepath.Join(t.TempDir(), "tool.conf")
	if err := os.WriteFile(configFile, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(t.Context(), Options{
		Contract: envcontract.ContractV1, Profile: envcontract.ProfileNative,
		WorkspaceRoot: t.TempDir(), SandboxHome: sandboxHome,
		SourceEnv: []string{"HOME=" + t.TempDir(), "LANG=C"},
		Declarations: []envcontract.ResourceRequest{
			{
				Name: "tool-config", Namespace: envcontract.NamespaceHostConfig,
				Access: envcontract.AccessRead, Path: configFile,
			},
			{
				Name: "tool-cache", Namespace: envcontract.NamespaceCache,
				Access: envcontract.AccessWrite, Path: "sandbox-home/cache/declared",
				Env: "DECLARED_CACHE", Tree: true,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(sandboxHome, "cache", "declared")
	if envLookup(prepared.Env, "DECLARED_CACHE") != cache ||
		!containsPath(prepared.ReadPaths, configFile) {
		t.Fatalf("declaration prepare env=%v read=%v", prepared.Env, prepared.ReadPaths)
	}
	byName := map[string]envcontract.ResourceRequest{}
	for _, request := range prepared.Spec.Requests {
		byName[request.Name] = request
	}
	if byName["tool-config"].Source != envcontract.SourceUserDeclaration {
		t.Fatalf("declaration source = %+v", byName["tool-config"])
	}
	if envLookup(prepared.Env, "GOMODCACHE") != "" {
		t.Fatal("declaration path must not require a Go adapter")
	}
}

func TestPrepareUserDeclarationsOverrideDiscovererNames(t *testing.T) {
	sandboxHome := t.TempDir()
	userFile := filepath.Join(t.TempDir(), "user.conf")
	adapterFile := filepath.Join(t.TempDir(), "adapter.conf")
	for _, path := range []string{userFile, adapterFile} {
		if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prepared, err := Prepare(t.Context(), Options{
		Contract: envcontract.ContractV1, Profile: envcontract.ProfileNative,
		WorkspaceRoot: t.TempDir(), SandboxHome: sandboxHome,
		SourceEnv: []string{"HOME=" + t.TempDir(), "LANG=C"},
		Declarations: []envcontract.ResourceRequest{{
			Name: "generic-config", Namespace: envcontract.NamespaceHostConfig,
			Access: envcontract.AccessRead, Path: userFile,
		}},
		Discoverers: []Discoverer{genericTool{config: adapterFile}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsPath(prepared.ReadPaths, userFile) ||
		containsPath(prepared.ReadPaths, adapterFile) {
		t.Fatalf("declaration must win the name: %v", prepared.ReadPaths)
	}
}

func TestPrepareRejectsSharedTempOnIsolated(t *testing.T) {
	_, err := Prepare(t.Context(), Options{
		Contract: envcontract.ContractV1, Profile: envcontract.ProfileIsolated,
		SharedUserTemp: true, WorkspaceRoot: t.TempDir(),
	})
	if err == nil {
		t.Fatal("isolated shared temp must fail")
	}
}

type genericTool struct{ config string }

func (genericTool) Name() string { return "generic-test-tool" }

func (g genericTool) Discover(
	_ context.Context,
	_ DiscoverInput,
) ([]envcontract.ResourceRequest, []envcontract.Fact, error) {
	return []envcontract.ResourceRequest{
		{
			Name: "generic-config", Namespace: envcontract.NamespaceHostConfig,
			Access: envcontract.AccessRead, Path: g.config,
			Source: "test-declaration", Required: true, Lifecycle: "source_version",
		},
		{
			Name: "generic-cache", Namespace: envcontract.NamespaceCache,
			Access: envcontract.AccessWrite, Path: "sandbox-home/cache/generic",
			Env: "GENERIC_CACHE", Tree: true, Source: "test-declaration",
			Required: true, Lifecycle: "workspace",
		},
	}, nil, nil
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
