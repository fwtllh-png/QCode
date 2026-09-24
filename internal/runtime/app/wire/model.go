package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
)

type ModelMetadataOptions struct {
	Descriptor            *model.Model
	AdditionalDescriptors map[string]model.Model
	Path                  string
}

type modelMetadataFile struct {
	CanonicalID     string             `json:"canonical_id"`
	WireID          string             `json:"wire_id"`
	ContextTokens   uint64             `json:"context_tokens"`
	MaxOutputTokens uint64             `json:"max_output_tokens"`
	Capabilities    model.Capabilities `json:"capabilities"`
	Pricing         *struct {
		InputPerMillion       float64  `json:"input_per_million"`
		CachedInputPerMillion *float64 `json:"cached_input_per_million,omitempty"`
		OutputPerMillion      float64  `json:"output_per_million"`
		Currency              string   `json:"currency"`
	} `json:"pricing,omitempty"`
}

func resolveModelMetadata(modelID string, options ModelMetadataOptions) (*model.Model, error) {
	if options.Descriptor != nil {
		if options.Path != "" {
			return nil, errors.New(
				"structured model metadata cannot be combined with a metadata file",
			)
		}
		descriptor := *options.Descriptor
		descriptor.Capabilities.ReasoningEfforts = append(
			[]string(nil),
			options.Descriptor.Capabilities.ReasoningEfforts...,
		)
		if descriptor.ID != modelID {
			return nil, errors.New("custom model metadata id does not match configured model")
		}
		if err := validateResolvedModelMetadata(descriptor); err != nil {
			return nil, err
		}
		return &descriptor, nil
	}
	if options.Path == "" {
		return nil, errors.New(
			"custom endpoint requires explicit model metadata",
		)
	}
	descriptor, err := loadModelMetadata(options.Path, modelID)
	if err != nil {
		return nil, err
	}
	if err := validateResolvedModelMetadata(descriptor); err != nil {
		return nil, err
	}
	return &descriptor, nil
}

func validateResolvedModelMetadata(descriptor model.Model) error {
	if strings.TrimSpace(descriptor.ID) == "" ||
		strings.TrimSpace(descriptor.CanonicalID) == "" ||
		strings.TrimSpace(descriptor.WireID) == "" ||
		descriptor.Limits.ContextTokens == 0 ||
		descriptor.Limits.MaxOutputTokens == 0 ||
		descriptor.MetadataProvenance.CanonicalID == "" ||
		descriptor.MetadataProvenance.WireID == "" ||
		descriptor.MetadataProvenance.Limits == "" ||
		descriptor.MetadataProvenance.Capabilities == "" ||
		descriptor.MetadataProvenance.Pricing == "" ||
		descriptor.Provenance == "" {
		return errors.New(
			"custom endpoint requires explicit identity, limits, capabilities, and provenance",
		)
	}
	for _, entry := range []struct {
		field      string
		provenance model.Provenance
	}{
		{field: "canonical_id", provenance: descriptor.MetadataProvenance.CanonicalID},
		{field: "wire_id", provenance: descriptor.MetadataProvenance.WireID},
		{field: "limits", provenance: descriptor.MetadataProvenance.Limits},
		{field: "capabilities", provenance: descriptor.MetadataProvenance.Capabilities},
		{field: "pricing", provenance: descriptor.MetadataProvenance.Pricing},
		{field: "model", provenance: descriptor.Provenance},
	} {
		switch entry.provenance {
		case model.ProvenanceBundled, model.ProvenanceConfig,
			model.ProvenanceStartup, model.ProvenanceFixture,
			model.ProvenanceProviderDiscovery,
			model.ProvenanceOperatorConfig, model.ProvenanceMixed:
		default:
			return fmt.Errorf(
				"custom model %s provenance %q is invalid",
				entry.field,
				entry.provenance,
			)
		}
	}
	if descriptor.Limits.MaxOutputTokens > descriptor.Limits.ContextTokens {
		return errors.New("model output limit exceeds context limit")
	}
	capabilities := descriptor.Capabilities
	if !capabilities.Streaming {
		return errors.New("custom endpoint must declare streaming capability")
	}
	if !capabilities.Reasoning &&
		(len(capabilities.ReasoningEfforts) != 0 ||
			capabilities.DefaultReasoningEffort != "" ||
			capabilities.ThinkingToggle) {
		return errors.New("reasoning controls require reasoning capability")
	}
	seen := make(map[string]struct{}, len(capabilities.ReasoningEfforts))
	for _, effort := range capabilities.ReasoningEfforts {
		if !slices.Contains(
			[]string{"none", "off", "minimal", "low", "medium", "high", "xhigh", "max"},
			effort,
		) {
			return fmt.Errorf("reasoning effort %q is invalid", effort)
		}
		if _, duplicate := seen[effort]; duplicate {
			return fmt.Errorf("reasoning effort %q is duplicated", effort)
		}
		seen[effort] = struct{}{}
	}
	if effort := capabilities.DefaultReasoningEffort; effort != "" {
		if _, ok := seen[effort]; !ok {
			return fmt.Errorf(
				"default reasoning effort %q is not advertised",
				effort,
			)
		}
	}
	if capabilities.AutomaticPromptCache && !capabilities.PromptCache {
		return errors.New(
			"automatic prompt cache requires prompt cache capability",
		)
	}
	return nil
}

// LoadModelMetadataFile 读取并校验一份模型元数据 JSON 文件，供宿主把
// execution.model_metadata 指向的声明装载进连接配置。
func LoadModelMetadataFile(path, modelID string) (model.Model, error) {
	descriptor, err := loadModelMetadata(path, modelID)
	if err != nil {
		return model.Model{}, err
	}
	if err := validateResolvedModelMetadata(descriptor); err != nil {
		return model.Model{}, err
	}
	return descriptor, nil
}

func loadModelMetadata(path, modelID string) (model.Model, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return model.Model{}, fmt.Errorf("read model metadata: %w", err)
	}
	var input modelMetadataFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return model.Model{}, fmt.Errorf("decode model metadata: %w", err)
	}
	if input.CanonicalID == "" || input.WireID == "" {
		return model.Model{}, errors.New(
			"model metadata requires canonical_id and wire_id",
		)
	}
	if input.Pricing != nil {
		if !strings.EqualFold(input.Pricing.Currency, "USD") {
			return model.Model{},
				errors.New("model metadata pricing currency must be USD")
		}
		if input.Pricing.InputPerMillion < 0 ||
			input.Pricing.OutputPerMillion < 0 ||
			input.Pricing.CachedInputPerMillion != nil &&
				*input.Pricing.CachedInputPerMillion < 0 {
			return model.Model{},
				errors.New("model metadata pricing must be non-negative")
		}
	}
	descriptor := model.Model{
		ID: modelID, CanonicalID: input.CanonicalID, WireID: input.WireID,
		Limits: model.Limits{
			ContextTokens: input.ContextTokens, MaxOutputTokens: input.MaxOutputTokens,
		},
		Capabilities: input.Capabilities,
		Pricing:      model.Pricing{Provenance: model.ProvenanceOperatorConfig},
		MetadataProvenance: model.MetadataProvenance{
			CanonicalID:  model.ProvenanceOperatorConfig,
			WireID:       model.ProvenanceOperatorConfig,
			Limits:       model.ProvenanceOperatorConfig,
			Capabilities: model.ProvenanceOperatorConfig,
			Pricing:      model.ProvenanceOperatorConfig,
		},
		Provenance: model.ProvenanceOperatorConfig,
	}
	if input.Pricing != nil {
		descriptor.Pricing = model.Pricing{
			InputPerMillion:       input.Pricing.InputPerMillion,
			CachedInputPerMillion: input.Pricing.CachedInputPerMillion,
			OutputPerMillion:      input.Pricing.OutputPerMillion,
			Currency:              input.Pricing.Currency,
			Known:                 true,
			Provenance:            model.ProvenanceOperatorConfig,
		}
	}
	return descriptor, nil
}
