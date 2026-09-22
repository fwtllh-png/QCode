package goproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/environment"
)

func TestEmptyCacheInfoFetchUsesBoundUpstream(t *testing.T) {
	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seenAuth = request.Header.Get("Authorization")
		if request.URL.Path != "/example.com/qcode/testmod/@v/v1.2.3.info" {
			t.Fatalf("upstream path = %s", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"Version":"v1.2.3","Time":"2020-01-02T03:04:05Z"}`))
	}))
	t.Cleanup(upstream.Close)
	service := newTestService(t, upstream.URL, "user:secret-token")
	response := serve(service, "/example.com/qcode/testmod/@v/v1.2.3.info")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.StatusCode, response.Body)
	}
	var info struct {
		Version string
		Time    string
	}
	if err := json.Unmarshal([]byte(response.Body), &info); err != nil ||
		info.Version != "v1.2.3" || info.Time != "2020-01-02T03:04:05Z" {
		t.Fatalf("info = %+v err=%v body=%s", info, err, response.Body)
	}
	if seenAuth == "" || strings.Contains(response.Body, "secret-token") {
		t.Fatalf("auth=%q body=%s", seenAuth, response.Body)
	}
}

func TestCredentialRejectedIsDistinctFromUnapprovedNetwork(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(upstream.Close)
	service := newTestService(t, upstream.URL, "user:bad")
	response := serve(service, "/example.com/qcode/testmod/@v/v1.2.3.info")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.StatusCode)
	}
	facts := service.TakeFacts()
	if len(facts) != 1 || facts[0].Category != environment.CategoryCredentialRejected {
		t.Fatalf("facts = %+v", facts)
	}
	if facts[0].Category == environment.CategoryNetworkTargetUnapproved {
		t.Fatal("credential rejection was classified as network denial")
	}
	if strings.Contains(facts[0].Detail, "bad") || strings.Contains(response.Body, "bad") {
		t.Fatalf("credential leaked: %+v body=%s", facts[0], response.Body)
	}
}

func TestPrefixAndRedirectKeepCredentialScope(t *testing.T) {
	var redirectedAuth string
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hits++
		if request.URL.Path == "/example.com/qcode/testmod/@v/v1.2.3.info" {
			http.Redirect(writer, request, "http://evil.example/stolen", http.StatusFound)
			return
		}
		redirectedAuth = request.Header.Get("Authorization")
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	service := newTestService(t, upstream.URL, "user:secret-token")
	outside := serve(service, "/evil.com/mod/@v/v1.2.3.info")
	if outside.StatusCode != http.StatusForbidden {
		t.Fatalf("outside status = %d", outside.StatusCode)
	}
	facts := service.TakeFacts()
	if len(facts) != 1 || facts[0].Category != environment.CategoryTrustValidationFailed {
		t.Fatalf("prefix facts = %+v", facts)
	}
	redirected := serve(service, "/example.com/qcode/testmod/@v/v1.2.3.info")
	if redirected.StatusCode != http.StatusBadGateway {
		t.Fatalf("redirect status = %d body=%s", redirected.StatusCode, redirected.Body)
	}
	if redirectedAuth != "" {
		t.Fatal("credential was forwarded to a different host")
	}
	redirectFacts := service.TakeFacts()
	if len(redirectFacts) != 1 ||
		redirectFacts[0].Category != environment.CategoryTrustValidationFailed {
		t.Fatalf("redirect facts = %+v", redirectFacts)
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d", hits)
	}
}

func TestBoundHostUsesUpstreamHostname(t *testing.T) {
	service := newTestService(t, "http://127.0.0.1:9", "user:secret-token")
	if host := service.BoundHost(); host != "127.0.0.1" {
		t.Fatalf("BoundHost = %q", host)
	}
}

func TestRewriteProcessEnvReplacesGoproxyAndOmitsSecret(t *testing.T) {
	service := newTestService(t, "http://127.0.0.1:9", "user:secret-token")
	env := service.RewriteProcessEnv(
		[]string{"GOPROXY=https://user:secret-token@proxy.example|direct", "HOME=/tmp"},
		"http://127.0.0.1:4321",
	)
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GOPROXY=http://127.0.0.1:4321") {
		t.Fatalf("env = %v", env)
	}
	if strings.Contains(joined, "secret-token") || strings.Contains(joined, "|direct") {
		t.Fatalf("secret or fallback leaked: %v", env)
	}
}

func TestClientTimeoutsArePublicContract(t *testing.T) {
	if DialTimeout != 10*time.Second ||
		TLSHandshakeTimeout != 10*time.Second ||
		ClientTimeout != 30*time.Second {
		t.Fatalf(
			"goproxy timeouts = dial %s tls %s client %s",
			DialTimeout, TLSHandshakeTimeout, ClientTimeout,
		)
	}
	service := newTestService(t, "http://127.0.0.1:9", "user:secret")
	if service.client == nil || service.client.Timeout != ClientTimeout {
		t.Fatalf("client timeout = %v", service.client.Timeout)
	}
}

func TestValidateBindingRejectsEmbeddedCredential(t *testing.T) {
	err := ValidateBinding(Binding{
		Upstream:   "https://user:secret@proxy.example",
		Prefixes:   []string{"example.com/"},
		Credential: CredentialRef{Kind: "env", Name: "TOKEN"},
	})
	if err == nil {
		t.Fatal("embedded upstream credential accepted")
	}
}

func newTestService(t *testing.T, upstream, secret string) *Service {
	t.Helper()
	service, err := New(Binding{
		Upstream:   upstream,
		Prefixes:   []string{"example.com/qcode/"},
		Credential: CredentialRef{Kind: "env", Name: "TEST_GOPROXY_TOKEN"},
	}, func(context.Context, string, string) (string, error) {
		return secret, nil
	}, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type recordedResponse struct {
	StatusCode int
	Body       string
}

func serve(service *Service, path string) recordedResponse {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil)
	service.ServeHTTP(recorder, request)
	result := recorder.Result()
	defer result.Body.Close()
	body, _ := io.ReadAll(result.Body)
	return recordedResponse{StatusCode: result.StatusCode, Body: string(body)}
}
