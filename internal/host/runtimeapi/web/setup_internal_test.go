package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestSetupApplyReconfiguresReadyRuntime(t *testing.T) {
	identity, err := protocol.NewWorkspaceIdentity(
		"file:///workspace",
		"/workspace",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	var applied SetupRequest
	server, err := New(Options{
		Assets:       fstest.MapFS{"index.html": {Data: []byte("ok")}},
		ExpectedHost: "127.0.0.1:43210",
		Setup: &SetupOptions{
			WorkspaceRoot:     "/workspace",
			WorkspaceIdentity: identity,
			Apply: func(_ context.Context, request SetupRequest) error {
				applied = request
				return nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server.ready.Store(true)
	request := httptest.NewRequest(
		"POST",
		"http://127.0.0.1:43210/api/v1/setup/apply",
		strings.NewReader(`{
			"model":"deepseek-chat",
			"api_key":"secret",
			"base_url":"https://api.deepseek.com/v1",
			"protocol":"openai_chat"
		}`),
	)
	request.Header.Set("Idempotency-Key", "reconfigure")
	result, err := server.setupApply(request, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Model != "deepseek-chat" || applied.APIKey != "secret" ||
		applied.BaseURL != "https://api.deepseek.com/v1" {
		t.Fatalf("applied request = %+v", applied)
	}
	if ready := result.(SetupResult).Ready; !ready {
		t.Fatal("ready Runtime was not reported ready after reconfiguration")
	}
	response := httptest.NewRecorder()
	server.bootstrap(response, httptest.NewRequest(
		"GET",
		"http://127.0.0.1:43210/api/v1/bootstrap",
		nil,
	))
	var bootstrap bootstrapResponse
	if err := json.Unmarshal(response.Body.Bytes(), &bootstrap); err != nil {
		t.Fatal(err)
	}
	if bootstrap.SetupRequired {
		t.Fatalf("ready bootstrap = %+v", bootstrap)
	}
	var raw map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, exists := raw["setup_catalog"]; exists {
		t.Fatal("bootstrap must not advertise a setup provider catalog")
	}
}

func TestSetupProbeReturnsDetachedEndpointFacts(t *testing.T) {
	identity, err := protocol.NewWorkspaceIdentity(
		"file:///workspace",
		"/workspace",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	var probed SetupProbeRequest
	server, err := New(Options{
		Assets:       fstest.MapFS{"index.html": {Data: []byte("ok")}},
		ExpectedHost: "127.0.0.1:43210",
		Setup: &SetupOptions{
			WorkspaceRoot:     "/workspace",
			WorkspaceIdentity: identity,
			Apply:             func(context.Context, SetupRequest) error { return nil },
			Probe: func(
				_ context.Context,
				request SetupProbeRequest,
			) (SetupProbeResult, error) {
				probed = request
				return SetupProbeResult{
					Models: []SetupDiscoveredModel{{
						ID: "model-a", ContextTokens: 65_536,
						MaxOutputTokens: 4_096,
					}},
					Capabilities: SetupModelCapabilities{
						Streaming: boolPointer(true),
						ToolCalls: boolPointer(true),
					},
				}, nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		"POST",
		"http://127.0.0.1:43210/api/v1/setup/probe",
		strings.NewReader(`{
			"base_url":"https://models.example.com/v1",
			"protocol":"openai_chat",
			"model":"model-a",
			"api_key":"secret"
		}`),
	)
	result, err := server.setupProbe(request, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if probed.APIKey != "secret" ||
		result.(SetupProbeResult).Models[0].ContextTokens != 65_536 {
		t.Fatalf("probe request=%+v result=%+v", probed, result)
	}
}

func boolPointer(value bool) *bool { return &value }

// TestSetupProbeSurfacesProbeFailuresAsUnavailable pins the probe error
// mapping: a plain probe failure must surface its reason (credential,
// network, endpoint status), not collapse into an opaque internal error.
func TestSetupProbeSurfacesProbeFailuresAsUnavailable(t *testing.T) {
	server, err := New(Options{
		Assets:       fstest.MapFS{"index.html": {Data: []byte("ok")}},
		ExpectedHost: "127.0.0.1:43210",
		Setup: &SetupOptions{
			Apply: func(context.Context, SetupRequest) error { return nil },
			Probe: func(context.Context, SetupProbeRequest) (SetupProbeResult, error) {
				return SetupProbeResult{}, errors.New("model capability probe HTTP 401")
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		"POST",
		"http://127.0.0.1:43210/api/v1/setup/probe",
		strings.NewReader(`{"base_url":"https://models.example.com/v1","model":"model-a"}`),
	)
	_, err = server.setupProbe(request, Dependencies{})
	var problem *protocol.Problem
	if !errors.As(err, &problem) {
		t.Fatalf("probe error = %T %v, want a problem", err, err)
	}
	if problem.Code != protocol.CodeUnavailable ||
		!strings.Contains(problem.Message, "HTTP 401") {
		t.Fatalf("problem = %+v", problem)
	}
}
