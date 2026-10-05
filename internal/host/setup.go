package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/common/atomicfile"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/runtime/app/wire"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/credential"
)

const (
	webSetupVersion    = 3
	webSupervisorScope = "web-supervisor"
	customProviderID   = "openai-compatible"
)

var setupModelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)

// webSetupConnection 是一条显式配置的 OpenAI-compatible 连接，
// 带各自的端点、基线模型、附加模型与凭证引用。
type webSetupConnection struct {
	ID                 string                `json:"id"`
	Provider           string                `json:"provider"`
	Model              string                `json:"model"`
	BaseURL            string                `json:"base_url,omitempty"`
	Protocol           string                `json:"protocol,omitempty"`
	Metadata           *SetupModelMetadata   `json:"model_metadata,omitempty"`
	Models             []webSetupModel       `json:"models,omitempty"`
	MetadataProvenance model.Provenance      `json:"metadata_provenance"`
	Credential         *credential.Reference `json:"credential,omitempty"`
}

// webSetupSelection v3：连接集合 + 默认连接。新会话从默认连接的基线
// 模型出发；已有会话各自持有 (provider, model) 选择。
type webSetupSelection struct {
	Version           int                  `json:"version"`
	Connections       []webSetupConnection `json:"connections"`
	DefaultConnection string               `json:"default_connection"`
}

// Active 返回默认连接（缺省回退首条）；零连接时返回 nil。
func (s webSetupSelection) Active() *webSetupConnection {
	for index := range s.Connections {
		if s.Connections[index].ID == s.DefaultConnection {
			return &s.Connections[index]
		}
	}
	if len(s.Connections) > 0 {
		return &s.Connections[0]
	}
	return nil
}

// Connection 按 ID 查找连接。
func (s webSetupSelection) Connection(id string) *webSetupConnection {
	for index := range s.Connections {
		if s.Connections[index].ID == id {
			return &s.Connections[index]
		}
	}
	return nil
}

// connectionID 生成确定性连接 ID：端点摘要保证可重复保存且 canonical
// 校验稳定。同一 Base URL 上的多个模型共享一条连接。
func connectionID(baseURL string) string {
	digest := sha256.Sum256([]byte(baseURL))
	return customProviderID + ":" + hex.EncodeToString(digest[:6])
}

type webSetupModel struct {
	ID       string             `json:"id"`
	Metadata SetupModelMetadata `json:"metadata"`
}

func cloneWebSetupSelection(input webSetupSelection) webSetupSelection {
	out := input
	out.Connections = make([]webSetupConnection, len(input.Connections))
	for index, connection := range input.Connections {
		out.Connections[index] = cloneWebSetupConnection(connection)
	}
	return out
}

func cloneWebSetupConnection(input webSetupConnection) webSetupConnection {
	out := input
	if input.Metadata != nil {
		value := *input.Metadata
		value.Capabilities.ReasoningEfforts = append(
			[]string(nil),
			input.Metadata.Capabilities.ReasoningEfforts...,
		)
		out.Metadata = &value
	}
	if input.Models != nil {
		out.Models = make([]webSetupModel, len(input.Models))
		for index, entry := range input.Models {
			out.Models[index] = entry
			out.Models[index].Metadata.Capabilities.ReasoningEfforts = append(
				[]string(nil),
				entry.Metadata.Capabilities.ReasoningEfforts...,
			)
		}
	}
	if input.Credential != nil {
		value := *input.Credential
		out.Credential = &value
	}
	return out
}

type webSetupAttempt struct {
	request SetupRequest
	result  chan error
}

// resolveSetupProbeConnection uses the same connection boundary as setup/apply:
// every probe targets an explicit OpenAI-compatible endpoint.
func resolveSetupProbeConnection(request SetupProbeRequest) (string, string, model.WireProtocol, error) {
	baseURL, err := validateSetupBaseURL(request.BaseURL)
	if err != nil {
		return "", "", "", err
	}
	protocol := model.WireProtocol(strings.TrimSpace(request.Protocol))
	if protocol == "" {
		protocol = model.ProtocolOpenAIChat
	}
	if protocol != model.ProtocolOpenAIChat && protocol != model.ProtocolOpenAIResponses {
		return "", "", "", invalidSetup("connection protocol must be openai_chat or openai_responses")
	}
	return connectionID(baseURL), baseURL, protocol, nil
}

func resolveWebSetup(request SetupRequest) (
	webSetupConnection, credential.Reference, error,
) {
	connection, reference, err := resolveWebSetupConnection(request)
	if err != nil {
		return webSetupConnection{}, credential.Reference{}, err
	}
	connection.ID = connection.Provider
	return connection, reference, nil
}

// resolveWebSetupConnection 校验一条新模型连接：Base URL、Protocol、
// Model ID、API Key 四要素齐全，模型元数据探测或手填。连接的运行时
// provider ID 由 Base URL 摘要决定，与端点一一对应。
func resolveWebSetupConnection(request SetupRequest) (
	webSetupConnection, credential.Reference, error,
) {
	modelID := strings.TrimSpace(request.Model)
	baseURL, err := validateSetupBaseURL(request.BaseURL)
	if err != nil {
		return webSetupConnection{}, credential.Reference{}, err
	}
	protocolName := strings.TrimSpace(request.Protocol)
	if protocolName == "" {
		protocolName = string(model.ProtocolOpenAIChat)
	}
	if protocolName != string(model.ProtocolOpenAIChat) &&
		protocolName != string(model.ProtocolOpenAIResponses) {
		return webSetupConnection{}, credential.Reference{}, invalidSetup(
			"connection protocol must be openai_chat or openai_responses",
		)
	}
	if !setupModelIDPattern.MatchString(modelID) {
		return webSetupConnection{}, credential.Reference{}, invalidSetup(
			"connection model id is invalid",
		)
	}
	if strings.TrimSpace(request.APIKey) == "" {
		return webSetupConnection{}, credential.Reference{}, invalidSetup(
			"API key is required",
		)
	}
	metadata, err := resolveSetupModelMetadata(protocolName, request.ModelMetadata)
	if err != nil {
		return webSetupConnection{}, credential.Reference{}, err
	}
	return webSetupConnection{
		Provider: connectionID(baseURL), Model: modelID,
		BaseURL: baseURL, Protocol: protocolName, Metadata: metadata,
		MetadataProvenance: model.ProvenanceOperatorConfig,
	}, credential.Reference{}, nil
}

// setupMetadataFromModel 把目录模型描述转换为设置协议的元数据形态。
func setupMetadataFromModel(descriptor model.Model) *SetupModelMetadata {
	return &SetupModelMetadata{
		CanonicalID:     descriptor.CanonicalID,
		WireID:          descriptor.WireID,
		ContextTokens:   descriptor.Limits.ContextTokens,
		MaxOutputTokens: descriptor.Limits.MaxOutputTokens,
		Capabilities:    setupCapabilitiesDTO(descriptor.Capabilities),
	}
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

// mergeConnection 把解析出的连接并入现有连接集：同 ID 替换、新 ID 追加，
// 并将其设为默认连接（setup/apply 的既有语义：应用即切换活跃连接）。
func mergeConnection(
	selection webSetupSelection, connection webSetupConnection,
) webSetupSelection {
	result := mergeConnectionKeepDefault(selection, connection)
	result.DefaultConnection = connection.ID
	return result
}

// mergeConnectionKeepDefault 合并连接但不改变默认连接（connection/add 用；
// 新连接仅进入集合，默认连接保持不变）。
func mergeConnectionKeepDefault(
	selection webSetupSelection, connection webSetupConnection,
) webSetupSelection {
	result := cloneWebSetupSelection(selection)
	result.Version = webSetupVersion
	replaced := false
	for index := range result.Connections {
		if result.Connections[index].ID == connection.ID {
			connection.Credential = result.Connections[index].Credential
			result.Connections[index] = connection
			replaced = true
			break
		}
	}
	if !replaced {
		result.Connections = append(result.Connections, connection)
	}
	if result.DefaultConnection == "" {
		result.DefaultConnection = connection.ID
	}
	return result
}

func setupModelMetadata(connection webSetupConnection) wire.ModelMetadataOptions {
	result := wire.ModelMetadataOptions{}
	if connection.BaseURL != "" && connection.Metadata != nil {
		result.Descriptor = setupModelDescriptor(
			connection.Model,
			*connection.Metadata,
			connection.MetadataProvenance,
		)
	}
	if len(connection.Models) != 0 {
		result.AdditionalDescriptors = make(map[string]model.Model, len(connection.Models))
		for _, registered := range connection.Models {
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
	metadata SetupModelMetadata,
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
	input *SetupModelMetadata,
) (*SetupModelMetadata, error) {
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
	input SetupModelCapabilities,
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
) SetupModelCapabilities {
	value := func(input bool) *bool { return &input }
	return SetupModelCapabilities{
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

func setupProbeResult(probed wire.ModelProbeResult) SetupProbeResult {
	result := SetupProbeResult{
		Capabilities: setupCapabilitiesDTO(probed.Capabilities),
		Warning:      probed.Warning,
	}
	for _, value := range probed.Models {
		result.Models = append(result.Models, SetupDiscoveredModel{
			ID: value.ID, Name: value.Name,
			ContextTokens:   value.ContextTokens,
			MaxOutputTokens: value.MaxOutputTokens,
		})
	}
	return result
}

func setupWireModelID(connection webSetupConnection, modelID string) string {
	if connection.Metadata != nil && modelID == connection.Model {
		return connection.Metadata.WireID
	}
	return modelID
}

// loadExecutionModelMetadata 装载显式配置的单连接会话模型元数据。
func loadExecutionModelMetadata(execution config.Execution) (*SetupModelMetadata, error) {
	if strings.TrimSpace(execution.ModelMetadata) == "" {
		return nil, invalidSetup(
			"execution.model_metadata is required; the file declares the model's limits and capabilities")
	}
	descriptor, err := wire.LoadModelMetadataFile(
		execution.ModelMetadata, execution.Model)
	if err != nil {
		return nil, fmt.Errorf("load model metadata: %w", err)
	}
	return setupMetadataFromModel(descriptor), nil
}

func setupSelectionPath(dataDir, _ string) string {
	return filepath.Join(dataDir, "web-setup", "selection.json")
}

func loadWebSetupConfig(
	options webCommandOptions,
	selection webSetupSelection,
	reference credential.Reference,
) (config.Snapshot, error) {
	active := selection.Active()
	if active == nil {
		return config.Snapshot{}, errors.New("no model connection is configured")
	}
	overrides := webConfigOverrides(options)
	providerID := active.ID
	overrides.Provider = &providerID
	overrides.Model = &active.Model
	overrides.Protocol = &active.Protocol
	overrides.CredentialKind = &reference.Kind
	overrides.CredentialName = &reference.Name
	return loadWebConfigWithOverrides(options.configPath, overrides)
}

// loadWebSetupSelection 只读取并校验当前格式的连接集，不迁移或改写配置。
func loadWebSetupSelection(dataDir, workspaceID string) (webSetupSelection, bool, error) {
	data, err := os.ReadFile(setupSelectionPath(dataDir, workspaceID))
	if errors.Is(err, os.ErrNotExist) {
		return webSetupSelection{}, false, nil
	}
	if err != nil {
		return webSetupSelection{}, false, fmt.Errorf("read Web setup selection: %w", err)
	}
	selection, err := decodeWebSetupSelection(data)
	if err != nil {
		return webSetupSelection{}, false, err
	}
	if len(selection.Connections) == 0 {
		return webSetupSelection{}, false, nil
	}
	for _, connection := range selection.Connections {
		request := SetupRequest{
			Model: connection.Model, BaseURL: connection.BaseURL,
			Protocol: connection.Protocol, APIKey: "persisted",
			ModelMetadata: connection.Metadata,
		}
		resolved, _, err := resolveWebSetupConnection(request)
		if err != nil {
			return webSetupSelection{}, false, err
		}
		// 保留显式配置或界面录入时记录的元数据来源。
		resolved.MetadataProvenance = connection.MetadataProvenance
		resolved.ID = resolved.Provider
		resolved.Models, err = resolveRegisteredModels(
			resolved.Protocol,
			resolved.Model,
			connection.Models,
		)
		if err != nil {
			return webSetupSelection{}, false, err
		}
		if connection.Credential != nil {
			if connection.Credential.Kind != "keyring" ||
				!strings.HasPrefix(connection.Credential.Name, "web/") {
				return webSetupSelection{}, false, errors.New(
					"Web setup credential reference is invalid",
				)
			}
			value := *connection.Credential
			resolved.Credential = &value
		}
		if !reflect.DeepEqual(resolved, connection) {
			return webSetupSelection{}, false, fmt.Errorf(
				"Web setup selection connection %q is not canonical: %s",
				connection.ID,
				webSetupConnectionDifference(connection, resolved),
			)
		}
	}
	return selection, true, nil
}

// webSetupConnectionDifference 命名第一条与当前派生规则不一致的字段。
// persisted 是磁盘上的记录，canonical 是按当前规则重建的结果；凭据引用
// 不进入错误文本，避免把密钥库路径带进日志。
func webSetupConnectionDifference(
	persisted, canonical webSetupConnection,
) string {
	stringFields := []struct {
		name           string
		persistedValue string
		canonicalValue string
	}{
		{"id", persisted.ID, canonical.ID},
		{"provider", persisted.Provider, canonical.Provider},
		{"model", persisted.Model, canonical.Model},
		{"base_url", persisted.BaseURL, canonical.BaseURL},
		{"protocol", persisted.Protocol, canonical.Protocol},
		{"metadata_provenance", string(persisted.MetadataProvenance),
			string(canonical.MetadataProvenance)},
	}
	for _, field := range stringFields {
		if field.persistedValue != field.canonicalValue {
			return fmt.Sprintf(
				"%s is %q, canonical %q",
				field.name, field.persistedValue, field.canonicalValue,
			)
		}
	}
	if !reflect.DeepEqual(persisted.Metadata, canonical.Metadata) {
		return "model_metadata differs from the current derivation"
	}
	if !reflect.DeepEqual(persisted.Models, canonical.Models) {
		return "models differ from the current derivation"
	}
	if !reflect.DeepEqual(persisted.Credential, canonical.Credential) {
		return "credential differs from the current derivation"
	}
	return "no field difference found"
}

// decodeWebSetupSelection 严格解码当前版本的连接集。
func decodeWebSetupSelection(data []byte) (webSetupSelection, error) {
	var selection webSetupSelection
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&selection); err != nil {
		return webSetupSelection{}, fmt.Errorf("decode Web setup selection: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return webSetupSelection{}, errors.New("Web setup selection has trailing data")
	}
	if selection.Version != webSetupVersion {
		return webSetupSelection{}, fmt.Errorf(
			"unsupported Web setup selection version %d (expected %d)",
			selection.Version, webSetupVersion,
		)
	}
	return selection, nil
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

// SetupRequest 声明一条 OpenAI-compatible 模型连接的四要素：Base URL、
// Protocol、Model ID、API Key（外加探测或手填的模型元数据）。
type SetupRequest struct {
	Model         string              `json:"model"`
	APIKey        string              `json:"api_key,omitempty"`
	BaseURL       string              `json:"base_url"`
	Protocol      string              `json:"protocol,omitempty"`
	ModelMetadata *SetupModelMetadata `json:"model_metadata,omitempty"`
}

type SetupModelMetadata struct {
	CanonicalID     string                 `json:"canonical_id"`
	WireID          string                 `json:"wire_id"`
	ContextTokens   uint64                 `json:"context_tokens"`
	MaxOutputTokens uint64                 `json:"max_output_tokens"`
	Capabilities    SetupModelCapabilities `json:"capabilities"`
}

type SetupModelCapabilities struct {
	Streaming              *bool    `json:"streaming"`
	Reasoning              *bool    `json:"reasoning"`
	ReasoningEfforts       []string `json:"reasoning_efforts,omitempty"`
	DefaultReasoningEffort string   `json:"default_reasoning_effort,omitempty"`
	ToolCalls              *bool    `json:"tool_calls"`
	NativeSearch           *bool    `json:"native_search"`
	IncrementalResponses   *bool    `json:"incremental_responses"`
	Vision                 *bool    `json:"vision"`
	ImageInput             *bool    `json:"image_input"`
	PromptCache            *bool    `json:"prompt_cache"`
	AutomaticPromptCache   *bool    `json:"automatic_prompt_cache"`
	ThinkingToggle         *bool    `json:"thinking_toggle"`
}

type SetupResult struct {
	Ready bool `json:"ready"`
}

type SetupProbeRequest struct {
	BaseURL  string `json:"base_url"`
	Protocol string `json:"protocol"`
	Model    string `json:"model"`
	APIKey   string `json:"api_key,omitempty"`
}

type SetupProbeResult struct {
	Models       []SetupDiscoveredModel `json:"models,omitempty"`
	Capabilities SetupModelCapabilities `json:"capabilities"`
	Warning      string                 `json:"warning,omitempty"`
}

type SetupDiscoveredModel struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	ContextTokens   uint64 `json:"context_tokens,omitempty"`
	MaxOutputTokens uint64 `json:"max_output_tokens,omitempty"`
}

type SetupOptions struct {
	WorkspaceRoot     string
	WorkspaceIdentity protocol.WorkspaceIdentity
	Apply             func(context.Context, SetupRequest) error
	Probe             func(context.Context, SetupProbeRequest) (SetupProbeResult, error)
	Connections       ConnectionController
}

func (o SetupOptions) validate() error {
	if o.WorkspaceRoot != "" || o.WorkspaceIdentity != (protocol.WorkspaceIdentity{}) {
		if strings.TrimSpace(o.WorkspaceRoot) == "" {
			return errors.New("setup workspace root is required with a workspace identity")
		}
		if err := o.WorkspaceIdentity.Validate(); err != nil {
			return err
		}
	}
	if o.Apply == nil {
		return errors.New("setup apply handler is required")
	}
	return nil
}

func (s *Server) setupProbe(r *http.Request, _ Dependencies) (any, error) {
	if s.setup == nil || s.setup.Probe == nil {
		return nil, unavailable("Runtime setup probe is unavailable")
	}
	var request SetupProbeRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	result, err := s.setup.Probe(r.Context(), request)
	return result, hostControlError(err)
}

func (s *Server) setupApply(r *http.Request, _ Dependencies) (any, error) {
	if s.setup == nil {
		return nil, unavailable("Runtime setup is unavailable")
	}
	var request SetupRequest
	if err := s.decodeIdempotentRequest(r, &request); err != nil {
		return nil, err
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if err := s.setup.Apply(r.Context(), request); err != nil {
		return nil, hostControlError(err)
	}
	return SetupResult{Ready: s.ready.Load()}, nil
}
