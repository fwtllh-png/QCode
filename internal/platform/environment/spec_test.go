package environment

import (
	_ "embed"
	"encoding/json"
	"fmt"
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
	spec, err := edsGoModuleInfoSpec()
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
	spec, err := edsGoModuleInfoSpec()
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
	matrix := platformMatrix()
	if len(matrix) != 6 {
		t.Fatalf("platform matrix rows = %d", len(matrix))
	}
	seen := make(map[capabilityID]bool, len(matrix))
	for _, row := range matrix {
		if seen[row.ID] {
			t.Fatalf("duplicate capability %q", row.ID)
		}
		seen[row.ID] = true
		support, err := supportFor("darwin", row.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch support {
		case supportAvailable, supportUnsupported, supportNotYet:
		default:
			t.Fatalf("darwin/%s has no declared support: %q", row.ID, support)
		}
	}
	if support, err := supportFor("darwin", capPrivateTmpView); err != nil ||
		support != supportUnsupported {
		t.Fatalf("darwin private tmp = %q %v", support, err)
	}
	for _, goos := range []string{"linux", "windows", "plan9"} {
		if _, err := supportFor(goos, capManagedProxy); err == nil {
			t.Fatalf("unsupported platform %q was accepted", goos)
		}
	}
	if _, err := supportFor(runtime.GOOS, "not_a_capability"); err == nil {
		t.Fatal("unknown capability must not invent support")
	}
}

func TestEDSBaselineObservationsStayDistinct(t *testing.T) {
	seen := make(map[string]bool)
	for _, observation := range edsGoBaselineObservations() {
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

//go:embed testdata/eds-go-module-info.json
var edsGoModuleInfoJSON []byte

func edsGoModuleInfoSpec() (EnvironmentSpec, error) {
	var spec EnvironmentSpec
	if err := json.Unmarshal(edsGoModuleInfoJSON, &spec); err != nil {
		return EnvironmentSpec{}, err
	}
	return spec, nil
}

func edsGoBaselineObservations() []baselineObservation {
	return []baselineObservation{
		{
			ID:             "missing_host_go_env",
			Category:       CategoryEnvironmentResourceUnavailable,
			RequiredAction: ActionApproveHostConfig,
			ForbiddenClaim: "host has no GOPROXY",
		},
		{
			ID:             "goproxy_auth_failed",
			Category:       CategoryCredentialUnavailable,
			RequiredAction: ActionBindCredential,
			ForbiddenClaim: "network unreachable",
		},
		{
			ID:             "mktemp_user_temp_eperm",
			Category:       CategoryFilesystemAccessDenied,
			RequiredAction: ActionEnableSharedUserTemp,
			ForbiddenClaim: "disable GOPROXY because temp is broken",
		},
		{
			ID:             "direct_fallback_unapproved",
			Category:       CategoryNetworkTargetUnapproved,
			RequiredAction: ActionApproveNetworkTarget,
			ForbiddenClaim: "treat Forbidden as the proxy response",
		},
	}
}

type baselineObservation struct {
	ID             string
	Category       string
	RequiredAction string
	ForbiddenClaim string
}

type support string

const (
	supportAvailable   support = "available"
	supportUnsupported support = "unsupported"
	supportNotYet      support = "not_yet"
)

type capabilityID string

const (
	capStrongSandboxReadRoots capabilityID = "strong_sandbox_read_roots"
	capManagedProxy           capabilityID = "managed_proxy"
	capResolveUserTemp        capabilityID = "resolve_user_temp"
	capPrivateTmpView         capabilityID = "private_tmp_view"
	capIndependentIdentity    capabilityID = "independent_identity"
	capCertificateFiles       capabilityID = "certificate_files"
)

type platformCapability struct {
	ID     capabilityID `json:"id"`
	Darwin support      `json:"darwin"`
}

func platformMatrix() []platformCapability {
	return []platformCapability{
		{
			ID:     capStrongSandboxReadRoots,
			Darwin: supportAvailable,
		},
		{
			ID:     capManagedProxy,
			Darwin: supportAvailable,
		},
		{
			ID:     capResolveUserTemp,
			Darwin: supportAvailable,
		},
		{
			ID:     capPrivateTmpView,
			Darwin: supportUnsupported,
		},
		{
			ID:     capIndependentIdentity,
			Darwin: supportNotYet,
		},
		{
			ID:     capCertificateFiles,
			Darwin: supportAvailable,
		},
	}
}

func supportFor(goos string, id capabilityID) (support, error) {
	for _, capability := range platformMatrix() {
		if capability.ID != id {
			continue
		}
		switch goos {
		case "darwin":
			return capability.Darwin, nil
		default:
			return "", fmt.Errorf("platform %q is not in the capability matrix", goos)
		}
	}
	return "", fmt.Errorf("capability %q is not in the platform matrix", id)
}
