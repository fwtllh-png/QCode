package goproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/environment"
)

func TestSuccessfulResponsesReplayFromCache(t *testing.T) {
	hits := 0
	upstream := newCountingUpstream(&hits, http.StatusOK, "body-data")
	t.Cleanup(upstream.Close)
	service := newTestService(t, upstream.URL, "user:secret-token")
	path := "/example.com/qcode/testmod/@v/v1.2.3.info"
	for attempt := 0; attempt < 3; attempt++ {
		response := serve(service, path)
		if response.StatusCode != http.StatusOK || response.Body != "body-data" {
			t.Fatalf("attempt %d response = %d %q", attempt, response.StatusCode, response.Body)
		}
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, cached responses must not refetch", hits)
	}
}

func TestNegationCachesBrieflyAndRecordsFact(t *testing.T) {
	hits := 0
	upstream := newCountingUpstream(&hits, http.StatusGone, "")
	t.Cleanup(upstream.Close)
	service := newTestService(t, upstream.URL, "user:secret-token")
	path := "/example.com/qcode/testmod/@v/v2.0.0.info"
	for attempt := 0; attempt < 2; attempt++ {
		response := serve(service, path)
		if response.StatusCode != http.StatusGone {
			t.Fatalf("attempt %d status = %d", attempt, response.StatusCode)
		}
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, negation must replay within the TTL", hits)
	}
	facts := service.TakeFacts()
	if len(facts) != 1 ||
		facts[0].Category != environment.CategoryUpstreamUnavailable ||
		facts[0].Resource != "example.com/qcode/testmod@v2.0.0" {
		t.Fatalf("negation facts = %+v", facts)
	}
	if !strings.Contains(facts[0].Detail, "not a sandbox failure") {
		t.Fatalf("negation detail = %q", facts[0].Detail)
	}
}

func TestSumdbRouteProxiesWithoutModulePrefixScope(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hits++
		if request.URL.Path != "/sumdb/sum.golang.org/support" {
			t.Fatalf("upstream path = %s", request.URL.Path)
		}
		_, _ = writer.Write([]byte("sumdb-data"))
	}))
	t.Cleanup(upstream.Close)
	service := newTestService(t, upstream.URL, "user:secret-token")
	response := serve(service, "/sumdb/sum.golang.org/support")
	if response.StatusCode != http.StatusOK || response.Body != "sumdb-data" {
		t.Fatalf("sumdb response = %d %q", response.StatusCode, response.Body)
	}
	if hits != 1 {
		t.Fatalf("sumdb upstream hits = %d", hits)
	}
	// Cached replay keeps the sumdb route working across commands.
	if replay := serve(service, "/sumdb/sum.golang.org/support"); replay.StatusCode != http.StatusOK {
		t.Fatalf("sumdb replay status = %d", replay.StatusCode)
	}
	if hits != 1 {
		t.Fatalf("sumdb replay hit upstream: hits = %d", hits)
	}
	for _, invalid := range []string{"/sumdb/", "/sumdb/only-name"} {
		if response := serve(service, invalid); response.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid sumdb path %q status = %d", invalid, response.StatusCode)
		}
	}
}

func TestUpstreamTimeoutBindingOverridesCeiling(t *testing.T) {
	service, err := New(Binding{
		Upstream: "https://goproxy.example",
		Prefixes: []string{"*"},
		Credential: CredentialRef{
			Kind: CredentialKindHost, Name: "goproxy.example",
		},
		UpstreamTimeout: 90 * time.Second,
	}, func(context.Context, string, string) (string, error) {
		return "token", nil
	}, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	if service.client.Timeout != 90*time.Second {
		t.Fatalf("timeout = %s", service.client.Timeout)
	}
	if _, err := New(Binding{
		Upstream: "https://goproxy.example",
		Prefixes: []string{"*"},
		Credential: CredentialRef{
			Kind: CredentialKindHost, Name: "goproxy.example",
		},
		UpstreamTimeout: -time.Second,
	}, func(context.Context, string, string) (string, error) {
		return "token", nil
	}, http.DefaultTransport); err == nil {
		t.Fatal("negative timeout accepted")
	}
}

func newCountingUpstream(hits *int, status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		*hits++
		writer.WriteHeader(status)
		if body != "" {
			_, _ = writer.Write([]byte(body))
		}
	}))
}

func TestOversizeUpstreamBodyStreamsPastCacheBudget(t *testing.T) {
	hits := 0
	body := strings.Repeat("a", 4096)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		hits++
		writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)
	service := newTestService(t, upstream.URL, "user:secret-token")
	// A tiny budget keeps the boundary test fast; production uses
	// CacheBudgetBytes and the overflow path is identical.
	service.cache = newResponseCache(64)
	path := "/example.com/qcode/testmod/@v/v1.2.3.zip"
	response := serve(service, path)
	if response.StatusCode != http.StatusOK || response.Body != body {
		t.Fatalf(
			"oversize body truncated: status=%d got=%d want=%d",
			response.StatusCode, len(response.Body), len(body),
		)
	}
	// The entry did not fit the budget, so a second fetch must hit upstream.
	replay := serve(service, path)
	if replay.StatusCode != http.StatusOK || replay.Body != body {
		t.Fatalf("replay status=%d len=%d", replay.StatusCode, len(replay.Body))
	}
	if hits != 2 {
		t.Fatalf("upstream hits = %d, oversize body must not cache", hits)
	}
}

func TestNegationReplayKeepsUpstreamBody(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		hits++
		writer.Header().Set("Content-Type", "text/plain")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte("module not found"))
	}))
	t.Cleanup(upstream.Close)
	service := newTestService(t, upstream.URL, "user:secret-token")
	path := "/example.com/qcode/testmod/@v/v9.9.9.info"
	first := serve(service, path)
	if first.StatusCode != http.StatusNotFound || first.Body != "module not found" {
		t.Fatalf("first negation = %d %q", first.StatusCode, first.Body)
	}
	second := serve(service, path)
	if second.StatusCode != http.StatusNotFound || second.Body != "module not found" {
		t.Fatalf("negation replay lost the body: %d %q", second.StatusCode, second.Body)
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, negation must replay within the TTL", hits)
	}
}
