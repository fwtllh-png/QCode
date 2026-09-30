package httpclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestClientMakesOneAttemptForRetryableHTTPStatusMatrix(t *testing.T) {
	statuses := []int{
		http.StatusRequestTimeout,
		http.StatusTooEarly,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
	}
	for _, status := range statuses {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(
				writer http.ResponseWriter,
				_ *http.Request,
			) {
				attempts.Add(1)
				writer.WriteHeader(status)
			}))
			defer server.Close()
			client := testClient()
			_, err := client.Stream(
				t.Context(),
				testRequest(t, server.URL, model.ProtocolOpenAIChat),
			)
			var problem *protocol.Problem
			if !errors.As(err, &problem) || !problem.Retryable {
				t.Fatalf("error = %v, want retryable problem", err)
			}
			if attempts.Load() != 1 {
				t.Fatalf("attempts = %d, want 1", attempts.Load())
			}
		})
	}
}

func TestClientRetainsRetryAfterMetadataWithoutSleeping(t *testing.T) {
	var attempts atomic.Int32
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		keys = append(keys, request.Header.Get("Idempotency-Key"))
		writer.Header().Set("Retry-After", "60")
		writer.Header().Set("RateLimit-Limit", "100")
		writer.Header().Set("RateLimit-Remaining", "0")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := testClient()
	_, err := client.Stream(t.Context(), testRequest(t, server.URL, model.ProtocolOpenAIChat))

	var problem *protocol.Problem
	if !errors.As(err, &problem) {
		t.Fatalf("error = %v", err)
	}
	if problem.HTTPStatus != http.StatusTooManyRequests ||
		problem.RateLimit == nil ||
		problem.RateLimit.Limit != "100" ||
		problem.RateLimit.Remaining != "0" ||
		problem.RateLimit.RetryAfterMS != 60000 {
		t.Fatalf("problem = %+v", problem)
	}
	if attempts.Load() != 1 || len(keys) != 1 || keys[0] == "" {
		t.Fatalf("idempotency keys = %v", keys)
	}
}

func TestClientDerivesSharedCooldownFromRateLimitedRequest(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		attempts.Add(1)
		time.Sleep(20 * time.Millisecond)
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := testClient()
	request := testRequest(t, server.URL, model.ProtocolOpenAIChat)
	_, err := client.Stream(t.Context(), request)
	var problem *protocol.Problem
	var failure *provider.Failure
	if !errors.As(err, &problem) ||
		!errors.As(err, &failure) ||
		problem.RateLimit != nil ||
		failure.RetryAfterMS != 0 {
		t.Fatalf("rate limit error = %#v, failure = %#v", problem, failure)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if _, err := client.Stream(ctx, request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cooldown error = %v, want deadline exceeded", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("provider attempts = %d, want shared cooldown before retry", attempts.Load())
	}
	if cooldown := client.RouteCooldown(request.Route); cooldown <= 0 {
		t.Fatalf("route cooldown = %s, want remaining governor wait", cooldown)
	}
}

func TestClientDoesNotRetryPermanentClientError(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(writer, "invalid model")
	}))
	defer server.Close()

	client := testClient()
	_, err := client.Stream(t.Context(), testRequest(t, server.URL, model.ProtocolOpenAIChat))
	if !protocol.IsCode(err, protocol.CodeInvalidArgument) {
		t.Fatalf("error = %v, code = %q", err, protocol.CodeOf(err))
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts.Load())
	}
}

func TestClientDoesNotRetryNonIdempotentRequest(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		writer.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	client := testClient()
	request := testRequest(t, server.URL, model.ProtocolOpenAIChat)
	request.Idempotent = false
	if _, err := client.Stream(t.Context(), request); err == nil {
		t.Fatal("Stream() error = nil")
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d", attempts.Load())
	}
}
