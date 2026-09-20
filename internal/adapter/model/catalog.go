package model

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
)

//go:embed catalog.v1.json
var bundledCatalog []byte

type AdapterID string

const (
	AdapterOpenAI           AdapterID = "openai"
	AdapterOpenAICompatible AdapterID = "openai_compatible"
)

type WireProtocol string

const (
	ProtocolOpenAIChat      WireProtocol = "openai_chat"
	ProtocolOpenAIResponses WireProtocol = "openai_responses"
)

type Provenance string

const (
	ProvenanceBundled           Provenance = "bundled"
	ProvenanceConfig            Provenance = "config"
	ProvenanceStartup           Provenance = "startup"
	ProvenanceFixture           Provenance = "fixture"
	ProvenanceProviderDiscovery Provenance = "provider_discovery"
	ProvenanceOperatorConfig    Provenance = "operator_config"
	ProvenanceMixed             Provenance = "mixed"
)

type CredentialRef struct {
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
}

type Limits struct {
	ContextTokens   uint64 `json:"context_tokens"`
	MaxOutputTokens uint64 `json:"max_output_tokens"`
}

type Capabilities struct {
	Streaming              bool     `json:"streaming"`
	Reasoning              bool     `json:"reasoning"`
	ReasoningEfforts       []string `json:"reasoning_efforts,omitempty"`
	DefaultReasoningEffort string   `json:"default_reasoning_effort,omitempty"`
	ToolCalls              bool     `json:"tool_calls"`
	NativeSearch           bool     `json:"native_search"`
	// IncrementalResponses allows connection-local Responses continuation.
	IncrementalResponses bool `json:"incremental_responses,omitempty"`
	Vision               bool `json:"vision"`
	ImageInput           bool `json:"image_input"`
	PromptCache          bool `json:"prompt_cache"`
	AutomaticPromptCache bool `json:"automatic_prompt_cache,omitempty"`
	ThinkingToggle       bool `json:"thinking_toggle,omitempty"`
}

func (c Capabilities) ReasoningEffortLevels() []string {
	if !c.Reasoning {
		return nil
	}
	return append([]string(nil), c.ReasoningEfforts...)
}

func (c Capabilities) SupportsReasoningEffort(effort string) bool {
	return effort != "" &&
		slices.Contains(c.ReasoningEffortLevels(), effort)
}

type Pricing struct {
	InputPerMillion       float64    `json:"input_per_million"`
	CachedInputPerMillion *float64   `json:"cached_input_per_million,omitempty"`
	OutputPerMillion      float64    `json:"output_per_million"`
	Currency              string     `json:"currency"`
	Known                 bool       `json:"known"`
	Provenance            Provenance `json:"provenance"`
}

type MetadataProvenance struct {
	CanonicalID  Provenance `json:"canonical_id"`
	WireID       Provenance `json:"wire_id"`
	Limits       Provenance `json:"limits"`
	Capabilities Provenance `json:"capabilities"`
	Pricing      Provenance `json:"pricing"`
}

type Model struct {
	ID                 string             `json:"id"`
	CanonicalID        string             `json:"canonical_id"`
	WireID             string             `json:"wire_id"`
	Limits             Limits             `json:"limits"`
	Capabilities       Capabilities       `json:"capabilities"`
	Pricing            Pricing            `json:"pricing"`
	MetadataProvenance MetadataProvenance `json:"metadata_provenance"`
	Provenance         Provenance         `json:"provenance"`
}

type Provider struct {
	ID         string           `json:"id"`
	Adapter    AdapterID        `json:"adapter"`
	Endpoint   string           `json:"endpoint"`
	Protocol   WireProtocol     `json:"protocol"`
	Credential CredentialRef    `json:"credential"`
	Models     map[string]Model `json:"models"`
	Provenance Provenance       `json:"provenance"`
}

type Catalog struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

func NewCatalog(providers ...Provider) (*Catalog, error) {
	catalog := &Catalog{providers: make(map[string]Provider, len(providers))}
	for _, provider := range providers {
		if err := catalog.AddProvider(provider); err != nil {
			return nil, err
		}
	}
	return catalog, nil
}

func DefaultCatalog() *Catalog {
	var document struct {
		Version   int        `json:"version"`
		Providers []Provider `json:"providers"`
	}
	if err := json.Unmarshal(bundledCatalog, &document); err != nil {
		panic(fmt.Errorf("decode bundled model catalog: %w", err))
	}
	if document.Version != 1 {
		panic(fmt.Errorf("unsupported bundled model catalog version %d", document.Version))
	}
	catalog, err := NewCatalog(document.Providers...)
	if err != nil {
		panic(err)
	}
	return catalog
}

func (c *Catalog) AddProvider(provider Provider) error {
	provider = normalizeProvider(provider)
	if err := validateProvider(provider); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.providers[provider.ID]; exists {
		return fmt.Errorf("provider %q already exists", provider.ID)
	}
	c.providers[provider.ID] = cloneProvider(provider)
	return nil
}

func (c *Catalog) Provider(id string) (Provider, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	provider, exists := c.providers[id]
	return cloneProvider(provider), exists
}

func (c *Catalog) Providers() []Provider {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ids := make([]string, 0, len(c.providers))
	for id := range c.providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]Provider, 0, len(ids))
	for _, id := range ids {
		result = append(result, cloneProvider(c.providers[id]))
	}
	return result
}

func validateProvider(provider Provider) error {
	if strings.TrimSpace(provider.ID) == "" {
		return errors.New("provider id is required")
	}
	if !provider.Adapter.Supports(provider.Protocol) {
		return fmt.Errorf(
			"provider %q adapter %q does not support protocol %q",
			provider.ID, provider.Adapter, provider.Protocol,
		)
	}
	switch provider.Protocol {
	case ProtocolOpenAIChat, ProtocolOpenAIResponses:
	default:
		return fmt.Errorf("provider %q has unsupported protocol %q", provider.ID, provider.Protocol)
	}
	endpoint, err := url.Parse(provider.Endpoint)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return fmt.Errorf("provider %q has invalid endpoint", provider.ID)
	}
	if len(provider.Models) == 0 {
		return fmt.Errorf("provider %q has no models", provider.ID)
	}
	for key, model := range provider.Models {
		if key == "" || model.ID != key || model.CanonicalID == "" || model.WireID == "" {
			return fmt.Errorf("provider %q has invalid model %q", provider.ID, key)
		}
		if model.Limits.ContextTokens == 0 || model.Limits.MaxOutputTokens == 0 {
			return fmt.Errorf("provider %q model %q has invalid limits", provider.ID, key)
		}
		if model.Limits.MaxOutputTokens > model.Limits.ContextTokens {
			return fmt.Errorf("provider %q model %q output limit exceeds context", provider.ID, key)
		}
		if model.Capabilities.AutomaticPromptCache &&
			!model.Capabilities.PromptCache {
			return fmt.Errorf(
				"provider %q model %q declares automatic prompt cache without prompt cache support",
				provider.ID,
				key,
			)
		}
		if !model.Capabilities.Reasoning &&
			(len(model.Capabilities.ReasoningEfforts) != 0 ||
				model.Capabilities.DefaultReasoningEffort != "" ||
				model.Capabilities.ThinkingToggle) {
			return fmt.Errorf(
				"provider %q model %q declares reasoning controls without reasoning",
				provider.ID,
				key,
			)
		}
		if model.Capabilities.IncrementalResponses &&
			provider.Protocol != ProtocolOpenAIResponses {
			return fmt.Errorf(
				"provider %q model %q declares incremental responses outside the responses protocol",
				provider.ID,
				key,
			)
		}
		efforts := make(map[string]struct{})
		for _, effort := range model.Capabilities.ReasoningEfforts {
			if !slices.Contains(
				[]string{"none", "off", "minimal", "low", "medium", "high", "xhigh", "max"},
				effort,
			) {
				return fmt.Errorf(
					"provider %q model %q has invalid reasoning effort %q",
					provider.ID,
					key,
					effort,
				)
			}
			if _, exists := efforts[effort]; exists {
				return fmt.Errorf(
					"provider %q model %q repeats reasoning effort %q",
					provider.ID,
					key,
					effort,
				)
			}
			efforts[effort] = struct{}{}
		}
		if effort := model.Capabilities.DefaultReasoningEffort; effort != "" {
			if _, exists := efforts[effort]; !exists {
				return fmt.Errorf(
					"provider %q model %q default reasoning effort %q is not advertised",
					provider.ID,
					key,
					effort,
				)
			}
		}
		if model.Pricing.Known && model.Pricing.Currency == "" {
			return fmt.Errorf("provider %q model %q known pricing requires currency", provider.ID, key)
		}
		if cached := model.Pricing.CachedInputPerMillion; cached != nil &&
			(!model.Pricing.Known || *cached < 0) {
			return fmt.Errorf("provider %q model %q has invalid cached input pricing", provider.ID, key)
		}
		if !model.Pricing.Known && (model.Pricing.InputPerMillion != 0 || model.Pricing.OutputPerMillion != 0) {
			return fmt.Errorf("provider %q model %q unknown pricing must not set dollar rates", provider.ID, key)
		}
	}
	if provider.Credential.Kind != "" && provider.Credential.Kind != "env" && provider.Credential.Kind != "keyring" {
		return fmt.Errorf("provider %q has invalid credential kind", provider.ID)
	}
	if provider.Credential.Kind != "" && provider.Credential.Name == "" {
		return fmt.Errorf("provider %q credential name is required", provider.ID)
	}
	return nil
}

func (a AdapterID) Supports(protocol WireProtocol) bool {
	switch a {
	case AdapterOpenAI, AdapterOpenAICompatible:
		return protocol == ProtocolOpenAIChat || protocol == ProtocolOpenAIResponses
	default:
		return false
	}
}

func normalizeProvider(provider Provider) Provider {
	provider = cloneProvider(provider)
	for id, descriptor := range provider.Models {
		// Unknown pricing must remain explicit rather than becoming a fabricated $0.
		if descriptor.Pricing.Provenance == "" {
			descriptor.Pricing.Provenance = descriptor.Provenance
		}
		provenance := &descriptor.MetadataProvenance
		if provenance.CanonicalID == "" {
			provenance.CanonicalID = descriptor.Provenance
		}
		if provenance.WireID == "" {
			provenance.WireID = descriptor.Provenance
		}
		if provenance.Limits == "" {
			provenance.Limits = descriptor.Provenance
		}
		if provenance.Capabilities == "" {
			provenance.Capabilities = descriptor.Provenance
		}
		if provenance.Pricing == "" {
			provenance.Pricing = descriptor.Pricing.Provenance
		}
		provider.Models[id] = descriptor
	}
	return provider
}

func cloneProvider(provider Provider) Provider {
	if provider.Models == nil {
		return provider
	}
	models := make(map[string]Model, len(provider.Models))
	for id, model := range provider.Models {
		model.Capabilities.ReasoningEfforts = append(
			[]string(nil),
			model.Capabilities.ReasoningEfforts...,
		)
		models[id] = model
	}
	provider.Models = models
	return provider
}
