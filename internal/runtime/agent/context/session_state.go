package agentcontext

import (
	"encoding/json"
	"fmt"
)

// MandatorySessionState keeps only ledger facts the next sample must still
// see after the projector clips older turns: goal, open todos, pending input,
// unverified or stale changes, confirmed continuity, omitted-turn retrieval,
// and resume. Compact is not required to preserve them.
func MandatorySessionState(capsule TruthCapsule) TruthCapsule {
	result := capsule
	entities := make([]TruthEntity, 0, len(capsule.Entities))
	for _, entity := range capsule.Entities {
		entity.normalizeLifecycle()
		if entity.Retention != RetentionMandatory {
			continue
		}
		entities = append(entities, entity)
	}
	result.Entities = entities
	result.Omissions = nil
	if len(entities) == 0 {
		return result
	}
	result.Seal()
	return result
}

// RenderSessionState writes the live session-state partition. It is not a
// history replacement and does not claim to compact messages.
func RenderSessionState(capsule TruthCapsule, budget int) (StructuredRender, error) {
	if len(capsule.Entities) == 0 {
		return StructuredRender{}, nil
	}
	if err := capsule.Validate(); err != nil {
		return StructuredRender{}, err
	}
	encoded, err := json.Marshal(capsule)
	if err != nil {
		return StructuredRender{}, err
	}
	header := "Current session state from ledgers.\n"
	if hint := SessionStateContinuityHint(capsule); hint != "" {
		header += hint + "\n"
	}
	if hint := SessionStateResumeHint(capsule); hint != "" {
		header += hint + "\n"
	}
	if hint := SessionStateRetrievalHint(capsule); hint != "" {
		header += hint + "\n"
	}
	truthBlock := TruthMarkerStart + "\n" + string(encoded) + "\n" + TruthMarkerEnd + "\n"
	text := MarkerStart + "\n" + header + truthBlock + MarkerEnd + "\n"
	if budget > 0 && len(text) > budget {
		return StructuredRender{}, fmt.Errorf(
			"%w: session state requires %d bytes; budget is %d",
			ErrMandatoryCapacity,
			len(text),
			budget,
		)
	}
	return StructuredRender{
		Text:         text,
		Sections:     []string{SectionTruth},
		CapsuleBytes: len(encoded),
	}, nil
}
