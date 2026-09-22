package wire

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	adapterenv "github.com/fwtllh-png/QCode/internal/adapter/environment"
	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/httpclient"
	"github.com/fwtllh-png/QCode/internal/environment"
	"github.com/fwtllh-png/QCode/internal/platform/envprobe"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/goproxy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

const authServiceFingerprint = "auth_service: bound"

func environmentFingerprint(service *goproxy.Service) []string {
	lines := append([]string(nil), envprobe.Fingerprint()...)
	if service != nil {
		lines = append(lines, authServiceFingerprint)
	}
	return lines
}

func bindAuthServices(state *buildState) (*goproxy.Service, *goproxy.BindReport, error) {
	if state == nil {
		return nil, nil, nil
	}
	services := state.config.execution.Environment.AuthServices
	if len(services) == 0 {
		return bindHostGoproxyAuth(state)
	}
	if len(services) > 1 {
		return nil, nil, errors.New("only one goproxy auth service is supported")
	}
	declared := services[0]
	if declared.Protocol != goproxy.Protocol {
		return nil, nil, fmt.Errorf("auth service protocol %q is not available", declared.Protocol)
	}
	if !sandbox.SupportsManagedNetworkProxy() {
		return nil, nil, fmt.Errorf(
			"goproxy auth service requires a process session channel: %s",
			environment.CategoryBackendCapabilityUnsupported,
		)
	}
	credentials := httpclient.DefaultCredentials()
	service, err := goproxy.New(goproxy.Binding{
		Upstream: declared.Upstream,
		Prefixes: append([]string(nil), declared.Prefixes...),
		Credential: goproxy.CredentialRef{
			Kind: declared.Credential.Kind,
			Name: declared.Credential.Name,
		},
		UpstreamTimeout: time.Duration(declared.UpstreamTimeoutMS) * time.Millisecond,
	}, resolveAuthCredential(credentials), nil)
	if err != nil {
		return nil, nil, err
	}
	egress.BindProtocolHandler(state.platform.backend, service)
	return service, nil, nil
}

func bindHostGoproxyAuth(
	state *buildState,
) (*goproxy.Service, *goproxy.BindReport, error) {
	report := &goproxy.BindReport{}
	if state.platform.backend == nil ||
		!sandbox.SupportsManagedNetworkProxy() ||
		state.options.SkipHostGoproxyAuth {
		return nil, report, nil
	}
	value := strings.TrimSpace(state.platform.hostGoproxyValue)
	netrcPath := state.platform.hostNetrcPath
	if value == "" {
		discovered, err := adapterenv.HostProxyEnv(context.Background(), nil)
		if err != nil {
			// A missing go binary or an absent GOPROXY is already reported
			// by the go preparer; nothing was promised, so nothing is owed.
			return nil, report, nil
		}
		value = discovered
	}
	if netrcPath == "" {
		netrcPath = goproxy.DefaultNetrcPath()
	}
	binding, ok := goproxy.InspectHost(value)
	if !ok {
		if strings.TrimSpace(value) != "" && !isDefaultPublicGoproxy(value) {
			report.Record(environment.Fact{
				Source:         goproxy.Source,
				Category:       environment.CategoryEnvironmentResourceUnavailable,
				RequiredAction: environment.ActionApproveHostConfig,
				Resource:       "goproxy-auth",
				Detail: "host GOPROXY is set but is not a bindable http(s) " +
					"upstream; private module downloads will not receive " +
					"credentials. Configure execution.environment.auth_services.",
			})
		}
		return nil, report, nil
	}
	if _, ok := goproxy.LookupHostSecret(value, netrcPath); !ok {
		// The Go toolchain default proxy is public and needs no credential;
		// only a non-default upstream owes an explanation when unbound.
		if !isDefaultPublicGoproxy(value) {
			report.Record(environment.Fact{
				Source:         goproxy.Source,
				Category:       environment.CategoryCredentialUnavailable,
				RequiredAction: environment.ActionBindCredential,
				Resource:       binding.Credential.Name,
				Detail: "host GOPROXY credential is not present in netrc; " +
					"module downloads through this proxy will fail with 401 " +
					"when it requires authentication. Add the credential to " +
					"netrc or configure execution.environment.auth_services.",
			})
		}
		return nil, report, nil
	}
	service, err := goproxy.New(binding, func(context.Context, string, string) (string, error) {
		secret, found := goproxy.LookupHostSecret(value, netrcPath)
		if !found || strings.TrimSpace(secret) == "" {
			return "", errors.New("host GOPROXY credential is unavailable")
		}
		return secret, nil
	}, nil)
	if err != nil {
		return nil, report, err
	}
	egress.BindProtocolHandler(state.platform.backend, service)
	return service, report, nil
}

func resolveAuthCredential(
	credentials httpclient.Credentials,
) goproxy.Resolver {
	return func(ctx context.Context, kind, name string) (string, error) {
		return credentials.Resolve(ctx, model.CredentialRef{Kind: kind, Name: name})
	}
}

// isDefaultPublicGoproxy reports whether the first GOPROXY item is the Go
// toolchain's documented default module proxy. That proxy is public, so an
// absent credential is not a binding failure and owes no Fact.
func isDefaultPublicGoproxy(value string) bool {
	items := goproxy.SplitProxyList(value)
	if len(items) == 0 {
		return true
	}
	parsed, err := url.Parse(items[0])
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Scheme, "https") &&
		strings.EqualFold(parsed.Hostname(), "proxy.golang.org")
}
