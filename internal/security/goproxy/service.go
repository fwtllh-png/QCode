// Package goproxy is the GOPROXY protocol auth service. The execution
// process only sees a session-local origin; this package holds the
// upstream binding and credential and never puts the secret into the
// process environment.
package goproxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/environment"
)

const Source = "auth_service"

// DialTimeout, TLSHandshakeTimeout, and ClientTimeout are the public
// safety ceilings for one GOPROXY upstream fetch. A hung artifact
// source must not hold a process session indefinitely. They are not
// configuration knobs.
const (
	DialTimeout         = 10 * time.Second
	TLSHandshakeTimeout = 10 * time.Second
	ClientTimeout       = 30 * time.Second
)

type Resolver func(ctx context.Context, kind, name string) (string, error)

type Binding struct {
	Upstream   string
	Prefixes   []string
	Credential CredentialRef
	// UpstreamTimeout bounds one upstream fetch. Zero selects the public
	// ClientTimeout ceiling; negative values are rejected so a hung
	// artifact source can never hold a process session indefinitely.
	UpstreamTimeout time.Duration
}

type CredentialRef struct {
	Kind string
	Name string
}

type Service struct {
	binding  Binding
	upstream *url.URL
	resolve  Resolver
	client   *http.Client
	cache    *responseCache

	mu    sync.Mutex
	facts []environment.Fact
}

func New(binding Binding, resolve Resolver, transport http.RoundTripper) (*Service, error) {
	if err := ValidateBinding(binding); err != nil {
		return nil, err
	}
	upstream, err := url.Parse(strings.TrimSpace(binding.Upstream))
	if err != nil {
		return nil, err
	}
	if resolve == nil {
		return nil, errors.New("goproxy credential resolver is required")
	}
	timeout := binding.UpstreamTimeout
	if timeout == 0 {
		timeout = ClientTimeout
	}
	if transport == nil {
		transport = &http.Transport{
			Proxy:           nil,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			DialContext: (&net.Dialer{
				Timeout: DialTimeout,
			}).DialContext,
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: TLSHandshakeTimeout,
		}
	}
	return &Service{
		binding:  binding,
		upstream: upstream,
		resolve:  resolve,
		cache:    newResponseCache(CacheBudgetBytes),
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
		},
	}, nil
}

func ValidateBinding(binding Binding) error {
	if strings.TrimSpace(binding.Upstream) == "" {
		return errors.New("goproxy upstream is required")
	}
	if binding.UpstreamTimeout < 0 {
		return errors.New("goproxy upstream timeout must not be negative")
	}
	parsed, err := url.Parse(strings.TrimSpace(binding.Upstream))
	if err != nil || parsed.Host == "" {
		return errors.New("goproxy upstream is invalid")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return errors.New("goproxy upstream must be an origin, not a path")
	}
	if parsed.User != nil {
		return errors.New("goproxy upstream must not embed credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("goproxy upstream must not include a query or fragment")
	}
	host := parsed.Hostname()
	loopback := host == "127.0.0.1" || host == "localhost" || host == "::1"
	if parsed.Scheme != "https" && !loopback {
		return errors.New("goproxy upstream must use https")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return errors.New("goproxy upstream scheme is invalid")
	}
	if len(binding.Prefixes) == 0 {
		return errors.New("goproxy module prefixes are required")
	}
	for _, prefix := range binding.Prefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" || prefix == "/" || prefix == "." ||
			strings.Contains(prefix, "..") || strings.ContainsAny(prefix, "?\x00") {
			return fmt.Errorf("goproxy prefix %q is invalid", prefix)
		}
	}
	if strings.TrimSpace(binding.Credential.Kind) == "" ||
		strings.TrimSpace(binding.Credential.Name) == "" {
		return errors.New("goproxy credential reference is required")
	}
	switch binding.Credential.Kind {
	case "env", "file", "keyring", CredentialKindHost:
	default:
		return fmt.Errorf("goproxy credential kind %q is not available", binding.Credential.Kind)
	}
	return nil
}

func (s *Service) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		http.Error(writer, "goproxy only serves GET", http.StatusMethodNotAllowed)
		return
	}
	if request.URL != nil && (request.URL.RawQuery != "" || request.URL.Fragment != "") {
		http.Error(writer, "goproxy path must not include a query or fragment", http.StatusBadRequest)
		return
	}
	parsed, err := ParseRequest(request.URL.Path)
	if err != nil {
		http.Error(writer, "goproxy path is not a module proxy request", http.StatusBadRequest)
		return
	}
	// Sumdb payloads are self-verifying (the go client checks them against
	// its own sumdb keys), so the trust boundary is the fixed upstream host;
	// the module-prefix scope does not apply to /sumdb/ paths.
	if parsed.Kind != KindSumdb && !MatchPrefix(parsed.Module, s.binding.Prefixes) {
		s.record(environment.Fact{
			Source:         Source,
			Category:       environment.CategoryTrustValidationFailed,
			Resource:       parsed.Module,
			Detail:         "module is outside the bound GOPROXY prefix scope",
			HasSideEffects: false,
		})
		http.Error(writer, "module is outside the bound GOPROXY scope", http.StatusForbidden)
		return
	}
	cacheKey := request.URL.Path
	if request.Method == http.MethodGet {
		if entry, hit := s.cache.get(cacheKey, time.Now()); hit {
			writeCachedEntry(writer, entry)
			return
		}
	}
	secret, err := s.resolve(request.Context(), s.binding.Credential.Kind, s.binding.Credential.Name)
	if err != nil || strings.TrimSpace(secret) == "" {
		s.record(environment.Fact{
			Source:         Source,
			Category:       environment.CategoryCredentialUnavailable,
			RequiredAction: environment.ActionBindCredential,
			Resource:       s.binding.Credential.Name,
			Detail:         "bound GOPROXY credential is unavailable",
			HasSideEffects: false,
		})
		http.Error(writer, "goproxy credential is unavailable", http.StatusBadGateway)
		return
	}
	target := strings.TrimRight(s.upstream.String(), "/") + "/" + parsed.Escaped
	upstream, err := url.Parse(target)
	if err != nil || upstream.Hostname() != s.upstream.Hostname() {
		s.record(environment.Fact{
			Source:   Source,
			Category: environment.CategoryTrustValidationFailed,
			Resource: s.upstream.Host,
			Detail:   "goproxy request would leave the bound upstream host",
		})
		http.Error(writer, "goproxy upstream host is fixed", http.StatusForbidden)
		return
	}
	outbound, err := http.NewRequestWithContext(request.Context(), http.MethodGet, target, nil)
	if err != nil {
		http.Error(writer, "goproxy upstream request failed", http.StatusBadGateway)
		return
	}
	applyCredential(outbound, secret)
	response, err := s.roundTrip(outbound, secret)
	if err != nil {
		if errors.Is(err, errRedirectHost) {
			s.record(environment.Fact{
				Source:   Source,
				Category: environment.CategoryTrustValidationFailed,
				Resource: s.upstream.Host,
				Detail:   "goproxy upstream redirected outside the bound host",
			})
			http.Error(writer, "goproxy redirect left the bound upstream", http.StatusBadGateway)
			return
		}
		s.record(environment.Fact{
			Source:   Source,
			Category: environment.CategoryUpstreamUnavailable,
			Resource: s.upstream.Host,
			Detail:   "goproxy upstream is unavailable",
		})
		http.Error(writer, "goproxy upstream is unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	s.recordUpstreamStatus(response.StatusCode, parsed)
	buffered := s.rememberUpstreamBody(cacheKey, request.Method, response, time.Now())
	writeFilteredHeader(writer, response.Header)
	writer.WriteHeader(response.StatusCode)
	if request.Method == http.MethodHead {
		return
	}
	if buffered != nil {
		_, _ = writer.Write(buffered)
		return
	}
	_, _ = io.Copy(writer, response.Body)
}

// recordUpstreamStatus explains non-2xx upstream answers as Facts so the
// model can distinguish an upstream negation from a sandbox failure instead
// of probing blind.
func (s *Service) recordUpstreamStatus(status int, parsed Request) {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		s.record(environment.Fact{
			Source:         Source,
			Category:       environment.CategoryCredentialRejected,
			RequiredAction: environment.ActionBindCredential,
			Resource:       s.upstream.Host,
			Detail:         "goproxy upstream rejected the bound credential",
			HasSideEffects: false,
		})
	case negationStatus(status):
		resource := parsed.Module
		if parsed.Version != "" {
			resource += "@" + parsed.Version
		}
		s.record(environment.Fact{
			Source:   Source,
			Category: environment.CategoryUpstreamUnavailable,
			Resource: resource,
			Detail: fmt.Sprintf(
				"upstream answered HTTP %d: this proxy does not serve the requested version; "+
					"it is an upstream negation, not a sandbox failure",
				status,
			),
			HasSideEffects: false,
		})
	case status >= 500:
		s.record(environment.Fact{
			Source:   Source,
			Category: environment.CategoryUpstreamUnavailable,
			Resource: s.upstream.Host,
			Detail:   fmt.Sprintf("goproxy upstream answered HTTP %d", status),
		})
	}
}

func writeFilteredHeader(writer http.ResponseWriter, header http.Header) {
	for name, values := range header {
		if hopHeader(name) {
			continue
		}
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
}

func writeCachedEntry(writer http.ResponseWriter, entry cacheEntry) {
	writeFilteredHeader(writer, entry.header)
	writer.WriteHeader(entry.status)
	if entry.body != nil {
		_, _ = writer.Write(entry.body)
	}
}

func (s *Service) BoundHost() string {
	if s == nil || s.upstream == nil {
		return ""
	}
	return s.upstream.Hostname()
}

func (s *Service) RewriteProcessEnv(env []string, listen string) []string {
	if s == nil || strings.TrimSpace(listen) == "" {
		return env
	}
	rewritten := make([]string, 0, len(env)+1)
	replaced := false
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name != "GOPROXY" {
			rewritten = append(rewritten, entry)
			continue
		}
		rewritten = append(rewritten, "GOPROXY="+listen)
		replaced = true
	}
	if !replaced {
		rewritten = append(rewritten, "GOPROXY="+listen)
	}
	return rewritten
}

func (s *Service) Facts() []environment.Fact {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]environment.Fact(nil), s.facts...)
}

func (s *Service) TakeFacts() []environment.Fact {
	s.mu.Lock()
	defer s.mu.Unlock()
	facts := s.facts
	s.facts = nil
	return facts
}

func (s *Service) record(fact environment.Fact) {
	s.mu.Lock()
	s.facts = append(s.facts, fact)
	s.mu.Unlock()
}

var errRedirectHost = errors.New("goproxy redirect left the bound upstream host")

func (s *Service) roundTrip(request *http.Request, secret string) (*http.Response, error) {
	client := *s.client
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return nil
		}
		if req.URL.Hostname() != s.upstream.Hostname() {
			return errRedirectHost
		}
		req.Header.Del("Authorization")
		applyCredential(req, secret)
		return nil
	}
	return client.Do(request)
}

func applyCredential(request *http.Request, secret string) {
	secret = strings.TrimSpace(secret)
	if secret == "" || request == nil {
		return
	}
	if strings.HasPrefix(secret, "Basic ") || strings.HasPrefix(secret, "Bearer ") {
		request.Header.Set("Authorization", secret)
		return
	}
	user, pass, ok := strings.Cut(secret, ":")
	if ok {
		request.SetBasicAuth(user, pass)
		return
	}
	request.SetBasicAuth("", secret)
}

func hopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailers", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}
