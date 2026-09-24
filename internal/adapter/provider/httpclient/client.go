package httpclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerratelimit "github.com/fwtllh-png/QCode/internal/adapter/provider/ratelimit"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
	"github.com/fwtllh-png/QCode/internal/observability/telemetry"
	"github.com/fwtllh-png/QCode/internal/observability/tracecontext"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/egress"
)

// MaxErrorBodyBytes bounds provider HTTP error diagnostics to 16 KiB.
// Model discovery and capability probes share this transport limit.
const MaxErrorBodyBytes = 16 << 10

var requestSequence atomic.Uint64

type CredentialResolver interface {
	Resolve(context.Context, model.CredentialRef) (string, error)
}
type Client struct {
	HTTP              *http.Client
	Credentials       CredentialResolver
	Egress            *egress.Gate
	Metrics           *telemetry.Metrics
	IdleTimeout       time.Duration
	MaxConcurrent     int
	RequestsPerSecond float64
	deadlines         DeadlineConfig

	mu     sync.Mutex
	active int
	limits providerratelimit.Controller
	health Health
}
type Health struct {
	Healthy             bool      `json:"healthy"`
	Active              int       `json:"active"`
	ConsecutiveFailures uint64    `json:"consecutive_failures"`
	LastError           string    `json:"last_error,omitempty"`
	UpdatedAt           time.Time `json:"updated_at"`
}

func New() *Client {
	return withConnectionTimeout(&Client{
		HTTP:          &http.Client{},
		Credentials:   DefaultCredentials(),
		IdleTimeout:   60 * time.Second,
		MaxConcurrent: 8,
		health:        Health{Healthy: true, UpdatedAt: time.Now()},
	}, 2*time.Minute)
}
func (c *Client) httpClient() *http.Client {
	base := c.HTTP
	if base == nil {
		base = http.DefaultClient
	}
	if c.Egress == nil {
		return base
	}
	return egress.WrapClient(base, c.Egress)
}
func (c *Client) RouteCooldown(route model.ReadyRoute) time.Duration {
	return c.limits.Remaining(providerratelimit.Key(route))
}

func (c *Client) Execute(
	ctx context.Context,
	request provider.ModelRequest,
	call providerwire.PreparedCall,
	adapter providerwire.Adapter,
) (provider.Stream, error) {
	requestContext, requestCancel, credential, cooldownWait, err := c.begin(ctx, request.Route)
	if err != nil {
		return nil, err
	}
	release := true
	defer func() {
		if release {
			requestCancel()
			c.release()
		}
	}()
	httpClient := c.httpClient()
	httpRequest, err := http.NewRequestWithContext(
		requestContext,
		call.Method,
		joinEndpoint(request.Route.Endpoint(), call.Path),
		bytes.NewReader(call.Body),
	)
	if err != nil {
		return nil, protocol.NewProblem(
			protocol.CodeInvalidArgument, "create provider request", false, err,
		)
	}
	applyHeaders(httpRequest, call, credential)
	tracecontext.InjectHTTP(requestContext, httpRequest.Header)
	transportRequestID := requestKey(call.Body)
	httpRequest.Header.Set("Idempotency-Key", transportRequestID)
	phase := newRequestPhase()
	httpRequest = httpRequest.WithContext(httptrace.WithClientTrace(
		httpRequest.Context(),
		phase.trace(),
	))
	response, err := httpClient.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return nil, operationContextFault(
				ctx.Err(),
				request.LogicalRequestID,
			)
		}
		problem := providerTransportFault(
			err,
			request,
			transportRequestID,
			phase.stage(),
			c.deadlineFor(phase.stage()),
		)
		c.recordFailure(problem)
		return nil, problem
	}
	stream, transferred, err := c.openResponse(
		response,
		request,
		call,
		adapter,
		transportRequestID, requestCancel, providerratelimit.Key(request.Route),
		cooldownWait,
	)
	if transferred {
		release = false
	}
	return stream, err
}
func (c *Client) begin(
	ctx context.Context,
	route model.ReadyRoute,
) (context.Context, context.CancelFunc, string, time.Duration, error) {
	cooldownWait, err := c.limits.Wait(
		ctx,
		providerratelimit.Key(route),
		c.RequestsPerSecond,
	)
	if err != nil {
		return nil, nil, "", cooldownWait, err
	}
	if err := c.acquire(ctx); err != nil {
		return nil, nil, "", cooldownWait, err
	}
	requestContext, cancel := context.WithCancel(ctx)
	fail := func(err error) (context.Context, context.CancelFunc, string, time.Duration, error) {
		cancel()
		c.release()
		return nil, nil, "", cooldownWait, err
	}
	resolver := c.Credentials
	if resolver == nil {
		resolver = DefaultCredentials()
	}
	credential, err := resolver.Resolve(requestContext, route.Credential())
	if err != nil {
		return fail(protocol.NewProblem(
			protocol.CodeUnavailable, "resolve provider credential", false, err,
		))
	}
	return requestContext, cancel, credential, cooldownWait, nil
}

func (c *Client) acquire(ctx context.Context) error {
	for {
		c.mu.Lock()
		maximum := c.MaxConcurrent
		if maximum <= 0 {
			maximum = 8
		}
		if c.active < maximum {
			c.active++
			c.health.Active = c.active
			c.health.UpdatedAt = time.Now()
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}
func (c *Client) release() {
	c.mu.Lock()
	if c.active > 0 {
		c.active--
	}
	c.health.Active = c.active
	c.health.UpdatedAt = time.Now()
	c.mu.Unlock()
}
func (c *Client) recordSuccess() {
	c.mu.Lock()
	c.health.Healthy = true
	c.health.ConsecutiveFailures = 0
	c.health.LastError = ""
	c.health.UpdatedAt = time.Now()
	c.mu.Unlock()
}
func (c *Client) recordFailure(err error) {
	c.mu.Lock()
	c.health.ConsecutiveFailures++
	c.health.Healthy = c.health.ConsecutiveFailures < 3
	c.health.LastError = errorString(err)
	c.health.UpdatedAt = time.Now()
	c.mu.Unlock()
}
func (c *Client) Health() Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.health
}
func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
func joinEndpoint(endpoint, path string) string {
	return strings.TrimRight(endpoint, "/") + path
}
func boundedBody(body io.ReadCloser) string {
	defer body.Close()
	data, _ := io.ReadAll(io.LimitReader(body, MaxErrorBodyBytes))
	return strings.TrimSpace(string(data))
}
func retryableTransportError(err error) bool {
	var certificateError *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameError x509.HostnameError
	var recordHeaderError tls.RecordHeaderError
	if errors.As(err, &certificateError) ||
		errors.As(err, &unknownAuthority) ||
		errors.As(err, &hostnameError) ||
		errors.As(err, &recordHeaderError) {
		return false
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return !dnsError.IsNotFound && (dnsError.IsTimeout || dnsError.IsTemporary)
	}
	if errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}
func requestKey(body []byte) string {
	digest := sha256.Sum256(body)
	return fmt.Sprintf("qcode-%x-%d", digest[:8], requestSequence.Add(1))
}

var _ providerwire.Transport = (*Client)(nil)
