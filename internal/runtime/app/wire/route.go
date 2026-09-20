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
	var catalog *model.Catalog
	if options.BaseURL == "" {
		catalog = model.DefaultCatalog()
	} else {
		provenance := model.ProvenanceStartup
		if options.Fixture {
			provenance = model.ProvenanceFixture
		}
		adapter := model.AdapterOpenAICompatible
		if bundled, exists := model.DefaultCatalog().Provider(options.ProviderID); exists {
			adapter = bundled.Adapter
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
		var err error
		catalog, err = model.NewCatalog(model.Provider{
			ID: options.ProviderID, Adapter: adapter, Endpoint: options.BaseURL,
			Protocol: options.Protocol, Credential: credential, Provenance: provenance,
			Models: map[string]model.Model{options.ModelID: descriptor},
		})
		if err != nil {
			return model.ReadyRoute{}, err
		}
	}
	resolver, err := model.NewResolver(catalog)
	if err != nil {
		return model.ReadyRoute{}, err
	}
	provenance := model.ProvenanceStartup
	if options.Fixture {
		provenance = model.ProvenanceFixture
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
