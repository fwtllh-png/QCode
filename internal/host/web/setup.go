package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/config"
	webhost "github.com/fwtllh-png/QCode/internal/host/runtimeapi/web"
	"github.com/fwtllh-png/QCode/internal/persist/atomicfile"
	"github.com/fwtllh-png/QCode/internal/runtime/app/wire"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/credential"
)

const (
	webSetupVersion    = 2
	webSupervisorScope = "web-supervisor"
	customProviderID   = "openai-compatible"
)

var setupModelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)

var setupProviderAliases = map[string][]string{
	"deepseek": {"deepseek-v4-flash"},
}

type webSetupSelection struct {
	Version            int                         `json:"version"`
	Provider           string                      `json:"provider"`
	Model              string                      `json:"model"`
	BaseURL            string                      `json:"base_url,omitempty"`
	Protocol           string                      `json:"protocol,omitempty"`
	Metadata           *webhost.SetupModelMetadata `json:"model_metadata,omitempty"`
	Models             []webSetupModel             `json:"models,omitempty"`
	MetadataProvenance model.Provenance            `json:"metadata_provenance"`
	Credential         *credential.Reference       `json:"credential,omitempty"`
}

type webSetupModel struct {
	ID       string                     `json:"id"`
	Metadata webhost.SetupModelMetadata `json:"metadata"`
}

func cloneWebSetupSelection(input webSetupSelection) webSetupSelection {
	out := input
	if input.Metadata != nil {
		value := *input.Metadata
		value.Capabilities.ReasoningEfforts = append(
			[]string(nil),
			input.Metadata.Capabilities.ReasoningEfforts...,
		)
		out.Metadata = &value
	}
	out.Models = make([]webSetupModel, len(input.Models))
	for index, entry := range input.Models {
		out.Models[index] = entry
		out.Models[index].Metadata.Capabilities.ReasoningEfforts = append(
			[]string(nil),
			entry.Metadata.Capabilities.ReasoningEfforts...,
		)
	}
	if input.Credential != nil {
		value := *input.Credential
		out.Credential = &value
	}
	return out
}

type webSetupAttempt struct {
	request webhost.SetupRequest
	result  chan error
}

func webSetupCatalog() webhost.SetupCatalog {
	catalog := model.DefaultCatalog()
	displayNames := map[string]string{
		"openai":   "OpenAI",
		"deepseek": "DeepSeek",
		"glm":      "GLM",
	}
	providers := make([]webhost.SetupProvider, 0, len(displayNames)+1)
	for _, id := range []string{"openai", "deepseek", "glm"} {
		provider, exists := catalog.Provider(id)
		if !exists {
			continue
		}
		providers = append(providers, webhost.SetupProvider{
			ID: id, DisplayName: displayNames[id],
			Protocol:       string(provider.Protocol),
			RequiresAPIKey: provider.Credential.Kind != "",
			Models:         setupKnownModels(catalog, provider),
		})
	}
	providers = append(providers, webhost.SetupProvider{
		ID: customProviderID, DisplayName: "OpenAI-compatible",
		Protocol: string(model.ProtocolOpenAIChat), Custom: true,
	})
	return webhost.SetupCatalog{
		Version: webhost.SetupCatalogVersion, Providers: providers,
	}
}

func setupKnownModels(
	catalog *model.Catalog,
	selected model.Provider,
) []string {
	counts := make(map[string]int)
	for _, provider := range catalog.Providers() {
		if !setupProviderOwns(selected.ID, provider.ID) {
			continue
		}
		for modelID := range provider.Models {
			counts[modelID]++
		}
	}
	models := make([]string, 0, len(counts))
	for modelID, count := range counts {
		if _, direct := selected.Models[modelID]; direct || count == 1 {
			models = append(models, modelID)
		}
	}
	sort.Strings(models)
	return models
}

// resolveSetupProbeConnection uses the same provider boundary as setup/apply.
// Built-in providers never accept an endpoint or protocol override from the UI.
func resolveSetupProbeConnection(request webhost.SetupProbeRequest) (string, string, model.WireProtocol, error) {
	providerID := strings.TrimSpace(request.Provider)
	if providerID == customProviderID {
		baseURL, err := validateSetupBaseURL(request.BaseURL)
		if err != nil {
			return "", "", "", err
		}
		protocol := model.WireProtocol(strings.TrimSpace(request.Protocol))
		if protocol == "" {
			protocol = model.ProtocolOpenAIChat
		}
		if protocol != model.ProtocolOpenAIChat && protocol != model.ProtocolOpenAIResponses {
			return "", "", "", invalidSetup("custom provider protocol must be openai_chat or openai_responses")
		}
		return providerID, baseURL, protocol, nil
	}
	for _, allowed := range webSetupCatalog().Providers {
		if allowed.ID != providerID || allowed.Custom {
			continue
		}
		provider, exists := model.DefaultCatalog().Provider(providerID)
		if exists {
			return providerID, provider.Endpoint, provider.Protocol, nil
		}
	}
	return "", "", "", invalidSetup("unknown setup provider")
}

func resolveWebSetup(request webhost.SetupRequest) (
	webSetupSelection, credential.Reference, error,
) {
	providerID := strings.TrimSpace(request.Provider)
	modelID := strings.TrimSpace(request.Model)
	if providerID == customProviderID {
		baseURL, err := validateSetupBaseURL(request.BaseURL)
		if err != nil {
			return webSetupSelection{}, credential.Reference{}, err
		}
		protocolName := strings.TrimSpace(request.Protocol)
		if protocolName == "" {
			protocolName = string(model.ProtocolOpenAIChat)
		}
		if protocolName != string(model.ProtocolOpenAIChat) &&
			protocolName != string(model.ProtocolOpenAIResponses) {
			return webSetupSelection{}, credential.Reference{}, invalidSetup(
				"custom provider protocol must be openai_chat or openai_responses",
			)
		}
		if !setupModelIDPattern.MatchString(modelID) {
			return webSetupSelection{}, credential.Reference{}, invalidSetup(
				"custom provider model id is invalid",
			)
		}
		metadata, err := resolveSetupModelMetadata(
			protocolName,
			request.ModelMetadata,
		)
		if err != nil {
			return webSetupSelection{}, credential.Reference{}, err
		}
		return webSetupSelection{
			Version: webSetupVersion, Provider: customProviderID, Model: modelID,
			BaseURL: baseURL, Protocol: protocolName, Metadata: metadata,
			MetadataProvenance: model.ProvenanceOperatorConfig,
		}, credential.Reference{}, nil
	}

	if !setupModelIDPattern.MatchString(modelID) {
		return webSetupSelection{}, credential.Reference{}, invalidSetup(
			"model id is invalid",
		)
	}
	allowed := map[string]bool{
		"openai": true, "deepseek": true, "glm": true,
	}
	catalog := model.DefaultCatalog()
	provider, exists := catalog.Provider(providerID)
	_, directlyKnown := provider.Models[modelID]
	if !exists || !allowed[providerID] && !directlyKnown {
		return webSetupSelection{}, credential.Reference{}, invalidSetup(
			"provider must be OpenAI, DeepSeek, GLM, or OpenAI-compatible",
		)
	}
	routeProvider := provider
	if !directlyKnown {
		if matched, found := uniqueSetupProviderForModel(
			catalog,
			provider,
			modelID,
		); found {
			routeProvider = matched
			directlyKnown = true
		}
	}
	if routeProvider.Credential.Kind != "" && strings.TrimSpace(request.APIKey) == "" {
		return webSetupSelection{}, credential.Reference{}, invalidSetup("API key is required")
	}
	baseURL := ""
	if !directlyKnown {
		baseURL = provider.Endpoint
	}
	var metadata *webhost.SetupModelMetadata
	if !directlyKnown {
		var err error
		metadata, err = resolveSetupModelMetadata(
			string(routeProvider.Protocol),
			request.ModelMetadata,
		)
		if err != nil {
			return webSetupSelection{}, credential.Reference{}, err
		}
	} else if request.ModelMetadata != nil {
		return webSetupSelection{}, credential.Reference{}, invalidSetup(
			"model metadata is not accepted for a catalog model",
		)
	}
	return webSetupSelection{
			Version:  webSetupVersion,
			Provider: providerID,
			Model:    modelID,
			BaseURL:  baseURL,
			Protocol: string(routeProvider.Protocol),
			Metadata: metadata,
			MetadataProvenance: func() model.Provenance {
				if metadata != nil {
					return model.ProvenanceOperatorConfig
				}
				return model.ProvenanceBundled
			}(),
		}, credential.Reference{
			Kind: routeProvider.Credential.Kind, Name: routeProvider.Credential.Name,
		}, nil
}

func uniqueSetupProviderForModel(
	catalog *model.Catalog,
	selected model.Provider,
	modelID string,
) (model.Provider, bool) {
	var match model.Provider
	found := false
	for _, provider := range catalog.Providers() {
		if provider.Adapter != selected.Adapter ||
			!setupProviderOwns(selected.ID, provider.ID) {
			continue
		}
		if _, exists := provider.Models[modelID]; !exists {
			continue
		}
		if found {
			return model.Provider{}, false
		}
		match = provider
		found = true
	}
	return match, found
}

func setupProviderOwns(selectedID, candidateID string) bool {
	return selectedID == candidateID ||
		slices.Contains(setupProviderAliases[selectedID], candidateID)
}

func validateSetupBaseURL(value string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", invalidSetup("custom provider base URL is invalid")
	}
	if parsed.Scheme == "https" {
		return value, nil
	}
	address := net.ParseIP(parsed.Hostname())
	if parsed.Scheme == "http" &&
		(parsed.Hostname() == "localhost" || address != nil && address.IsLoopback()) {
		return value, nil
	}
	return "", invalidSetup("custom provider must use HTTPS or loopback HTTP")
}

func setupModelMetadata(selection webSetupSelection) wire.ModelMetadataOptions {
	result := wire.ModelMetadataOptions{}
	if selection.BaseURL != "" && selection.Metadata != nil {
		result.Descriptor = setupModelDescriptor(
			selection.Model,
			*selection.Metadata,
			selection.MetadataProvenance,
		)
	}
	if len(selection.Models) != 0 {
		result.AdditionalDescriptors = make(map[string]model.Model, len(selection.Models))
		for _, registered := range selection.Models {
			result.AdditionalDescriptors[registered.ID] = *setupModelDescriptor(
				registered.ID,
				registered.Metadata,
				model.ProvenanceOperatorConfig,
			)
		}
	}
	return result
}

func setupModelDescriptor(
	id string,
	metadata webhost.SetupModelMetadata,
	provenance model.Provenance,
) *model.Model {
	capabilities, _ := setupCapabilities(metadata.Capabilities)
	capabilities = wire.WithDefaultReasoningEfforts(id, capabilities)
	return &model.Model{
		ID: id, CanonicalID: metadata.CanonicalID, WireID: metadata.WireID,
		Limits: model.Limits{
			ContextTokens: metadata.ContextTokens, MaxOutputTokens: metadata.MaxOutputTokens,
		},
		Capabilities: capabilities,
		Pricing:      model.Pricing{Provenance: provenance},
		MetadataProvenance: model.MetadataProvenance{
			CanonicalID:  provenance,
			WireID:       provenance,
			Limits:       provenance,
			Capabilities: provenance,
			Pricing:      provenance,
		},
		Provenance: provenance,
	}
}

func resolveSetupModelMetadata(
	protocolName string,
	input *webhost.SetupModelMetadata,
) (*webhost.SetupModelMetadata, error) {
	if input == nil {
		return nil, invalidSetup("custom provider model metadata is required")
	}
	metadata := *input
	metadata.CanonicalID = strings.TrimSpace(metadata.CanonicalID)
	metadata.WireID = strings.TrimSpace(metadata.WireID)
	metadata.Capabilities.ReasoningEfforts = append(
		[]string(nil),
		metadata.Capabilities.ReasoningEfforts...,
	)
	for index := range metadata.Capabilities.ReasoningEfforts {
		metadata.Capabilities.ReasoningEfforts[index] =
			strings.TrimSpace(metadata.Capabilities.ReasoningEfforts[index])
	}
	metadata.Capabilities.DefaultReasoningEffort =
		strings.TrimSpace(metadata.Capabilities.DefaultReasoningEffort)
	capabilities, err := setupCapabilities(metadata.Capabilities)
	if err != nil {
		return nil, err
	}
	metadata.Capabilities = setupCapabilitiesDTO(capabilities)
	if !setupModelIDPattern.MatchString(metadata.CanonicalID) ||
		!setupModelIDPattern.MatchString(metadata.WireID) {
		return nil, invalidSetup("custom model canonical_id and wire_id are required")
	}
	if metadata.ContextTokens == 0 || metadata.MaxOutputTokens == 0 {
		return nil, invalidSetup("custom model context and output limits must be positive")
	}
	if metadata.MaxOutputTokens > metadata.ContextTokens {
		return nil, invalidSetup("custom model output limit exceeds context limit")
	}
	if !capabilities.Streaming {
		return nil, invalidSetup("custom model must declare streaming capability")
	}
	if !capabilities.ToolCalls {
		return nil, invalidSetup("custom model must support tool calls")
	}
	if !capabilities.Reasoning &&
		(len(capabilities.ReasoningEfforts) != 0 ||
			capabilities.DefaultReasoningEffort != "" ||
			capabilities.ThinkingToggle) {
		return nil, invalidSetup(
			"custom model reasoning controls require reasoning capability",
		)
	}
	seen := make(map[string]struct{}, len(capabilities.ReasoningEfforts))
	for _, effort := range capabilities.ReasoningEfforts {
		if !slices.Contains(
			[]string{"none", "off", "minimal", "low", "medium", "high", "xhigh", "max"},
			effort,
		) {
			return nil, invalidSetup("custom model reasoning effort is invalid")
		}
		if _, duplicate := seen[effort]; duplicate {
			return nil, invalidSetup("custom model reasoning efforts must be unique")
		}
		seen[effort] = struct{}{}
	}
	if capabilities.DefaultReasoningEffort != "" &&
		!slices.Contains(
			capabilities.ReasoningEfforts,
			capabilities.DefaultReasoningEffort,
		) {
		return nil, invalidSetup(
			"custom model default reasoning effort must be declared",
		)
	}
	if capabilities.AutomaticPromptCache && !capabilities.PromptCache {
		return nil, invalidSetup(
			"custom model automatic prompt cache requires prompt cache capability",
		)
	}
	if capabilities.IncrementalResponses &&
		protocolName != string(model.ProtocolOpenAIResponses) {
		return nil, invalidSetup(
			"custom model incremental responses require openai_responses protocol",
		)
	}
	return &metadata, nil
}

func setupCapabilities(
	input webhost.SetupModelCapabilities,
) (model.Capabilities, error) {
	required := []*bool{
		input.Streaming,
		input.Reasoning,
		input.ToolCalls,
		input.NativeSearch,
		input.IncrementalResponses,
		input.Vision,
		input.ImageInput,
		input.PromptCache,
		input.AutomaticPromptCache,
		input.ThinkingToggle,
	}
	if slices.Contains(required, nil) {
		return model.Capabilities{}, invalidSetup(
			"custom model capability declaration is incomplete",
		)
	}
	return model.Capabilities{
		Streaming:              *input.Streaming,
		Reasoning:              *input.Reasoning,
		ReasoningEfforts:       append([]string(nil), input.ReasoningEfforts...),
		DefaultReasoningEffort: input.DefaultReasoningEffort,
		ToolCalls:              *input.ToolCalls,
		NativeSearch:           *input.NativeSearch,
		IncrementalResponses:   *input.IncrementalResponses,
		Vision:                 *input.Vision,
		ImageInput:             *input.ImageInput,
		PromptCache:            *input.PromptCache,
		AutomaticPromptCache:   *input.AutomaticPromptCache,
		ThinkingToggle:         *input.ThinkingToggle,
	}, nil
}

func setupCapabilitiesDTO(
	input model.Capabilities,
) webhost.SetupModelCapabilities {
	value := func(input bool) *bool { return &input }
	return webhost.SetupModelCapabilities{
		Streaming:              value(input.Streaming),
		Reasoning:              value(input.Reasoning),
		ReasoningEfforts:       append([]string(nil), input.ReasoningEfforts...),
		DefaultReasoningEffort: input.DefaultReasoningEffort,
		ToolCalls:              value(input.ToolCalls),
		NativeSearch:           value(input.NativeSearch),
		IncrementalResponses:   value(input.IncrementalResponses),
		Vision:                 value(input.Vision),
		ImageInput:             value(input.ImageInput),
		PromptCache:            value(input.PromptCache),
		AutomaticPromptCache:   value(input.AutomaticPromptCache),
		ThinkingToggle:         value(input.ThinkingToggle),
	}
}

func setupRuntimeProviderID(selection webSetupSelection) string {
	if selection.Provider == customProviderID || selection.BaseURL != "" {
		return selection.Provider
	}
	catalog := model.DefaultCatalog()
	provider, exists := catalog.Provider(selection.Provider)
	if !exists {
		return selection.Provider
	}
	if routeProvider, found := uniqueSetupProviderForModel(
		catalog,
		provider,
		selection.Model,
	); found {
		return routeProvider.ID
	}
	return selection.Provider
}

func setupWireModelID(selection webSetupSelection, modelID string) string {
	if selection.Metadata != nil && modelID == selection.Model {
		return selection.Metadata.WireID
	}
	return modelID
}

func setupSelectionPath(dataDir, _ string) string {
	return filepath.Join(dataDir, "web-setup", "selection.json")
}

func loadWebSetupConfig(
	options webCommandOptions,
	selection webSetupSelection,
	reference credential.Reference,
) (config.Snapshot, error) {
	overrides := webConfigOverrides(options)
	runtimeProviderID := setupRuntimeProviderID(selection)
	overrides.Provider = &runtimeProviderID
	overrides.Model = &selection.Model
	overrides.Protocol = &selection.Protocol
	overrides.CredentialKind = &reference.Kind
	overrides.CredentialName = &reference.Name
	return config.Load(config.LoadOptions{
		Path: options.configPath, Overrides: overrides,
	})
}

func loadWebSetupSelection(dataDir, workspaceID string) (webSetupSelection, bool, error) {
	data, err := os.ReadFile(setupSelectionPath(dataDir, workspaceID))
	if errors.Is(err, os.ErrNotExist) {
		return webSetupSelection{}, false, nil
	}
	if err != nil {
		return webSetupSelection{}, false, fmt.Errorf("read Web setup selection: %w", err)
	}
	var selection webSetupSelection
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&selection); err != nil {
		return webSetupSelection{}, false, fmt.Errorf("decode Web setup selection: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return webSetupSelection{}, false, errors.New("Web setup selection has trailing data")
	}
	if selection.Provider == customProviderID && selection.Metadata == nil {
		return webSetupSelection{}, false, nil
	}
	request := webhost.SetupRequest{
		Provider: selection.Provider, Model: selection.Model,
		BaseURL: selection.BaseURL, Protocol: selection.Protocol, APIKey: "persisted",
		ModelMetadata: selection.Metadata,
	}
	resolved, _, err := resolveWebSetup(request)
	if err != nil {
		if selection.Version < webSetupVersion &&
			selection.BaseURL != "" && selection.Metadata == nil {
			return webSetupSelection{}, false, nil
		}
		return webSetupSelection{}, false, err
	}
	resolved.Models, err = resolveRegisteredModels(
		resolved.Protocol,
		resolved.Model,
		selection.Models,
	)
	if err != nil {
		return webSetupSelection{}, false, err
	}
	if selection.Credential != nil {
		if selection.Credential.Kind != "keyring" ||
			!strings.HasPrefix(selection.Credential.Name, "web/") {
			return webSetupSelection{}, false, errors.New(
				"Web setup credential reference is invalid",
			)
		}
		value := *selection.Credential
		resolved.Credential = &value
	}
	if !reflect.DeepEqual(resolved, selection) &&
		!canUpgradeSetupSelection(selection, resolved) {
		return webSetupSelection{}, false, errors.New("Web setup selection is not canonical")
	}
	return resolved, true, nil
}

func resolveRegisteredModels(
	protocolName, baseline string,
	input []webSetupModel,
) ([]webSetupModel, error) {
	if input == nil {
		return nil, nil
	}
	result := make([]webSetupModel, 0, len(input))
	seen := map[string]bool{baseline: true}
	for _, entry := range input {
		entry.ID = strings.TrimSpace(entry.ID)
		if !setupModelIDPattern.MatchString(entry.ID) {
			return nil, invalidSetup("registered model id is invalid")
		}
		if seen[entry.ID] {
			return nil, invalidSetup("registered model id must be unique")
		}
		metadata, err := resolveSetupModelMetadata(protocolName, &entry.Metadata)
		if err != nil {
			return nil, err
		}
		entry.Metadata = *metadata
		seen[entry.ID] = true
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func canUpgradeSetupSelection(previous, resolved webSetupSelection) bool {
	return (previous.Version == 1 || previous.Version == webSetupVersion) &&
		previous.Provider != customProviderID &&
		previous.Provider == resolved.Provider &&
		previous.Model == resolved.Model &&
		(previous.MetadataProvenance == "" ||
			previous.MetadataProvenance == resolved.MetadataProvenance) &&
		resolved.BaseURL == ""
}

func saveWebSetupSelection(dataDir, workspaceID string, selection webSetupSelection) error {
	path := setupSelectionPath(dataDir, workspaceID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(selection)
	if err != nil {
		return err
	}
	return atomicfile.Replace(path, append(data, '\n'), 0o600)
}

func invalidSetup(message string) error {
	return protocol.NewProblem(protocol.CodeInvalidArgument, message, false, nil)
}
