package wire

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
)

func resolveExecRoute(options execRouteOptions) (model.ReadyRoute, error) {
	if options.ProviderID == "" || options.ModelID == "" {
		return model.ReadyRoute{}, errors.New("--provider and --model are required without --provider-fixture")
	}
	if options.BaseURL == "" {
		return model.ReadyRoute{}, fmt.Errorf(
			"provider %q requires an explicit base URL; every connection is OpenAI-compatible",
			options.ProviderID)
	}
	provenance := model.ProvenanceStartup
	if options.Fixture {
		provenance = model.ProvenanceFixture
	}
	credential := model.CredentialRef{}
	if options.APIKeyEnv != "" {
		credential = model.CredentialRef{Kind: "env", Name: options.APIKeyEnv}
	}
	if options.Model == nil {
		return model.ReadyRoute{}, errors.New("custom endpoint requires explicit model metadata")
	}
	descriptor := *options.Model
	if descriptor.ID != options.ModelID {
		return model.ReadyRoute{}, errors.New("custom model metadata id does not match --model")
	}
	catalog, err := model.NewCatalog(model.Provider{
		ID: options.ProviderID, Adapter: model.AdapterOpenAICompatible,
		Endpoint: options.BaseURL, Protocol: options.Protocol,
		Credential: credential, Provenance: provenance,
		Models: map[string]model.Model{options.ModelID: descriptor},
	})
	if err != nil {
		return model.ReadyRoute{}, err
	}
	resolver, err := model.NewResolver(catalog)
	if err != nil {
		return model.ReadyRoute{}, err
	}
	route, err := resolver.Resolve(model.RouteRequest{
		ProviderID: options.ProviderID, ModelID: options.ModelID, Provenance: provenance,
	})
	if err != nil {
		return model.ReadyRoute{}, err
	}
	if options.Credential.Kind != "" || options.Credential.Name != "" {
		route = route.WithCredential(options.Credential)
	}
	return route, nil
}

func fixtureModel(id string) *model.Model {
	return &model.Model{
		ID: id, CanonicalID: id, WireID: id,
		Limits: model.Limits{ContextTokens: 1_000_000, MaxOutputTokens: 64_000},
		Capabilities: model.Capabilities{
			Streaming: true, Reasoning: true, ToolCalls: true, NativeSearch: true,
			Vision: true, ImageInput: true, PromptCache: true,
			// 与产品文档的推理档位约定保持一致；Default 档由客户端以空值补充。
			ReasoningEfforts: []string{"low", "medium", "high", "xhigh"},
		},
		Pricing: model.Pricing{
			CachedInputPerMillion: new(float64),
			Currency:              "USD", Known: true, Provenance: model.ProvenanceFixture,
		},
		MetadataProvenance: model.MetadataProvenance{
			CanonicalID: model.ProvenanceFixture, WireID: model.ProvenanceFixture,
			Limits: model.ProvenanceFixture, Capabilities: model.ProvenanceFixture,
			Pricing: model.ProvenanceFixture,
		},
		Provenance: model.ProvenanceFixture,
	}
}

func parseProtocol(value string) (model.WireProtocol, error) {
	wireProtocol := model.WireProtocol(value)
	switch wireProtocol {
	case model.ProtocolOpenAIChat, model.ProtocolOpenAIResponses:
		return wireProtocol, nil
	default:
		return "", fmt.Errorf("unsupported protocol %q", value)
	}
}

func resolveFixturePath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	repositoryPath := filepath.Clean(filepath.Join(filepath.Dir(executable), "..", path))
	if _, err := os.Stat(repositoryPath); err != nil {
		return "", err
	}
	return repositoryPath, nil
}

func defaultPromptBudgets(maxTokens uint64) map[string]promptcontext.Budget {
	maxInt := uint64(^uint(0) >> 1)
	maxBytes := maxInt
	if maxTokens <= maxInt/4 {
		maxBytes = maxTokens * 4
	}
	budget := promptcontext.Budget{MaxBytes: int(maxBytes), MaxTokens: maxTokens}
	result := map[string]promptcontext.Budget{
		promptcontext.PartitionTotal: budget,
	}
	for _, partition := range []string{
		promptcontext.PartitionBase, promptcontext.PartitionMode,
		promptcontext.PartitionRepository, promptcontext.PartitionWorkingSet,
		promptcontext.PartitionSkills, promptcontext.PartitionUserMemory,
		promptcontext.PartitionConstitution, promptcontext.PartitionToolPrefix,
		promptcontext.PartitionToolCatalog, promptcontext.PartitionRepoMap,
		promptcontext.PartitionDirectoryRules,
		promptcontext.PartitionWorkingSetLedger, promptcontext.PartitionEvidence,
		promptcontext.PartitionCodingPolicy,
	} {
		result[partition] = budget
	}
	return result
}

func promptBudgets(
	configured map[string]promptcontext.Budget,
	maxTokens uint64,
) map[string]promptcontext.Budget {
	if configured == nil {
		return defaultPromptBudgets(maxTokens)
	}
	result := make(map[string]promptcontext.Budget, len(configured)+1)
	for partition, budget := range configured {
		result[partition] = budget
	}
	result[promptcontext.PartitionTotal] =
		defaultPromptBudgets(maxTokens)[promptcontext.PartitionTotal]
	return result
}

func routePromptBudgets(
	configured map[string]promptcontext.Budget,
	route model.ReadyRoute,
	maxOutputTokens, maxTurnTokens, maxSessionTokens uint64,
) map[string]promptcontext.Budget {
	capacity := agentcontext.ResolveCapacity(
		route, maxOutputTokens, maxTurnTokens, maxSessionTokens,
	)
	return promptBudgets(configured, capacity.HardInputTokens)
}


// connectionsCatalog 把默认连接与附加连接组装成一个目录：每条连接一个
// provider（统一 OpenAI-compatible 适配器、显式端点、各自的凭证与模型）。
// 用途 slot 解析与可选路由派生；不存在内置目录回退。
func connectionsCatalog(
	act execRouteOptions,
	additional map[string]model.Model,
	extras []ExtraConnectionSpec,
) (*model.Catalog, error) {
	providers := make([]model.Provider, 0, len(extras)+1)
	actCredential := model.CredentialRef{}
	if act.APIKeyEnv != "" {
		actCredential = model.CredentialRef{Kind: "env", Name: act.APIKeyEnv}
	}
	if act.Model == nil {
		return nil, fmt.Errorf(
			"connection %s requires explicit model metadata", act.ProviderID)
	}
	actModels := map[string]model.Model{act.ModelID: *act.Model}
	for id, descriptor := range additional {
		if descriptor.ID != id {
			return nil, fmt.Errorf(
				"connection %s model %q has mismatched id %q",
				act.ProviderID, id, descriptor.ID)
		}
		actModels[id] = descriptor
	}
	providers = append(providers, model.Provider{
		ID: act.ProviderID, Adapter: model.AdapterOpenAICompatible,
		Endpoint: act.BaseURL, Protocol: act.Protocol,
		Credential: actCredential, Provenance: model.ProvenanceStartup,
		Models: actModels,
	})
	for _, spec := range extras {
		provider, err := extraConnectionProvider(spec)
		if err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	return model.NewCatalog(providers...)
}

// extraConnectionProvider 校验并转换一条附加连接为目录 provider。
func extraConnectionProvider(spec ExtraConnectionSpec) (model.Provider, error) {
	if spec.BaseURL == "" {
		return model.Provider{}, fmt.Errorf(
			"extra connection %s requires an explicit base URL", spec.ProviderID)
	}
	if spec.Model == nil {
		return model.Provider{}, fmt.Errorf(
			"extra connection %s requires explicit model metadata", spec.ProviderID)
	}
	models := map[string]model.Model{spec.Model.ID: *spec.Model}
	for id, descriptor := range spec.Models {
		if descriptor.ID != id {
			return model.Provider{}, fmt.Errorf(
				"extra connection %s model %q has mismatched id %q",
				spec.ProviderID, id, descriptor.ID)
		}
		models[id] = descriptor
	}
	return model.Provider{
		ID: spec.ProviderID, Adapter: model.AdapterOpenAICompatible,
		Endpoint: spec.BaseURL, Protocol: spec.Protocol,
		Credential: spec.Credential, Provenance: model.ProvenanceStartup,
		Models: models,
	}, nil
}

// extraConnectionRoutes 把附加连接解析为可选路由：每条连接在自身单
// provider 目录上解析基线与附加模型，套用连接凭证。
func extraConnectionRoutes(specs []ExtraConnectionSpec) (map[string]model.ReadyRoute, error) {
	result := make(map[string]model.ReadyRoute)
	for _, spec := range specs {
		provider, err := extraConnectionProvider(spec)
		if err != nil {
			return nil, err
		}
		catalog, err := model.NewCatalog(provider)
		if err != nil {
			return nil, err
		}
		resolver, err := model.NewResolver(catalog)
		if err != nil {
			return nil, err
		}
		for id := range provider.Models {
			route, err := resolver.Resolve(model.RouteRequest{
				ProviderID: spec.ProviderID, ModelID: id,
			})
			if err != nil {
				return nil, err
			}
			result[model.RouteKey(spec.ProviderID, id)] = route
		}
	}
	return result, nil
}
