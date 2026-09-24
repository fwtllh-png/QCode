package web

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type modelControllerFixture struct {
	added ModelMutationRequest
}

func (f *modelControllerFixture) ProbeModel(
	_ context.Context,
	model string,
) (SetupProbeResult, error) {
	return SetupProbeResult{
		Models: []SetupDiscoveredModel{{ID: model}},
		Capabilities: SetupModelCapabilities{
			Streaming: boolPointer(true),
			ToolCalls: boolPointer(true),
		},
	}, nil
}

func (f *modelControllerFixture) AddModel(
	_ context.Context,
	request ModelMutationRequest,
) (protocol.ModelCatalog, error) {
	f.added = request
	return protocol.ModelCatalog{Version: protocol.ModelCatalogVersion}, nil
}

func (f *modelControllerFixture) UpdateModel(
	context.Context,
	ModelMutationRequest,
) (protocol.ModelCatalog, error) {
	return protocol.ModelCatalog{Version: protocol.ModelCatalogVersion}, nil
}

func (f *modelControllerFixture) RemoveModel(
	context.Context,
	string,
) (protocol.ModelCatalog, error) {
	return protocol.ModelCatalog{Version: protocol.ModelCatalogVersion}, nil
}

func TestModelTestReportsProviderCatalogResult(t *testing.T) {
	server := &Server{capacity: defaultCapacity()}
	dependencies := Dependencies{
		DefaultProfile: protocol.SessionProfile{Provider: "fixture"},
		ModelProbe: func(_ context.Context, model string) (bool, error) {
			return model == "available-model", nil
		},
	}
	for _, test := range []struct {
		model, status string
	}{
		{model: "available-model", status: "available"},
		{model: "missing-model", status: "not_listed"},
	} {
		request := httptest.NewRequest(
			"POST",
			"/api/v1/model/test",
			strings.NewReader(`{"model":"`+test.model+`"}`),
		)
		result, err := server.modelTest(request, dependencies)
		if err != nil {
			t.Fatal(err)
		}
		if result.(ModelTestResult).Status != test.status {
			t.Fatalf("status = %q, want %q", result.(ModelTestResult).Status, test.status)
		}
	}
}

func TestModelAddUsesDedicatedController(t *testing.T) {
	controller := &modelControllerFixture{}
	server := &Server{capacity: defaultCapacity(), modelControl: controller}
	request := httptest.NewRequest(
		"POST",
		"/api/v1/model/add",
		strings.NewReader(`{
			"model":"model-b",
			"model_metadata":{
				"canonical_id":"model-b",
				"wire_id":"model-b",
				"context_tokens":65536,
				"max_output_tokens":8192,
				"capabilities":{"streaming":true,"tool_calls":true}
			}
		}`),
	)
	if _, err := server.modelAdd(request, Dependencies{}); err != nil {
		t.Fatal(err)
	}
	if controller.added.Model != "model-b" {
		t.Fatalf("added model = %+v", controller.added)
	}
}

type failingModelController struct{}

func (failingModelController) ProbeModel(
	context.Context, string,
) (SetupProbeResult, error) {
	return SetupProbeResult{}, errors.New("model capability probe HTTP 401")
}

func (failingModelController) AddModel(
	context.Context, ModelMutationRequest,
) (protocol.ModelCatalog, error) {
	return protocol.ModelCatalog{}, nil
}

func (failingModelController) UpdateModel(
	context.Context, ModelMutationRequest,
) (protocol.ModelCatalog, error) {
	return protocol.ModelCatalog{}, nil
}

func (failingModelController) RemoveModel(
	context.Context, string,
) (protocol.ModelCatalog, error) {
	return protocol.ModelCatalog{}, nil
}

// TestModelProbeSurfacesProbeFailuresAsUnavailable pins the probe error
// mapping: a plain probe failure must surface its reason, not collapse into
// an opaque internal Web API error.
func TestModelProbeSurfacesProbeFailuresAsUnavailable(t *testing.T) {
	server := &Server{
		capacity: defaultCapacity(), modelControl: failingModelController{},
	}
	request := httptest.NewRequest(
		"POST", "/api/v1/model/probe", strings.NewReader(`{"model":"glm-5.3-flash"}`),
	)
	_, err := server.modelProbe(request, Dependencies{})
	var problem *protocol.Problem
	if !errors.As(err, &problem) {
		t.Fatalf("probe error = %T %v, want a problem", err, err)
	}
	if problem.Code != protocol.CodeUnavailable ||
		!strings.Contains(problem.Message, "HTTP 401") {
		t.Fatalf("problem = %+v", problem)
	}
}
