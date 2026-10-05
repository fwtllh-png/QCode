package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

type RouteRequest struct {
	ProviderID string
	ModelID    string
	Provenance Provenance
	Auto       bool
	// Require are capabilities the resolved model must have. They filter rather
	// than select: Auto still needs a unique model-id match, and an explicit
	// provider/model that lacks a required bit is refused. Picking "any model
	// with vision" is exactly the complexity router D2 declines to build.
	Require []Capability
}
type ReadyRoute struct {
	providerID, endpoint string
	adapter              AdapterID
	protocol             WireProtocol
	credential           CredentialRef
	model                Model
	provenance           Provenance
	ready                bool
}
type RouteDescriptor struct {
	ProviderID string        `json:"provider_id"`
	Adapter    AdapterID     `json:"adapter"`
	Endpoint   string        `json:"endpoint"`
	Protocol   WireProtocol  `json:"protocol"`
	Credential CredentialRef `json:"credential"`
	Model      Model         `json:"model"`
	Provenance Provenance    `json:"provenance"`
}

type RouteIdentity struct {
	ProviderID string        `json:"provider_id"`
	Adapter    AdapterID     `json:"adapter"`
	Endpoint   string        `json:"endpoint"`
	Protocol   WireProtocol  `json:"protocol"`
	Credential CredentialRef `json:"credential"`
	ModelID    string        `json:"model_id"`
	WireID     string        `json:"wire_id"`
}

func RouteKey(providerID, modelID string) string { return providerID + "\x00" + modelID }

func (r ReadyRoute) ProviderID() string        { return r.providerID }
func (r ReadyRoute) Adapter() AdapterID        { return r.adapter }
func (r ReadyRoute) Endpoint() string          { return r.endpoint }
func (r ReadyRoute) Protocol() WireProtocol    { return r.protocol }
func (r ReadyRoute) Credential() CredentialRef { return r.credential }

// ConnectionID isolates observations by the complete non-secret provider
// transport identity. Model IDs alone are not portable across endpoints.
func (r ReadyRoute) ConnectionID() string {
	endpoint := strings.TrimRight(r.endpoint, "/")
	if parsed, err := url.Parse(endpoint); err == nil {
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		parsed.Host = strings.ToLower(parsed.Host)
		endpoint = strings.TrimRight(parsed.String(), "/")
	}
	sum := sha256.Sum256([]byte(
		r.providerID + "\x00" + string(r.adapter) + "\x00" +
			endpoint + "\x00" + string(r.protocol),
	))
	return "connection_" + hex.EncodeToString(sum[:16])
}

// WithCredential selects an explicit non-secret workspace credential reference.
func (r ReadyRoute) WithCredential(reference CredentialRef) ReadyRoute {
	r.credential = reference
	return r
}

func (r ReadyRoute) WithModelID(id string) ReadyRoute {
	r.model.ID, r.model.CanonicalID, r.model.WireID = id, id, id
	r.model.Pricing = Pricing{}
	r.model.Provenance, r.model.MetadataProvenance.CanonicalID, r.model.MetadataProvenance.WireID, r.model.MetadataProvenance.Pricing = ProvenanceStartup, ProvenanceStartup, ProvenanceStartup, ProvenanceStartup
	return r
}
func (r ReadyRoute) Model() Model           { return r.model }
func (r ReadyRoute) Provenance() Provenance { return r.provenance }

// WithProvenance records why a route was selected without changing its
// connection identity or model metadata provenance.
func (r ReadyRoute) WithProvenance(provenance Provenance) ReadyRoute {
	r.provenance = provenance
	return r
}

func (r ReadyRoute) Identity() (RouteIdentity, error) {
	if err := r.Validate(); err != nil {
		return RouteIdentity{}, err
	}
	return RouteIdentity{
		ProviderID: r.providerID,
		Adapter:    r.adapter,
		Endpoint:   r.endpoint,
		Protocol:   r.protocol,
		Credential: r.credential,
		ModelID:    r.model.ID,
		WireID:     r.model.WireID,
	}, nil
}

func (r ReadyRoute) Describe() (RouteDescriptor, error) {
	if err := r.Validate(); err != nil {
		return RouteDescriptor{}, err
	}
	return RouteDescriptor{
		ProviderID: r.providerID,
		Adapter:    r.adapter,
		Endpoint:   r.endpoint,
		Protocol:   r.protocol,
		Credential: r.credential,
		Model:      r.model,
		Provenance: r.provenance,
	}, nil
}

func (r ReadyRoute) Validate() error {
	if !r.ready {
		return errors.New("route was not produced by resolver")
	}
	if r.providerID == "" || r.endpoint == "" || r.model.ID == "" ||
		!r.adapter.Supports(r.protocol) {
		return errors.New("ready route is incomplete")
	}
	return nil
}

type Resolver struct {
	catalog *Catalog
}

func NewResolver(catalog *Catalog) (*Resolver, error) {
	if catalog == nil {
		return nil, errors.New("model catalog is required")
	}
	return &Resolver{catalog: catalog}, nil
}

func (r *Resolver) Resolve(request RouteRequest) (ReadyRoute, error) {
	if request.ProviderID == "" {
		if !request.Auto {
			return ReadyRoute{}, errors.New("provider id is required; enable auto routing explicitly")
		}
		var matches []string
		for _, candidate := range r.catalog.Providers() {
			model, exists := candidate.Models[request.ModelID]
			if !exists {
				continue
			}
			// Capability filter runs before uniqueness: a second provider that
			// offers the model without the required bits must not turn a unique
			// capable match into "found 2".
			if len(model.Capabilities.MissingCapabilities(request.Require)) != 0 {
				continue
			}
			matches = append(matches, candidate.ID)
		}
		if len(matches) != 1 {
			return ReadyRoute{}, fmt.Errorf(
				"auto route for model %q requires exactly one provider; found %d",
				request.ModelID,
				len(matches),
			)
		}
		request.ProviderID = matches[0]
	}
	if request.ModelID == "" {
		return ReadyRoute{}, errors.New("model id is required")
	}
	provider, exists := r.catalog.Provider(request.ProviderID)
	if !exists {
		return ReadyRoute{}, fmt.Errorf("unknown provider %q", request.ProviderID)
	}
	model, exists := provider.Models[request.ModelID]
	if !exists {
		return ReadyRoute{}, fmt.Errorf("provider %q does not offer model %q", provider.ID, request.ModelID)
	}
	if err := RequireCapabilities(model.ID, model.Capabilities, request.Require); err != nil {
		return ReadyRoute{}, err
	}
	provenance := request.Provenance
	if provenance == "" {
		provenance = provider.Provenance
	}
	return ReadyRoute{
		providerID: provider.ID,
		adapter:    provider.Adapter,
		endpoint:   provider.Endpoint,
		protocol:   provider.Protocol,
		credential: provider.Credential,
		model:      model,
		provenance: provenance,
		ready:      true,
	}, nil
}
