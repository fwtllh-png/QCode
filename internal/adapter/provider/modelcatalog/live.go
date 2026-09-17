package modelcatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	provideranthropic "github.com/fwtllh-png/QCode/internal/adapter/provider/anthropic"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/httpclient"
	provideropenai "github.com/fwtllh-png/QCode/internal/adapter/provider/openai"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/router"
	"github.com/fwtllh-png/QCode/internal/security/egress"
)

var NewRegistry, New = router.NewRegistry, router.New

type liveModelRow struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	DisplayName     string `json:"display_name,omitempty"`
	ContextWindow   uint64 `json:"context_window,omitempty"`
	ContextLength   uint64 `json:"context_length,omitempty"`
	MaxTokens       uint64 `json:"max_tokens,omitempty"`
	MaxOutputTokens uint64 `json:"max_output_tokens,omitempty"`
}

type DiscoveredModel struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	ContextTokens   uint64 `json:"context_tokens,omitempty"`
	MaxOutputTokens uint64 `json:"max_output_tokens,omitempty"`
}

func ProbeCapabilities(
	ctx context.Context,
	baseURL, apiKey, modelID string,
) (model.Capabilities, error) {
	return ProbeCapabilitiesForProtocol(ctx, baseURL, apiKey, modelID, model.ProtocolOpenAIChat, 0)
}

func ProbeCapabilitiesForProtocol(
	ctx context.Context,
	baseURL, apiKey, modelID string,
	protocol model.WireProtocol,
	maxOutputTokens uint64,
) (model.Capabilities, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	target := base + "/chat/completions"
	if protocol != model.ProtocolOpenAIChat && protocol != model.ProtocolOpenAIResponses && protocol != model.ProtocolAnthropic {
		return model.Capabilities{}, fmt.Errorf("automatic capability probing is unavailable for %s; enter model metadata explicitly", protocol)
	}
	if protocol == model.ProtocolOpenAIResponses {
		target = base + "/responses"
	} else if protocol == model.ProtocolAnthropic {
		target = base + "/messages"
		if maxOutputTokens == 0 {
			return model.Capabilities{}, errors.New("the provider did not advertise the output limit required for a Messages probe; enter model metadata explicitly")
		}
	}
	gate := &egress.Gate{Enforce: true}
	if !gate.AllowURL(target) && !gate.AllowURL(base) {
		return model.Capabilities{}, fmt.Errorf("model probe endpoint host cannot be granted")
	}
	payload := map[string]any{
		"model": modelID,
		"messages": []map[string]string{{
			"role":    "user",
			"content": "Call capability_probe once with an empty object.",
		}},
		"stream": true,
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "capability_probe",
				"description": "Verify function calling support.",
				"parameters": map[string]any{
					"type":                 "object",
					"properties":           map[string]any{},
					"additionalProperties": false,
				},
			},
		}},
		"tool_choice": "required",
	}
	if protocol == model.ProtocolOpenAIResponses {
		delete(payload, "messages")
		payload["input"] = "Call capability_probe once with an empty object."
		payload["store"] = false
		payload["tools"] = []map[string]any{{
			"type": "function", "name": "capability_probe",
			"description": "Verify function calling support.",
			"parameters": map[string]any{
				"type": "object", "properties": map[string]any{}, "additionalProperties": false,
			},
		}}
	} else if protocol == model.ProtocolAnthropic {
		payload["max_tokens"] = maxOutputTokens
		payload["tools"] = []map[string]any{{
			"name": "capability_probe", "description": "Verify function calling support.",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
		}}
		payload["tool_choice"] = map[string]any{"type": "tool", "name": "capability_probe"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return model.Capabilities{}, err
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		target,
		bytes.NewReader(body),
	)
	if err != nil {
		return model.Capabilities{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	if strings.TrimSpace(apiKey) != "" {
		if protocol == model.ProtocolAnthropic {
			request.Header.Set("x-api-key", strings.TrimSpace(apiKey))
		} else {
			request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
		}
	}
	if protocol == model.ProtocolAnthropic {
		request.Header.Set("anthropic-version", "2023-06-01")
	}
	response, err := egress.WrapClient(
		&http.Client{Timeout: 20 * time.Second},
		gate,
	).Do(request)
	if err != nil {
		return model.Capabilities{}, fmt.Errorf("model capability probe unreachable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return model.Capabilities{}, fmt.Errorf(
			"model capability probe HTTP %d",
			response.StatusCode,
		)
	}
	var stream provider.Stream
	if protocol == model.ProtocolAnthropic {
		stream, err = provideranthropic.NewStream(response.Body)
	} else {
		stream, err = provideropenai.NewStream(response.Body, protocol)
	}
	if err != nil {
		return model.Capabilities{}, err
	}
	var capabilities model.Capabilities
	for {
		event, receiveErr := stream.Recv()
		if receiveErr != nil {
			if errors.Is(receiveErr, io.EOF) {
				break
			}
			return model.Capabilities{}, receiveErr
		}
		switch event.Type {
		case provider.EventMessageStart:
			capabilities.Streaming = true
		case provider.EventReasoningDelta:
			capabilities.Reasoning = true
		case provider.EventToolCallDelta:
			capabilities.ToolCalls = true
		}
	}
	return capabilities, nil
}

func ProbeCapabilitiesWithCredential(
	ctx context.Context,
	baseURL string,
	credential model.CredentialRef,
	modelID string,
) (model.Capabilities, error) {
	return ProbeCapabilitiesWithCredentialForProtocol(ctx, baseURL, credential, modelID, model.ProtocolOpenAIChat, 0)
}

func ProbeCapabilitiesWithCredentialForProtocol(
	ctx context.Context,
	baseURL string,
	credential model.CredentialRef,
	modelID string,
	protocol model.WireProtocol,
	maxOutputTokens uint64,
) (model.Capabilities, error) {
	apiKey := ""
	if credential.Kind != "" && credential.Name != "" {
		var err error
		apiKey, err = httpclient.DefaultCredentials().Resolve(ctx, credential)
		if err != nil {
			return model.Capabilities{}, fmt.Errorf(
				"resolve credential %s: %w",
				credential.Name,
				err,
			)
		}
	}
	return ProbeCapabilitiesForProtocol(ctx, baseURL, apiKey, modelID, protocol, maxOutputTokens)
}

func List(
	ctx context.Context,
	providerID, baseURL string,
	credentialOverride model.CredentialRef,
) (map[string]any, error) {
	providerID = strings.TrimSpace(providerID)
	catalogProvider, ok := model.DefaultCatalog().Provider(providerID)
	if !ok {
		if providerID == "" || strings.TrimSpace(baseURL) == "" {
			return nil, fmt.Errorf("unknown provider %q", providerID)
		}
		catalogProvider = model.Provider{ID: providerID, Endpoint: baseURL}
	}
	if fixture := strings.TrimSpace(os.Getenv("QCODE_MODEL_LIST_FIXTURE")); fixture != "" {
		data, err := os.ReadFile(fixture)
		if err != nil {
			return nil, err
		}
		return liveModelResult(catalogProvider.ID, "fixture", "", data)
	}
	credential := catalogProvider.Credential
	if credentialOverride.Kind != "" || credentialOverride.Name != "" {
		credential = credentialOverride
	}
	apiKey := ""
	if credential.Kind != "" && credential.Name != "" {
		var err error
		apiKey, err = httpclient.DefaultCredentials().Resolve(ctx, credential)
		if err != nil {
			return nil, fmt.Errorf("resolve credential %s: %w", credential.Name, err)
		}
	}
	return discover(
		ctx,
		catalogProvider,
		baseURL,
		apiKey,
	)
}

func Discover(
	ctx context.Context,
	providerID, baseURL, apiKey string,
) (map[string]any, error) {
	providerID = strings.TrimSpace(providerID)
	catalogProvider, ok := model.DefaultCatalog().Provider(providerID)
	if !ok {
		if providerID == "" || strings.TrimSpace(baseURL) == "" {
			return nil, fmt.Errorf("unknown provider %q", providerID)
		}
		catalogProvider = model.Provider{ID: providerID, Endpoint: baseURL}
	}
	return discover(ctx, catalogProvider, baseURL, strings.TrimSpace(apiKey))
}

func discover(
	ctx context.Context,
	catalogProvider model.Provider,
	baseURL, apiKey string,
) (map[string]any, error) {
	base := strings.TrimRight(catalogProvider.Endpoint, "/")
	if strings.TrimSpace(baseURL) != "" {
		base = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	}
	target := base + "/models"
	if override := strings.TrimSpace(os.Getenv("QCODE_MODEL_LIST_URL")); override != "" {
		target = override
	}
	gate := &egress.Gate{Enforce: true}
	if !gate.AllowURL(target) && !gate.AllowURL(base) {
		return nil, fmt.Errorf("live list endpoint host cannot be granted")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		if catalogProvider.Protocol == model.ProtocolAnthropic {
			request.Header.Set("x-api-key", apiKey)
			request.Header.Set("anthropic-version", "2023-06-01")
		} else {
			request.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}
	response, err := egress.WrapClient(
		&http.Client{Timeout: 8 * time.Second},
		gate,
	).Do(request)
	if err != nil {
		return nil, fmt.Errorf("live list unreachable")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("live list HTTP %d", response.StatusCode)
	}
	return liveModelResult(catalogProvider.ID, "live", liveModelHost(base), body)
}

func Probe(
	ctx context.Context,
	providerID, baseURL string,
	credential model.CredentialRef,
	modelID string,
) (bool, error) {
	value, err := List(ctx, providerID, baseURL, credential)
	if err != nil {
		return false, err
	}
	ids, _ := value["models"].([]string)
	return slices.Contains(ids, strings.TrimSpace(modelID)), nil
}

func liveModelResult(provider, source, endpointHost string, data []byte) (map[string]any, error) {
	var payload struct {
		Data []liveModelRow `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode models payload: %w", err)
	}
	models := make([]DiscoveredModel, 0, len(payload.Data))
	for _, row := range payload.Data {
		if id := strings.TrimSpace(row.ID); id != "" {
			name := strings.TrimSpace(row.Name)
			if name == "" {
				name = strings.TrimSpace(row.DisplayName)
			}
			contextTokens := row.ContextWindow
			if contextTokens == 0 {
				contextTokens = row.ContextLength
			}
			maxOutputTokens := row.MaxOutputTokens
			if maxOutputTokens == 0 {
				maxOutputTokens = row.MaxTokens
			}
			models = append(models, DiscoveredModel{
				ID: id, Name: name,
				ContextTokens:   contextTokens,
				MaxOutputTokens: maxOutputTokens,
			})
		}
	}
	ids := make([]string, len(models))
	for index := range models {
		ids[index] = models[index].ID
	}
	result := map[string]any{
		"provider": provider, "source": source, "live": true,
		"models": ids, "model_metadata": models, "count": len(ids),
	}
	if endpointHost != "" {
		result["endpoint_host"] = endpointHost
	}
	return result, nil
}

func liveModelHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}
