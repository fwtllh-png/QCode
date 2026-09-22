package environment

import (
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	envcontract "github.com/fwtllh-png/QCode/internal/environment"
	platformenv "github.com/fwtllh-png/QCode/internal/platform/environment"
)

func TestLookPathFromEnvUsesSourcePATH(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "go")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/usr/bin")
	got, err := lookPathFromEnv([]string{"PATH=" + dir}, "go")
	if err != nil {
		t.Fatal(err)
	}
	if got != fake {
		t.Fatalf("lookPathFromEnv = %q want %q", got, fake)
	}
}

func TestGoDiscoverUsesPublicEnvInterfaces(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go executable is required")
	}
	sandboxHome := t.TempDir()
	requests, facts, err := Go{}.Discover(t.Context(), platformenv.DiscoverInput{
		SandboxHome: sandboxHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]envcontract.ResourceRequest{}
	for _, request := range requests {
		if err := envcontract.ValidateRequest(request); err != nil {
			t.Fatalf("%s: %v", request.Name, err)
		}
		byName[request.Name] = request
	}
	if byName["go-executable"].Path == "" ||
		byName["go-env-goproxy"].Env != "GOPROXY" ||
		byName["go-mod-cache"].Env != "GOMODCACHE" ||
		byName["go-build-cache"].Env != "GOCACHE" ||
		byName["go-tmp"].Env != "GOTMPDIR" {
		t.Fatalf("go adapter requests = %+v", byName)
	}
	if strings.Contains(byName["go-env-goproxy"].Value, "@") &&
		strings.Contains(byName["go-env-goproxy"].Value, "://") {
		if parsed, err := url.Parse(strings.Split(byName["go-env-goproxy"].Value, "|")[0]); err == nil &&
			parsed.User != nil {
			t.Fatalf("GOPROXY userinfo leaked: %q", byName["go-env-goproxy"].Value)
		}
	}
	for _, request := range requests {
		name := strings.ToUpper(request.Env)
		if name == "" {
			continue
		}
		switch name {
		case "GOPROXY", "GONOSUMDB", "GOSUMDB", "GOPRIVATE", "GO111MODULE",
			"GOROOT", "GOMODCACHE", "GOCACHE", "GOTMPDIR", "GOENV":
		default:
			t.Fatalf("non-public Go env leaked: %s", request.Env)
		}
	}
	if _, ok := byName["goproxy-direct-fallback"]; ok {
		t.Fatal("|direct must not become a bindable network grant")
	}
	_ = facts
}

func TestGoproxyDirectFallbackIsNotAutoGranted(t *testing.T) {
	requests, facts := goproxyNetwork("https://goproxy.example:443|direct")
	for _, request := range requests {
		if request.Host == "goproxy.example" && request.Namespace == envcontract.NamespaceNetwork {
			continue
		}
		if request.Namespace == envcontract.NamespaceCredential && request.Access == envcontract.AccessUse {
			continue
		}
		t.Fatalf("unexpected request: %+v", request)
	}
	if len(facts) != 1 ||
		facts[0].Category != envcontract.CategoryNetworkTargetUnapproved ||
		facts[0].Resource != "goproxy-direct-fallback" {
		t.Fatalf("direct facts = %+v", facts)
	}
	if _, facts := goproxyNetwork("off"); len(facts) != 0 {
		t.Fatalf("off proxy facts = %+v", facts)
	}
}

func TestRedactProxyUserinfo(t *testing.T) {
	got := redactProxyUserinfo("https://user:pass@goproxy.example,direct")
	if strings.Contains(got, "user") || strings.Contains(got, "pass") {
		t.Fatalf("userinfo remains: %q", got)
	}
	if !strings.Contains(got, "goproxy.example") {
		t.Fatalf("host stripped: %q", got)
	}
}

func TestGoDiscoverWithoutSandboxHomeOmitsCaches(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go executable is required")
	}
	requests, _, err := Go{}.Discover(t.Context(), platformenv.DiscoverInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range requests {
		if request.Namespace == envcontract.NamespaceCache {
			t.Fatalf("cache without sandbox home: %+v", request)
		}
		if request.Env == "GOROOT" && !filepath.IsAbs(request.Path) {
			t.Fatalf("GOROOT path = %q", request.Path)
		}
	}
}

func writeGoMod(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestToolchainFactsExplainAutoSwitchWhenModuleNeedsNewerGo(t *testing.T) {
	workspace := t.TempDir()
	writeGoMod(t, workspace, "module example.com/eds\n\ngo 1.30\n")
	facts := toolchainFacts(workspace, map[string]string{
		"GOVERSION": "go1.26.3", "GOTOOLCHAIN": "auto",
	})
	if len(facts) != 1 ||
		facts[0].Category != envcontract.CategoryEnvironmentResourceUnavailable ||
		facts[0].Resource != "go-toolchain" {
		t.Fatalf("facts = %+v", facts)
	}
	if !strings.Contains(facts[0].Detail, "go >= 1.30") ||
		!strings.Contains(facts[0].Detail, "go1.26.3") ||
		!strings.Contains(facts[0].Detail, "GOTOOLCHAIN=auto") {
		t.Fatalf("detail = %q", facts[0].Detail)
	}
	if facts[0].RequiredAction != "" {
		t.Fatalf("auto switch needs no action: %+v", facts[0])
	}
}

func TestToolchainFactsStaySilentWhenHostSatisfiesModule(t *testing.T) {
	workspace := t.TempDir()
	writeGoMod(t, workspace, "module example.com/eds\n\ngo 1.20\n")
	if facts := toolchainFacts(workspace, map[string]string{
		"GOVERSION": "go1.26.3",
	}); len(facts) != 0 {
		t.Fatalf("satisfied requirement produced facts: %+v", facts)
	}
	nested := filepath.Join(workspace, "eds_metaserver")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGoMod(t, nested, "module example.com/eds/metaserver\n\ngo 1.25\n")
	if facts := toolchainFacts(workspace, map[string]string{
		"GOVERSION": "go1.26.3",
	}); len(facts) != 0 {
		t.Fatalf("satisfied nested requirement produced facts: %+v", facts)
	}
}

func TestToolchainFactsHonorToolchainDirectiveAndLocalSwitch(t *testing.T) {
	workspace := t.TempDir()
	writeGoMod(t, workspace, "module example.com/eds\n\ngo 1.20\n\ntoolchain go1.31.2\n")
	facts := toolchainFacts(workspace, map[string]string{
		"GOVERSION": "go1.26.3", "GOTOOLCHAIN": "local",
	})
	if len(facts) != 1 || !strings.Contains(facts[0].Detail, "go >= 1.31.2") {
		t.Fatalf("facts = %+v", facts)
	}
	if !strings.Contains(facts[0].Detail, "GOTOOLCHAIN=local") ||
		facts[0].RequiredAction != envcontract.ActionApproveHostConfig {
		t.Fatalf("local switch must explain and require action: %+v", facts[0])
	}
}

func TestModuleGoRequirementTakesStrictestAcrossModules(t *testing.T) {
	workspace := t.TempDir()
	writeGoMod(t, workspace, "module example.com/eds\n\ngo 1.20\n")
	nested := filepath.Join(workspace, "eds_metaserver")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGoMod(t, nested, "module example.com/eds/metaserver\n\ngo 1.28\n")
	goVersion, toolchain := moduleGoRequirement(workspace)
	if goVersion != "1.28" || toolchain != "" {
		t.Fatalf("requirement = %q toolchain = %q", goVersion, toolchain)
	}
	vendored := filepath.Join(workspace, "vendor", "dep")
	if err := os.MkdirAll(vendored, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGoMod(t, vendored, "module vendored/dep\n\ngo 1.99\n")
	if goVersion, _ := moduleGoRequirement(workspace); goVersion != "1.28" {
		t.Fatalf("vendored manifest leaked in: %q", goVersion)
	}
}

func TestCompareGoVersion(t *testing.T) {
	for _, test := range []struct {
		left, right string
		want        int
	}{
		{"1.25", "1.25", 0},
		{"go1.26.3", "1.25", 1},
		{"1.20", "go1.26.3", -1},
		{"1.31.2", "1.31.10", -1},
		{"", "1.20", -1},
	} {
		if got := compareGoVersion(test.left, test.right); got != test.want {
			t.Fatalf("compare(%q, %q) = %d, want %d", test.left, test.right, got, test.want)
		}
	}
}
