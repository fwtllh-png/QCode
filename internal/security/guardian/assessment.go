// Package guardian defines the data contracts, strict output validation,
// and local decision table for model-based auto review. It depends only
// on the standard library and the security vocabulary package; it does
// not invoke providers, execute tools, or make policy decisions.
//
// The design contract is documented in docs/zh-CN/guardian-auto-review-design.md.
package guardian

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
//
// Strictness guarantees:
//   - The input must be exactly one JSON object with no trailing content.
//   - Duplicate keys are rejected (the first occurrence wins in a map,
//     silently masking the second; we detect this by counting tokens).
//   - rationale is required and must be a non-null string.
//   - authorization_source_ids is always validated against the provided
//     set; a nil set means no sources are known, so all citations fail.
func ParseAssessment(data []byte, validSourceIDs map[string]bool) (Assessment, error) {
	// Phase 1: tokenize to detect duplicate keys and trailing content.
	if err := validateJSONShape(data); err != nil {
		return Assessment{}, err
	}
	// Phase 2: decode into a raw map for field-level validation.
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&raw); err != nil {
		return Assessment{}, fmt.Errorf("guardian output is not a JSON object: %w", err)
	}
	// Reject unknown top-level keys.
	for key := range raw {
		if !knownFields[key] {
			return Assessment{}, fmt.Errorf("guardian output has unknown field %q", key)
		}
	}
	// Phase 3: validate each required field.
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
	// rationale is required and must be a non-null string.
	if v, ok := raw["rationale"]; ok {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return Assessment{}, fmt.Errorf("guardian rationale is null")
		}
		if err := json.Unmarshal(v, &assessment.Rationale); err != nil {
			return Assessment{}, fmt.Errorf("guardian rationale is not a string: %s", string(v))
		}
	} else {
		return Assessment{}, fmt.Errorf("guardian output is missing rationale")
	}
	// Always parse source IDs so the non-supported check can see them.
	if v, ok := raw["authorization_source_ids"]; ok {
		if err := json.Unmarshal(v, &assessment.AuthorizationSourceIDs); err != nil {
			return Assessment{}, fmt.Errorf("guardian authorization_source_ids is invalid: %s", string(v))
		}
	}
	// "supported" must cite at least one still-valid user source.
	// validSourceIDs is always checked: a nil map returns false for every
	// key, correctly rejecting all citations when no sources are known.
	if assessment.Authorization == AuthSupported {
		if len(assessment.AuthorizationSourceIDs) == 0 {
			return Assessment{}, fmt.Errorf("guardian authorization=supported requires authorization_source_ids")
		}
		for _, id := range assessment.AuthorizationSourceIDs {
			if !validSourceIDs[id] {
				return Assessment{}, fmt.Errorf("guardian cites unknown or revoked source %q", id)
			}
		}
	}
	// Non-supported authorization must not cite sources.
	if assessment.Authorization != AuthSupported && len(assessment.AuthorizationSourceIDs) > 0 {
		return Assessment{}, fmt.Errorf("guardian cites sources without authorization=supported")
	}
	return assessment, nil
}

// validateJSONShape tokenizes the input to detect duplicate keys and
// ensure the input contains exactly one complete JSON object with no
// trailing content. It does not validate field values.
func validateJSONShape(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	// Expect the opening brace of the object.
	token, err := dec.Token()
	if err != nil {
		return fmt.Errorf("guardian output is not valid JSON: %w", err)
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("guardian output is not a JSON object")
	}
	seen := make(map[string]bool)
	for dec.More() {
		// Read the key.
		keyToken, err := dec.Token()
		if err != nil {
			return fmt.Errorf("guardian output has malformed object: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("guardian object key is not a string")
		}
		if seen[key] {
			return fmt.Errorf("guardian output has duplicate key %q", key)
		}
		seen[key] = true
		// Skip the value (we only care about key uniqueness here).
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return fmt.Errorf("guardian output has malformed value for %q: %w", key, err)
		}
	}
	// Expect the closing brace.
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("guardian object is not closed: %w", err)
	}
	// Reject any trailing content after the object.
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("guardian output has trailing content after the JSON object")
	}
	return nil
}
