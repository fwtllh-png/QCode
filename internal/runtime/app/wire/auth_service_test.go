package wire

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/environment"
	"github.com/fwtllh-png/QCode/internal/security/goproxy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestEnvironmentFingerprintNamesBoundAuthService(t *testing.T) {
	if lines := environmentFingerprint(nil); containsLine(lines, authServiceFingerprint) {
		t.Fatalf("unbound fingerprint leaked auth service: %v", lines)
	}
	service, err := goproxy.New(goproxy.Binding{
		Upstream:   "http://127.0.0.1:9",
		Prefixes:   []string{"*"},
		Credential: goproxy.CredentialRef{Kind: "env", Name: "QCODE_TEST_GOPROXY_TOKEN"},
	}, func(context.Context, string, string) (string, error) {
		return "user:secret", nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	lines := environmentFingerprint(service)
	if !containsLine(lines, authServiceFingerprint) {
		t.Fatalf("bound fingerprint missing auth service: %v", lines)
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "secret") || strings.Contains(joined, "QCODE_TEST") {
		t.Fatalf("fingerprint leaked a credential: %q", joined)
	}
}

func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func TestBindAuthServicesLeavesEmptyConfigUnbound(t *testing.T) {
	service, _, err := bindAuthServices(&buildState{})
	if err != nil || service != nil {
		t.Fatalf("empty auth services = %v err=%v", service, err)
	}
}

func TestBindAuthServicesUsesHostGoproxyCredential(t *testing.T) {
	if !sandbox.SupportsManagedNetworkProxy() {
		t.Skip("process session channels are unsupported")
	}
	backend := &recordingProtocolBackend{}
	service, _, err := bindAuthServices(&buildState{
		platform: platformBuildState{
			backend:          backend,
			hostGoproxyValue: "https://user:host-token@goproxy.example,direct",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if service == nil || backend.handler == nil {
		t.Fatalf("host GOPROXY auth was not bound: service=%v handler=%v", service, backend.handler)
	}
	env := service.RewriteProcessEnv(nil, "http://127.0.0.1:4321")
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GOPROXY=http://127.0.0.1:4321") {
		t.Fatalf("session GOPROXY missing: %v", env)
	}
	if strings.Contains(joined, "host-token") || strings.Contains(joined, "goproxy.example") {
		t.Fatalf("host secret or upstream leaked into process env: %v", env)
	}
}

func TestBindAuthServicesLeavesHostProxyUnboundWithoutCredential(t *testing.T) {
	if !sandbox.SupportsManagedNetworkProxy() {
		t.Skip("process session channels are unsupported")
	}
	service, _, err := bindAuthServices(&buildState{
		platform: platformBuildState{
			backend:          &recordingProtocolBackend{},
			hostGoproxyValue: "https://proxy.golang.org,direct",
			hostNetrcPath:    "/no/such/netrc",
		},
	})
	if err != nil || service != nil {
		t.Fatalf("public GOPROXY without host credential = %v err=%v", service, err)
	}
}

func TestBindAuthServicesRejectsSecondService(t *testing.T) {
	_, _, err := bindAuthServices(&buildState{
		config: configBuildState{execution: config.Execution{
			Environment: config.ExecutionEnvironment{
				AuthServices: []config.EnvironmentAuthService{
					testGoproxyService(),
					testGoproxyService(),
				},
			},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "only one") {
		t.Fatalf("second service error = %v", err)
	}
}

func TestBindAuthServicesPrefersDeclaredOverHost(t *testing.T) {
	if !sandbox.SupportsManagedNetworkProxy() {
		t.Skip("process session channels are unsupported")
	}
	backend := &recordingProtocolBackend{}
	service, _, err := bindAuthServices(&buildState{
		config: configBuildState{execution: config.Execution{
			Environment: config.ExecutionEnvironment{
				AuthServices: []config.EnvironmentAuthService{testGoproxyService()},
			},
		}},
		platform: platformBuildState{
			backend:          backend,
			hostGoproxyValue: "https://user:host-token@other.example",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if service == nil {
		t.Fatal("declared auth service was not bound")
	}
	env := strings.Join(service.RewriteProcessEnv(nil, "http://127.0.0.1:9"), "\n")
	if strings.Contains(env, "host-token") {
		t.Fatalf("host credential leaked into declared service: %q", env)
	}
}

func TestBindAuthServicesBindsGoproxyHandler(t *testing.T) {
	if !sandbox.SupportsManagedNetworkProxy() {
		t.Skip("process session channels are unsupported")
	}
	backend := &recordingProtocolBackend{}
	service, _, err := bindAuthServices(&buildState{
		config: configBuildState{execution: config.Execution{
			Environment: config.ExecutionEnvironment{
				AuthServices: []config.EnvironmentAuthService{testGoproxyService()},
			},
		}},
		platform: platformBuildState{backend: backend},
	})
	if err != nil {
		t.Fatal(err)
	}
	if service == nil || backend.handler == nil {
		t.Fatalf("service=%v handler=%v", service, backend.handler)
	}
}

func TestBindAuthServicesFailsClosedWithoutSessionChannel(t *testing.T) {
	if sandbox.SupportsManagedNetworkProxy() {
		t.Skip("this platform allocates process session channels")
	}
	_, _, err := bindAuthServices(&buildState{
		config: configBuildState{execution: config.Execution{
			Environment: config.ExecutionEnvironment{
				AuthServices: []config.EnvironmentAuthService{testGoproxyService()},
			},
		}},
	})
	if err == nil ||
		!strings.Contains(err.Error(), environment.CategoryBackendCapabilityUnsupported) {
		t.Fatalf("unsupported platform error = %v", err)
	}
}

func TestBindHostGoproxyAuthRecordsMissingCredentialFact(t *testing.T) {
	if !sandbox.SupportsManagedNetworkProxy() {
		t.Skip("process session channels are unsupported")
	}
	service, report, err := bindAuthServices(&buildState{
		platform: platformBuildState{
			backend:          &recordingProtocolBackend{},
			hostGoproxyValue: "https://goproxy.corp.example",
			hostNetrcPath:    "/no/such/netrc",
		},
	})
	if err != nil || service != nil {
		t.Fatalf("unbound service = %v err=%v", service, err)
	}
	facts := report.Facts()
	if len(facts) != 1 {
		t.Fatalf("bind facts = %+v, want exactly one", facts)
	}
	fact := facts[0]
	if fact.Category != environment.CategoryCredentialUnavailable ||
		fact.RequiredAction != environment.ActionBindCredential ||
		fact.Resource != "goproxy.corp.example" {
		t.Fatalf("bind fact = %+v", fact)
	}
}

func TestBindHostGoproxyAuthRecordsUnbindableUpstreamFact(t *testing.T) {
	if !sandbox.SupportsManagedNetworkProxy() {
		t.Skip("process session channels are unsupported")
	}
	service, report, err := bindAuthServices(&buildState{
		platform: platformBuildState{
			backend:          &recordingProtocolBackend{},
			hostGoproxyValue: "socks5://goproxy.corp.example",
		},
	})
	if err != nil || service != nil {
		t.Fatalf("unbound service = %v err=%v", service, err)
	}
	facts := report.Facts()
	if len(facts) != 1 || facts[0].Category != environment.CategoryEnvironmentResourceUnavailable {
		t.Fatalf("bind facts = %+v", facts)
	}
}

func TestBindHostGoproxyAuthOwesNoFactForDefaultPublicProxy(t *testing.T) {
	if !sandbox.SupportsManagedNetworkProxy() {
		t.Skip("process session channels are unsupported")
	}
	service, report, err := bindAuthServices(&buildState{
		platform: platformBuildState{
			backend:          &recordingProtocolBackend{},
			hostGoproxyValue: "https://proxy.golang.org,direct",
			hostNetrcPath:    "/no/such/netrc",
		},
	})
	if err != nil || service != nil {
		t.Fatalf("public proxy service = %v err=%v", service, err)
	}
	if facts := report.Facts(); len(facts) != 0 {
		t.Fatalf("public proxy owed no facts: %+v", facts)
	}
}

func testGoproxyService() config.EnvironmentAuthService {
	return config.EnvironmentAuthService{
		Protocol: "goproxy",
		Upstream: "https://goproxy.example",
		Prefixes: []string{"example.com/qcode/"},
		Credential: config.EnvironmentAuthCredential{
			Kind: "env", Name: "QCODE_TEST_GOPROXY_TOKEN",
		},
	}
}

type recordingProtocolBackend struct {
	sandbox.Backend
	handler http.Handler
}

func (b *recordingProtocolBackend) BindProtocolHandler(handler http.Handler) {
	b.handler = handler
}

func (b *recordingProtocolBackend) Capability() sandbox.Capability {
	return sandbox.Capability{Available: true, ManagedProxy: true}
}
