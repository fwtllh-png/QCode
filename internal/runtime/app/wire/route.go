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

// execConnectionProvider keeps the startup identity and credential selection in
// the same catalog entry used by act, purpose slots, and selectable routes.
func execConnectionProvider(options execRouteOptions) (model.Provider, error) {
	if options.ProviderID == "" || options.ModelID == "" {
		return model.Provider{}, errors.New("--provider and --model are required without --provider-fixture")
	}
	if options.BaseURL == "" {
		return model.Provider{}, fmt.Errorf(
			"provider %q requires an explicit base URL; every connection is OpenAI-compatible",
			options.ProviderID)
	}
	if options.Model == nil {
		return model.Provider{}, errors.New("custom endpoint requires explicit model metadata")
	}
	if options.Model.ID != options.ModelID {
		return model.Provider{}, errors.New("custom model metadata id does not match --model")
	}
	provenance := model.ProvenanceStartup
	if options.Fixture {
		provenance = model.ProvenanceFixture
	}
	credential := options.Credential
	if credential == (model.CredentialRef{}) && options.APIKeyEnv != "" {
		credential = model.CredentialRef{Kind: "env", Name: options.APIKeyEnv}
	}
	return model.Provider{
		ID: options.ProviderID, Adapter: model.AdapterOpenAICompatible,
		Endpoint: options.BaseURL, Protocol: options.Protocol,
		Credential: credential, Provenance: provenance,
		Models: map[string]model.Model{options.ModelID: *options.Model},
	}, nil
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

// connectionsCatalog validates every configured connection once. Fixture-only
// slot models are registered here too, but never become selectable implicitly.
func connectionsCatalog(options routeSetOptions) (*model.Catalog, error) {
	act, err := execConnectionProvider(options.Act)
	if err != nil {
		return nil, err
	}
	for id, descriptor := range options.Additional {
		if descriptor.ID != id {
			return nil, fmt.Errorf("additional model %q has mismatched id %q", id, descriptor.ID)
		}
		if err := validateResolvedModelMetadata(descriptor); err != nil {
			return nil, fmt.Errorf("additional model %q: %w", id, err)
		}
		// The execution descriptor remains authoritative for the active model,
		// including when a registered-model list also contains that model ID.
		if id != options.Act.ModelID {
			act.Models[id] = descriptor
		}
	}
	if options.Act.Fixture {
		for name, slot := range options.Slots {
			if slot.Provider != act.ID {
				return nil, fmt.Errorf(
					"route.%s: a fixture session routes every purpose through the fixture provider %q, not %q",
					name, act.ID, slot.Provider)
			}
			if _, exists := act.Models[slot.Model]; !exists {
				act.Models[slot.Model] = *fixtureModel(slot.Model)
			}
		}
	}
	providers := make([]model.Provider, 0, len(options.Extras)+1)
	providers = append(providers, act)
	for _, spec := range options.Extras {
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
