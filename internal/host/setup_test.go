package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/credential"
)

func TestWebSetupRequiresFourExplicitFields(t *testing.T) {
	valid := SetupRequest{
		Model: "vendor/model-v1", APIKey: "secret-value",
		BaseURL: "https://models.example.com/v1", Protocol: "openai_chat",
		ModelMetadata: testSetupMetadata("vendor/model-v1"),
	}
	selection, reference, err := resolveWebSetup(valid)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Provider != connectionID("https://models.example.com/v1") ||
		selection.ID != selection.Provider ||
		selection.Model != "vendor/model-v1" ||
		selection.BaseURL != "https://models.example.com/v1" ||
		selection.Protocol != "openai_chat" ||
		selection.MetadataProvenance != model.ProvenanceOperatorConfig {
		t.Fatalf("resolved selection = %+v", selection)
	}
	if reference.Kind != "" || reference.Name != "" {
		t.Fatalf("connection carries no credential reference: %+v", reference)
	}

	for name, mutate := range map[string]func(*SetupRequest){
		"missing base URL": func(r *SetupRequest) { r.BaseURL = "" },
		"missing model":    func(r *SetupRequest) { r.Model = "" },
		"missing api key":  func(r *SetupRequest) { r.APIKey = "" },
		"missing metadata": func(r *SetupRequest) { r.ModelMetadata = nil },
		"plain http base":  func(r *SetupRequest) { r.BaseURL = "http://example.com/v1" },
		"unknown protocol": func(r *SetupRequest) {
			r.Protocol = "anthropic"
		},
		"invalid model id": func(r *SetupRequest) {
			r.Model = "not a model id"
		},
	} {
		request := valid
		mutate(&request)
		if _, _, err := resolveWebSetup(request); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestWebSetupRejectsUnsupportedOrMalformedSelection(t *testing.T) {
	for name, data := range map[string]string{
		"missing version": `{"connections":[]}`,
		"zero version":    `{"version":0,"connections":[]}`,
		"v1":              `{"version":1,"connections":[]}`,
		"v2":              `{"version":2,"connections":[]}`,
		"future version":  `{"version":4,"connections":[]}`,
		"flat preset":     `{"version":1,"provider":"deepseek","model":"deepseek-chat","metadata_provenance":"bundled"}`,
		"flat custom":     `{"version":2,"provider":"openai-compatible","model":"old-model","base_url":"https://models.example.com/v1","protocol":"openai_chat"}`,
		"unknown field":   `{"version":3,"connections":[],"unknown":true}`,
		"wrong shape":     `{"version":3,"connections":{}}`,
		"trailing data":   `{"version":3,"connections":[]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			assertWebSetupRejectedWithoutRewrite(t, []byte(data))
		})
	}
	dataDir := t.TempDir()
	if _, found, err := loadWebSetupSelection(dataDir, "workspace"); err != nil || found {
		t.Fatalf("absent selection found=%v err=%v", found, err)
	}
	if err := saveWebSetupSelection(dataDir, "workspace", webSetupSelection{Version: webSetupVersion}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := loadWebSetupSelection(dataDir, "workspace"); err != nil || found {
		t.Fatalf("empty current selection found=%v err=%v", found, err)
	}
}

func TestWebSetupRejectsNonCanonicalConnections(t *testing.T) {
	valid, _, err := resolveWebSetup(SetupRequest{
		Model: "vendor/model-v1", APIKey: "secret-value",
		BaseURL: "https://models.example.com/v1", Protocol: "openai_chat",
		ModelMetadata: testSetupMetadata("vendor/model-v1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*webSetupConnection){
		"missing endpoint": func(c *webSetupConnection) { c.BaseURL = "" },
		"missing metadata": func(c *webSetupConnection) { c.Metadata = nil },
		"missing protocol": func(c *webSetupConnection) { c.Protocol = "" },
		"old custom provider": func(c *webSetupConnection) {
			c.Provider = customProviderID
		},
		"old preset identity": func(c *webSetupConnection) {
			c.ID, c.Provider = "openai", "openai"
		},
		"mismatched identity": func(c *webSetupConnection) { c.ID = "other-connection" },
		"invalid credential": func(c *webSetupConnection) {
			c.Credential = &credential.Reference{Kind: "env", Name: "MODEL_KEY"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			connection := cloneWebSetupConnection(valid)
			mutate(&connection)
			// An invalid entry must reject the whole file, even alongside a valid connection.
			selection := wrapConnection(valid)
			selection.Connections = append(selection.Connections, connection)
			data, err := json.Marshal(selection)
			if err != nil {
				t.Fatal(err)
			}
			assertWebSetupRejectedWithoutRewrite(t, data)
		})
	}
}

func TestWebSetupNonCanonicalErrorNamesConnectionAndField(t *testing.T) {
	valid, _, err := resolveWebSetup(SetupRequest{
		Model: "vendor/model-v1", APIKey: "secret-value",
		BaseURL: "https://models.example.com/v1", Protocol: "openai_chat",
		ModelMetadata: testSetupMetadata("vendor/model-v1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := cloneWebSetupConnection(valid)
	connection.ID, connection.Provider = "openai", "openai"
	expect := []string{
		`connection "openai" is not canonical`,
		`id is "openai"`,
		fmt.Sprintf("canonical %q", valid.Provider),
	}
	dataDir := t.TempDir()
	path := setupSelectionPath(dataDir, "workspace")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(wrapConnection(connection))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = loadWebSetupSelection(dataDir, "workspace")
	if err == nil {
		t.Fatal("non-canonical selection was accepted")
	}
	for _, fragment := range expect {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error %q does not contain %q", err, fragment)
		}
	}
}

func assertWebSetupRejectedWithoutRewrite(t *testing.T, data []byte) {
	t.Helper()
	dataDir := t.TempDir()
	path := setupSelectionPath(dataDir, "workspace")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := loadWebSetupSelection(dataDir, "workspace"); err == nil || found {
		t.Fatalf("invalid selection found=%v err=%v", found, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, after) {
		t.Fatal("loading an invalid selection rewrote the file")
	}
}

func TestWebSetupPersistsOnlyNonSecretSelection(t *testing.T) {
	inputMetadata := testSetupMetadata("vendor/model-v1")
	inputMetadata.WireID = "wire-model-v1"
	selection, reference, err := resolveWebSetup(SetupRequest{
		Model:    "vendor/model-v1",
		BaseURL:  "https://models.example.com/v1/",
		Protocol: "openai_responses", APIKey: "secret-value",
		ModelMetadata: inputMetadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reference.Kind != "" || reference.Name != "" ||
		selection.BaseURL != "https://models.example.com/v1" ||
		selection.MetadataProvenance != model.ProvenanceOperatorConfig {
		t.Fatalf("custom setup = %+v reference=%+v", selection, reference)
	}
	metadata := setupModelMetadata(selection).Descriptor
	if metadata == nil ||
		metadata.Limits.ContextTokens != 65_536 ||
		metadata.Limits.MaxOutputTokens != 8_192 ||
		metadata.WireID != "wire-model-v1" ||
		!metadata.Capabilities.Reasoning ||
		metadata.Capabilities.DefaultReasoningEffort != "high" ||
		metadata.MetadataProvenance.Limits != model.ProvenanceOperatorConfig {
		t.Fatalf("resolved custom metadata = %+v", metadata)
	}
	if wireID := setupWireModelID(selection, selection.Model); wireID != "wire-model-v1" {
		t.Fatalf("wire model id = %q", wireID)
	}
	for name, merge := range map[string]func(webSetupSelection, webSetupConnection) webSetupSelection{
		"setup apply":    mergeConnection,
		"connection add": mergeConnectionKeepDefault,
	} {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			created := merge(webSetupSelection{}, selection)
			if created.Version != webSetupVersion {
				t.Fatalf("new selection version=%d", created.Version)
			}
			if err := saveWebSetupSelection(dataDir, "workspace", created); err != nil {
				t.Fatal(err)
			}
			loaded, found, err := loadWebSetupSelection(dataDir, "workspace")
			if err != nil || !found || !reflect.DeepEqual(loaded, wrapConnection(selection)) {
				t.Fatalf("loaded setup = %+v found=%v err=%v", loaded, found, err)
			}
		})
	}
	selection.Credential = &credential.Reference{
		Kind: "keyring",
		Name: "web/setup/00000000000000000000000000000000",
	}
	selection.Models = []webSetupModel{{
		ID:       "vendor/model-v2",
		Metadata: *testSetupMetadata("vendor/model-v2"),
	}}
	dataDir := t.TempDir()
	if err := saveWebSetupSelection(dataDir, "workspace", wrapConnection(selection)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(setupSelectionPath(dataDir, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-value") {
		t.Fatal("setup selection persisted the API key")
	}
	if !strings.Contains(string(data), `"context_tokens":65536`) ||
		!strings.Contains(string(data), `"reasoning_efforts":["off","high"]`) {
		t.Fatalf("setup selection did not persist model metadata: %s", data)
	}
	loaded, found, err := loadWebSetupSelection(dataDir, "workspace")
	if err != nil || !found || !reflect.DeepEqual(loaded, wrapConnection(selection)) {
		t.Fatalf("loaded setup = %+v found=%v err=%v", loaded, found, err)
	}
	restored := setupModelMetadata(*loaded.Active()).Descriptor
	if restored == nil ||
		restored.Limits != metadata.Limits ||
		!reflect.DeepEqual(restored.Capabilities, metadata.Capabilities) ||
		restored.MetadataProvenance != metadata.MetadataProvenance {
		t.Fatalf("restored custom metadata = %+v", restored)
	}
	additional := setupModelMetadata(*loaded.Active()).AdditionalDescriptors
	if descriptor, ok := additional["vendor/model-v2"]; !ok ||
		descriptor.ID != "vendor/model-v2" {
		t.Fatalf("restored additional models = %+v", additional)
	}
}

func TestWebSetupRejectsMissingOrInvalidCustomMetadata(t *testing.T) {
	base := SetupRequest{
		Model: "custom-model", APIKey: "secret-value",
		BaseURL: "https://models.example.com/v1", Protocol: "openai_chat",
	}
	if _, _, err := resolveWebSetup(base); err == nil {
		t.Fatal("custom setup without metadata was accepted")
	}
	base.ModelMetadata = testSetupMetadata(base.Model)
	base.ModelMetadata.Capabilities.Vision = nil
	if _, _, err := resolveWebSetup(base); err == nil {
		t.Fatal("custom setup with an omitted capability was accepted")
	}
	base.ModelMetadata = testSetupMetadata(base.Model)
	base.ModelMetadata.ContextTokens = 0
	if _, _, err := resolveWebSetup(base); err == nil {
		t.Fatal("custom setup with zero context was accepted")
	}
	base.ModelMetadata = testSetupMetadata(base.Model)
	base.ModelMetadata.Capabilities.ToolCalls = boolPointer(false)
	if _, _, err := resolveWebSetup(base); err == nil {
		t.Fatal("custom setup without tool calls was accepted")
	}
	base.ModelMetadata = testSetupMetadata(base.Model)
	base.ModelMetadata.MaxOutputTokens = base.ModelMetadata.ContextTokens + 1
	if _, _, err := resolveWebSetup(base); err == nil {
		t.Fatal("custom setup with output above context was accepted")
	}
	base.ModelMetadata = testSetupMetadata(base.Model)
	base.ModelMetadata.Capabilities.DefaultReasoningEffort = "medium"
	if _, _, err := resolveWebSetup(base); err == nil {
		t.Fatal("custom setup with undeclared default effort was accepted")
	}
	base.ModelMetadata = testSetupMetadata(base.Model)
	base.ModelMetadata.Capabilities.Reasoning = boolPointer(false)
	base.ModelMetadata.Capabilities.ReasoningEfforts = nil
	base.ModelMetadata.Capabilities.DefaultReasoningEffort = ""
	base.ModelMetadata.Capabilities.ThinkingToggle = boolPointer(true)
	if _, _, err := resolveWebSetup(base); err == nil {
		t.Fatal("custom setup with thinking toggle but no reasoning was accepted")
	}
	base.ModelMetadata = testSetupMetadata(base.Model)
	base.ModelMetadata.Capabilities.IncrementalResponses = boolPointer(true)
	if _, _, err := resolveWebSetup(base); err == nil {
		t.Fatal("chat setup with incremental responses was accepted")
	}
}

func TestSetupProbeConnectionMatchesApplyBoundary(t *testing.T) {
	for _, protocol := range []string{"openai_chat", "openai_responses"} {
		gotID, endpoint, got, err := resolveSetupProbeConnection(SetupProbeRequest{
			BaseURL: "https://models.example.com/v1/", Protocol: protocol,
		})
		if err != nil ||
			gotID != connectionID("https://models.example.com/v1") ||
			endpoint != "https://models.example.com/v1" || string(got) != protocol {
			t.Fatalf("id=%s endpoint=%s protocol=%s err=%v", gotID, endpoint, got, err)
		}
	}
	if _, endpoint, protocol, err := resolveSetupProbeConnection(SetupProbeRequest{
		BaseURL: "https://models.example.com/v1",
	}); err != nil ||
		endpoint != "https://models.example.com/v1" ||
		protocol != "openai_chat" {
		t.Fatalf("default protocol: endpoint=%s protocol=%s err=%v", endpoint, protocol, err)
	}
	for _, request := range []SetupProbeRequest{
		{},
		{BaseURL: "file:///tmp"},
		{BaseURL: "https://example.com", Protocol: "unknown"},
	} {
		if _, _, _, err := resolveSetupProbeConnection(request); err == nil {
			t.Fatalf("accepted invalid connection: %+v", request)
		}
	}
}

func wrapConnection(connection webSetupConnection) webSetupSelection {
	return webSetupSelection{
		Version:           webSetupVersion,
		Connections:       []webSetupConnection{cloneWebSetupConnection(connection)},
		DefaultConnection: connection.ID,
	}
}

func testSetupMetadata(modelID string) *SetupModelMetadata {
	return &SetupModelMetadata{
		CanonicalID:     modelID,
		WireID:          modelID,
		ContextTokens:   65_536,
		MaxOutputTokens: 8_192,
		Capabilities: SetupModelCapabilities{
			Streaming:              boolPointer(true),
			Reasoning:              boolPointer(true),
			ReasoningEfforts:       []string{"off", "high"},
			DefaultReasoningEffort: "high",
			ToolCalls:              boolPointer(true),
			NativeSearch:           boolPointer(false),
			IncrementalResponses:   boolPointer(false),
			Vision:                 boolPointer(false),
			ImageInput:             boolPointer(false),
			PromptCache:            boolPointer(true),
			AutomaticPromptCache:   boolPointer(false),
			ThinkingToggle:         boolPointer(false),
		},
	}
}

func boolPointer(value bool) *bool {
	return &value
}

func TestSetupApplyRequiresIdempotencyBeforeCallback(t *testing.T) {
	const host = "127.0.0.1:43210"
	applied := 0
	server := newTestServerWithOptions(t, Options{
		Assets:       fstest.MapFS{"index.html": {Data: []byte("ok")}},
		ExpectedHost: host,
		Setup: &SetupOptions{Apply: func(context.Context, SetupRequest) error {
			applied++
			return nil
		}},
	})
	for _, key := range []string{"", " \t ", "setup-once"} {
		request := httptest.NewRequest(http.MethodPost,
			"http://"+host+"/api/v1/setup/apply", strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer "+server.CapabilityToken())
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if key == "setup-once" {
			if response.Code != http.StatusOK || applied != 1 {
				t.Fatalf("valid setup: status=%d calls=%d body=%s", response.Code, applied, response.Body)
			}
		} else if response.Code != http.StatusBadRequest || applied != 0 ||
			!strings.Contains(response.Body.String(), "Idempotency-Key header is required") {
			t.Fatalf("key=%q: status=%d calls=%d body=%s", key, response.Code, applied, response.Body)
		}
	}
}

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
