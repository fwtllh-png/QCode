package environment

import (
	"runtime"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/authority"
)

func TestChildProfileIsAlwaysIsolated(t *testing.T) {
	if ChildProfile(ProfileNative) != ProfileIsolated ||
		ChildProfile(ProfileIsolated) != ProfileIsolated {
		t.Fatal("child profile must stay isolated")
	}
}

func TestStampDeclarationSetsSourceAndLifecycle(t *testing.T) {
	stamped := StampDeclaration(ResourceRequest{
		Name: "tool-cache", Namespace: NamespaceCache, Access: AccessWrite,
	})
	if stamped.Source != SourceUserDeclaration || stamped.Lifecycle != "workspace" {
		t.Fatalf("stamped cache = %+v", stamped)
	}
}

func TestValidateRequestRestrictsAccessUseToCredentials(t *testing.T) {
	if err := ValidateRequest(ResourceRequest{
		Name: "secret-file", Namespace: NamespaceHostConfig, Access: AccessUse,
		Source: "test",
	}); err == nil {
		t.Fatal("use on host_config must fail")
	}
	if err := ValidateRequest(ResourceRequest{
		Name: "proxy-credential", Namespace: NamespaceCredential, Access: AccessRead,
		Source: "test",
	}); err == nil {
		t.Fatal("credential read must fail")
	}
	if err := ValidateRequest(ResourceRequest{
		Name: "proxy-credential", Namespace: NamespaceCredential, Access: AccessUse,
		Host: "goproxy.example", Source: "test",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestValidatePostureRejectsSharedTempOnIsolated(t *testing.T) {
	if err := ValidatePosture(ProfileNative, true); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePosture(ProfileIsolated, true); err == nil {
		t.Fatal("isolated shared temp must fail")
	}
}

func TestEDSGoModuleInfoFixtureCompiles(t *testing.T) {
	spec, err := EDSGoModuleInfoSpec()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSpec(spec); err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileSpec(spec, BindContext{})
	if err != nil {
		t.Fatal(err)
	}
	byName := fixtureByName(t, compiled)

	proxy := byName["goproxy-byted"]
	if !proxy.Bindable ||
		proxy.Resource.Namespace != authority.NamespaceNetwork ||
		proxy.Resource.ID != "goproxy.byted.org" ||
		proxy.Resource.Port != 443 {
		t.Fatalf("compiled GOPROXY resource = %+v", proxy)
	}

	direct := byName["goproxy-direct-fallback"]
	if direct.Request.Required ||
		direct.Request.Source != "goproxy-direct-not-auto-granted" {
		t.Fatalf("direct fallback = %+v", direct)
	}

	credential := byName["goproxy-credential"]
	if !credential.Bindable ||
		credential.Resource.Namespace != authority.NamespaceCredential ||
		credential.Resource.Access != tool.AccessUse {
		t.Fatalf("credential must bind as use: %+v", credential)
	}

	env := byName["go-env-goproxy"]
	if !env.Bindable ||
		env.Resource.Namespace != authority.NamespaceHostConfig ||
		env.Resource.Kind != "env" ||
		env.Resource.ID != "GOPROXY" {
		t.Fatalf("host_config env = %+v", env)
	}

	if byName["go-mod-cache"].Bindable ||
		byName["go-mod-cache"].Reason != "path_root_unresolved" {
		t.Fatalf("cache without sandbox home = %+v", byName["go-mod-cache"])
	}
	if byName["darwin-user-temp"].Bindable ||
		byName["darwin-user-temp"].Reason != "path_root_unresolved" {
		t.Fatalf("shared temp without user temp = %+v", byName["darwin-user-temp"])
	}
	if !byName["darwin-user-temp"].Request.Shared {
		t.Fatal("shared user temp must declare shared")
	}
}

func TestCompileBindsSection8WithResolvedRoots(t *testing.T) {
	spec, err := EDSGoModuleInfoSpec()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileSpec(spec, BindContext{
		WorkspaceRoot: t.TempDir(), WorkspaceID: strings.Repeat("a", 64),
		WorkspaceGeneration: 1, SandboxHome: t.TempDir(), UserTemp: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := fixtureByName(t, compiled)
	for _, name := range []string{
		"go-env-goproxy", "goproxy-byted", "goproxy-credential",
		"go-mod-cache", "go-build-cache", "darwin-user-temp",
	} {
		if !byName[name].Bindable {
			t.Fatalf("%s must bind under v1+native roots: %+v", name, byName[name])
		}
	}
	cache := byName["go-mod-cache"]
	if cache.Resource.Namespace != authority.NamespaceCache || !cache.Resource.Tree ||
		cache.Request.Env != "GOMODCACHE" {
		t.Fatalf("cache tree = %+v", cache)
	}
	temp := byName["darwin-user-temp"]
	if temp.Resource.Namespace != authority.NamespaceSharedUserTemp ||
		!temp.Resource.Tree {
		t.Fatalf("shared temp = %+v", temp.Resource)
	}
}

func fixtureByName(t *testing.T, compiled []CompiledResource) map[string]CompiledResource {
	t.Helper()
	byName := make(map[string]CompiledResource, len(compiled))
	for _, item := range compiled {
		byName[item.Request.Name] = item
	}
	return byName
}

func TestPlatformMatrixCoversDeclaredPlatforms(t *testing.T) {
	matrix := PlatformMatrix()
	if len(matrix) != 6 {
		t.Fatalf("platform matrix rows = %d", len(matrix))
	}
	seen := make(map[CapabilityID]bool, len(matrix))
	for _, row := range matrix {
		if seen[row.ID] {
			t.Fatalf("duplicate capability %q", row.ID)
		}
		seen[row.ID] = true
		support, err := SupportFor("darwin", row.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch support {
		case SupportAvailable, SupportUnsupported, SupportNotYet:
		default:
			t.Fatalf("darwin/%s has no declared support: %q", row.ID, support)
		}
	}
	if support, err := SupportFor("darwin", CapPrivateTmpView); err != nil ||
		support != SupportUnsupported {
		t.Fatalf("darwin private tmp = %q %v", support, err)
	}
	for _, goos := range []string{"linux", "windows", "plan9"} {
		if _, err := SupportFor(goos, CapManagedProxy); err == nil {
			t.Fatalf("unsupported platform %q was accepted", goos)
		}
	}
	if _, err := SupportFor(runtime.GOOS, "not_a_capability"); err == nil {
		t.Fatal("unknown capability must not invent support")
	}
}

func TestEDSBaselineObservationsStayDistinct(t *testing.T) {
	seen := make(map[string]bool)
	for _, observation := range EDSGoBaselineObservations() {
		if observation.ID == "" || observation.Category == "" ||
			observation.RequiredAction == "" || observation.ForbiddenClaim == "" {
			t.Fatalf("incomplete baseline observation: %+v", observation)
		}
		if seen[observation.Category] {
			t.Fatalf("duplicate baseline category %q", observation.Category)
		}
		seen[observation.Category] = true
	}
	if !seen[CategoryEnvironmentResourceUnavailable] ||
		!seen[CategoryCredentialUnavailable] ||
		!seen[CategoryFilesystemAccessDenied] ||
		!seen[CategoryNetworkTargetUnapproved] {
		t.Fatalf("baseline categories = %v", seen)
	}
}
