package web

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
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
	"sync"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/config"
	webhost "github.com/fwtllh-png/QCode/internal/host/runtimeapi/web"
	"github.com/fwtllh-png/QCode/internal/persist/atomicfile"
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

// legacySetupCatalogData 冻结了旧内置目录中曾暴露给 Web 设置界面的
// provider 条目（openai/deepseek/glm 及 deepseek-v4-flash 别名归属）。
// 它只服务于 selection.json 旧预设连接的一次性物化迁移，不再作为
// 可选 provider 来源。
//
//go:embed legacy_setup_catalog.json
var legacySetupCatalogData []byte

type legacySetupCatalog struct {
	Version   int              `json:"version"`
	Aliases   map[string][]string `json:"aliases"`
	Providers []model.Provider `json:"providers"`
}

var legacySetupProviders = sync.OnceValue(func() map[string]model.Provider {
	var document legacySetupCatalog
	if err := json.Unmarshal(legacySetupCatalogData, &document); err != nil {
		panic(fmt.Errorf("decode legacy setup catalog: %w", err))
	}
	if document.Version != 1 {
		panic(fmt.Errorf("unsupported legacy setup catalog version %d", document.Version))
	}
	result := make(map[string]model.Provider, len(document.Providers))
	for _, provider := range document.Providers {
		if provider.Adapter.Supports(provider.Protocol) &&
			provider.Endpoint != "" && len(provider.Models) > 0 {
			result[provider.ID] = provider
		}
	}
	return result
})

var legacySetupAliases = sync.OnceValue(func() map[string][]string {
	var document legacySetupCatalog
	if err := json.Unmarshal(legacySetupCatalogData, &document); err != nil {
		panic(err)
	}
	return document.Aliases
})

// webSetupConnection 是一条可用的模型连接：内置 provider 或自定义
// OpenAI-compatible 端点，带各自的基线模型、附加模型与凭证引用。
type webSetupConnection struct {
	ID        string                     `json:"id"`
	Provider  string                     `json:"provider"`
	Model     string                     `json:"model"`
	BaseURL   string                     `json:"base_url,omitempty"`
	Protocol  string                     `json:"protocol,omitempty"`
	Metadata  *webhost.SetupModelMetadata `json:"model_metadata,omitempty"`
	Models    []webSetupModel            `json:"models,omitempty"`
	MetadataProvenance model.Provenance  `json:"metadata_provenance"`
	Credential *credential.Reference     `json:"credential,omitempty"`
}

// webSetupSelection v3：连接集合 + 默认连接。新会话从默认连接的基线
// 模型出发；已有会话各自持有 (provider, model) 选择。
type webSetupSelection struct {
	Version           int                   `json:"version"`
	Connections       []webSetupConnection  `json:"connections"`
	DefaultConnection string                `json:"default_connection"`
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
	ID       string                     `json:"id"`
	Metadata webhost.SetupModelMetadata `json:"metadata"`
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
	request webhost.SetupRequest
	result  chan error
}

// resolveSetupProbeConnection uses the same connection boundary as setup/apply:
// every probe targets an explicit OpenAI-compatible endpoint.
func resolveSetupProbeConnection(request webhost.SetupProbeRequest) (string, string, model.WireProtocol, error) {
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

func resolveWebSetup(request webhost.SetupRequest) (
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
func resolveWebSetupConnection(request webhost.SetupRequest) (
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

// legacyProviderForModel 在冻结迁移表中找到拥有该模型的 provider：
// 直接匹配优先，再按旧别名归属（如 deepseek → deepseek-v4-flash）。
func legacyProviderForModel(providerID, modelID string) (model.Provider, bool) {
	providers := legacySetupProviders()
	if provider, exists := providers[providerID]; exists {
		if _, known := provider.Models[modelID]; known {
			return provider, true
		}
	}
	var match model.Provider
	found := false
	for candidateID, provider := range providers {
		owned := candidateID == providerID ||
			slices.Contains(legacySetupAliases()[providerID], candidateID)
		if !owned {
			continue
		}
		if _, known := provider.Models[modelID]; !known {
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

// materializeLegacyConnection 把旧预设连接（BaseURL 为空、端点来自已
// 删除的内置目录）物化为显式连接：补全端点、协议与模型元数据，保留
// 原连接 ID，keyring 凭证引用继续有效。
func materializeLegacyConnection(connection webSetupConnection) (webSetupConnection, bool) {
	owner, known := legacyProviderForModel(connection.Provider, connection.Model)
	if !known {
		return webSetupConnection{}, false
	}
	descriptor, known := owner.Models[connection.Model]
	if !known {
		return webSetupConnection{}, false
	}
	connection.BaseURL = owner.Endpoint
	connection.Protocol = string(owner.Protocol)
	connection.Metadata = setupMetadataFromModel(descriptor)
	connection.MetadataProvenance = model.ProvenanceBundled
	return connection, true
}

// setupMetadataFromModel 把目录模型描述转换为设置协议的元数据形态。
func setupMetadataFromModel(descriptor model.Model) *webhost.SetupModelMetadata {
	value := func(input bool) *bool { return &input }
	capabilities := descriptor.Capabilities
	return &webhost.SetupModelMetadata{
		CanonicalID:     descriptor.CanonicalID,
		WireID:          descriptor.WireID,
		ContextTokens:   descriptor.Limits.ContextTokens,
		MaxOutputTokens: descriptor.Limits.MaxOutputTokens,
		Capabilities: webhost.SetupModelCapabilities{
			Streaming:              value(capabilities.Streaming),
			Reasoning:              value(capabilities.Reasoning),
			ReasoningEfforts:       append([]string(nil), capabilities.ReasoningEfforts...),
			DefaultReasoningEffort: capabilities.DefaultReasoningEffort,
			ToolCalls:              value(capabilities.ToolCalls),
			NativeSearch:           value(capabilities.NativeSearch),
			IncrementalResponses:   value(capabilities.IncrementalResponses),
			Vision:                 value(capabilities.Vision),
			ImageInput:             value(capabilities.ImageInput),
			PromptCache:            value(capabilities.PromptCache),
			AutomaticPromptCache:   value(capabilities.AutomaticPromptCache),
			ThinkingToggle:         value(capabilities.ThinkingToggle),
		},
	}
}

// isMaterializedLegacyConnection 判断一条已保存连接是否为物化后的旧
// 预设连接（Provider 保留旧名而非端点摘要）；canonical 校验对其放行。
func isMaterializedLegacyConnection(connection webSetupConnection) bool {
	owner, known := legacyProviderForModel(connection.Provider, connection.Model)
	if !known {
		return false
	}
	descriptor, known := owner.Models[connection.Model]
	return known &&
		owner.Endpoint == connection.BaseURL &&
		owner.Protocol == model.WireProtocol(connection.Protocol) &&
		connection.Metadata != nil &&
		reflect.DeepEqual(connection.Metadata, setupMetadataFromModel(descriptor))
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
	result := cloneWebSetupSelection(selection)
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
	result.DefaultConnection = connection.ID
	return result
}

// mergeConnectionKeepDefault 合并连接但不改变默认连接（connection/add 用；
// 新连接仅进入集合，默认连接保持不变）。
func mergeConnectionKeepDefault(
	selection webSetupSelection, connection webSetupConnection,
) webSetupSelection {
	result := cloneWebSetupSelection(selection)
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

func setupWireModelID(connection webSetupConnection, modelID string) string {
	if connection.Metadata != nil && modelID == connection.Model {
		return connection.Metadata.WireID
	}
	return modelID
}

// loadExecutionModelMetadata 装载 TOML/CLI 单连接会话的模型元数据。
func loadExecutionModelMetadata(execution config.Execution) (*webhost.SetupModelMetadata, error) {
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
	return config.Load(config.LoadOptions{
		Path: options.configPath, Overrides: overrides,
	})
}

// loadWebSetupSelection 读取并校验已保存的连接集。旧预设连接（BaseURL
// 为空）与旧自定义连接（Provider 为 "openai-compatible"）在此物化/规范
// 化为新形态并回写；无法迁移的旧连接被丢弃而不是让启动失败。
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
	migrated := false
	kept := make([]webSetupConnection, 0, len(selection.Connections))
	for _, stored := range selection.Connections {
		connection := stored
		if connection.BaseURL == "" {
			materialized, ok := materializeLegacyConnection(connection)
			if !ok {
				migrated = true
				continue
			}
			connection = materialized
			migrated = true
		}
		if connection.Metadata == nil {
			migrated = true
			continue
		}
		request := webhost.SetupRequest{
			Model: connection.Model, BaseURL: connection.BaseURL,
			Protocol: connection.Protocol, APIKey: "persisted",
			ModelMetadata: connection.Metadata,
		}
		resolved, _, err := resolveWebSetupConnection(request)
		if err != nil {
			return webSetupSelection{}, false, err
		}
		// 物化迁移保留原 provenance（bundled）；新连接恒为 operator_config。
		resolved.MetadataProvenance = connection.MetadataProvenance
		resolved.ID = connection.ID
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
		if connection.Provider != resolved.Provider {
			// 规范化旧形态：旧自定义连接 Provider 为 "openai-compatible"
			// 而 ID 已是端点摘要；物化后的旧预设连接保留旧 provider 名。
			hashID := connectionID(connection.BaseURL)
			switch {
			case connection.ID == hashID &&
				(connection.Provider == customProviderID ||
					connection.Provider == connection.ID):
				connection.Provider = hashID
				migrated = true
			case connection.ID == connection.Provider &&
				isMaterializedLegacyConnection(connection):
			default:
				return webSetupSelection{}, false, errors.New(
					"Web setup selection is not canonical",
				)
			}
			resolved.Provider = connection.Provider
		}
		if !reflect.DeepEqual(resolved, connection) {
			return webSetupSelection{}, false, errors.New("Web setup selection is not canonical")
		}
		kept = append(kept, connection)
	}
	selection.Connections = kept
	if len(kept) == 0 {
		return webSetupSelection{}, false, nil
	}
	if selection.Active() == nil {
		selection.DefaultConnection = kept[0].ID
		migrated = true
	}
	if migrated {
		if err := saveWebSetupSelection(dataDir, workspaceID, selection); err != nil {
			return webSetupSelection{}, false, fmt.Errorf(
				"persist migrated Web setup selection: %w", err)
		}
	}
	return selection, true, nil
}

// decodeWebSetupSelection 严格解码 v3 连接集，并把历史 v1/v2 单连接格式
// 无损升级为单条目 v3（仓库显式升级先例：setup v1→v2）。旧预设连接缺省
// 的端点/元数据在 loadWebSetupSelection 的物化迁移中补全。
func decodeWebSetupSelection(data []byte) (webSetupSelection, error) {
	var selection webSetupSelection
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&selection); err != nil {
		var typeError *json.UnmarshalTypeError
		if errors.As(err, &typeError) && typeError.Field == "connections" {
			return webSetupSelection{}, errors.New("Web setup selection is not canonical")
		}
		// v2（及更早）的扁平字段在 v3 结构上是未知字段：尝试旧格式升级。
		upgraded, upgradeErr := decodeLegacyWebSetupSelection(data)
		if upgradeErr != nil {
			return webSetupSelection{}, fmt.Errorf("decode Web setup selection: %w", err)
		}
		return upgraded, nil
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return webSetupSelection{}, errors.New("Web setup selection has trailing data")
	}
	if selection.Version == 0 {
		selection.Version = webSetupVersion
	}
	return selection, nil
}

type legacyWebSetupSelection struct {
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

func decodeLegacyWebSetupSelection(data []byte) (webSetupSelection, error) {
	var legacy legacyWebSetupSelection
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&legacy); err != nil {
		return webSetupSelection{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return webSetupSelection{}, errors.New("Web setup selection has trailing data")
	}
	if legacy.Provider == "" {
		return webSetupSelection{}, errors.New("Web setup selection has no provider")
	}
	id := legacy.Provider
	if legacy.BaseURL != "" {
		id = connectionID(legacy.BaseURL)
	}
	connection := webSetupConnection{
		ID: id,
		Provider: legacy.Provider, Model: legacy.Model,
		BaseURL: legacy.BaseURL, Protocol: legacy.Protocol,
		Metadata: legacy.Metadata, Models: legacy.Models,
		MetadataProvenance: legacy.MetadataProvenance,
		Credential: legacy.Credential,
	}
	return webSetupSelection{
		Version: webSetupVersion,
		Connections: []webSetupConnection{connection},
		DefaultConnection: connection.ID,
	}, nil
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
