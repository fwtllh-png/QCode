// Package guardian defines the data contracts, strict output validation,
// and local decision table for model-based auto review. It depends only
// on the standard library and the security vocabulary package; it does
// not invoke providers, execute tools, or make policy decisions.
//
// The design contract is documented in docs/zh-CN/guardian-auto-review-design.md.
package guardian

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RiskLevel is the model-assessed risk of the specific operation.
type RiskLevel string

const (
	RiskLow      RiskLevel = "low"
	RiskMedium   RiskLevel = "medium"
	RiskHigh     RiskLevel = "high"
	RiskCritical RiskLevel = "critical"
)

func (r RiskLevel) Valid() bool {
	switch r {
	case RiskLow, RiskMedium, RiskHigh, RiskCritical:
		return true
	}
	return false
}

// Authorization reports whether the user has effectively requested
// this operation. "supported" requires at least one valid user source;
// it does not satisfy Fresh or explicit Ask requirements.
type Authorization string

const (
	AuthSupported   Authorization = "supported"
	AuthUnknown     Authorization = "unknown"
	AuthConflicting Authorization = "conflicting"
)

func (a Authorization) Valid() bool {
	switch a {
	case AuthSupported, AuthUnknown, AuthConflicting:
		return true
	}
	return false
}

// Recommendation is the model's suggested action, not the final policy
// decision. The local decision table consumes risk, authorization, and
// recommendation together, never recommendation alone.
type Recommendation string

const (
	RecommendAllow  Recommendation = "allow"
	RecommendPrompt Recommendation = "prompt"
)

func (r Recommendation) Valid() bool {
	switch r {
	case RecommendAllow, RecommendPrompt:
		return true
	}
	return false
}

// Assessment is the strictly validated structured output from one model
// review (design §6.1). Parsing rejects unknown fields, duplicate keys,
// unknown enum values, missing required fields, invalid source references,
// extra text, and incomplete responses.
type Assessment struct {
	RiskLevel              RiskLevel      `json:"risk_level"`
	Authorization          Authorization  `json:"authorization"`
	AuthorizationSourceIDs []string       `json:"authorization_source_ids,omitempty"`
	Recommendation         Recommendation `json:"recommendation"`
	Rationale              string         `json:"rationale"`
}

// knownFields are the only JSON keys permitted in a valid assessment.
var knownFields = map[string]bool{
	"risk_level": true, "authorization": true,
	"authorization_source_ids": true, "recommendation": true, "rationale": true,
}

// ParseAssessment strictly parses the model output. It returns an error
// for any deviation from the contract; callers must treat parse failure
// as "fall back to human approval", never as Allow.
func ParseAssessment(data []byte, validSourceIDs map[string]bool) (Assessment, error) {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return Assessment{}, fmt.Errorf("guardian output is not a JSON object: %w", err)
	}
	// Reject unknown top-level keys.
	for key := range raw {
		if !knownFields[key] {
			return Assessment{}, fmt.Errorf("guardian output has unknown field %q", key)
		}
	}
	var assessment Assessment
	if v, ok := raw["risk_level"]; ok {
		if err := json.Unmarshal(v, &assessment.RiskLevel); err != nil ||
			!assessment.RiskLevel.Valid() {
			return Assessment{}, fmt.Errorf("guardian risk_level is invalid: %s", string(v))
		}
	} else {
		return Assessment{}, fmt.Errorf("guardian output is missing risk_level")
	}
	if v, ok := raw["authorization"]; ok {
		if err := json.Unmarshal(v, &assessment.Authorization); err != nil ||
			!assessment.Authorization.Valid() {
			return Assessment{}, fmt.Errorf("guardian authorization is invalid: %s", string(v))
		}
	} else {
		return Assessment{}, fmt.Errorf("guardian output is missing authorization")
	}
	if v, ok := raw["recommendation"]; ok {
		if err := json.Unmarshal(v, &assessment.Recommendation); err != nil ||
			!assessment.Recommendation.Valid() {
			return Assessment{}, fmt.Errorf("guardian recommendation is invalid: %s", string(v))
		}
	} else {
		return Assessment{}, fmt.Errorf("guardian output is missing recommendation")
	}
	if v, ok := raw["rationale"]; ok {
		if err := json.Unmarshal(v, &assessment.Rationale); err != nil {
			return Assessment{}, fmt.Errorf("guardian rationale is not a string: %s", string(v))
		}
	}
	// Always parse source IDs so the non-supported check can see them.
	if v, ok := raw["authorization_source_ids"]; ok {
		if err := json.Unmarshal(v, &assessment.AuthorizationSourceIDs); err != nil {
			return Assessment{}, fmt.Errorf("guardian authorization_source_ids is invalid: %s", string(v))
		}
	}
	// "supported" must cite at least one still-valid user source.
	if assessment.Authorization == AuthSupported {
		if len(assessment.AuthorizationSourceIDs) == 0 {
			return Assessment{}, fmt.Errorf("guardian authorization=supported requires authorization_source_ids")
		}
		if validSourceIDs != nil {
			for _, id := range assessment.AuthorizationSourceIDs {
				if !validSourceIDs[id] {
					return Assessment{}, fmt.Errorf("guardian cites unknown or revoked source %q", id)
				}
			}
		}
	}
	// "prompt" or non-supported authorization must not cite sources.
	if assessment.Authorization != AuthSupported && len(assessment.AuthorizationSourceIDs) > 0 {
		return Assessment{}, fmt.Errorf("guardian cites sources without authorization=supported")
	}
	return assessment, nil
}
