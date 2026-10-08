package agentcontext

import "time"

// RenderNarrativeDigest writes the optional rolling digest partition. It is
// not a history replacement. Oversized text is omitted so narrative cannot
// crowd out ledger authority.
func RenderNarrativeDigest(artifact NarrativeArtifact, budget int) (string, error) {
	if err := artifact.Validate(time.Time{}); err != nil {
		return "", err
	}
	lines := artifact.Body.renderLines()
	if len(lines) == 0 {
		return "", nil
	}
	text, _ := renderNarrative(lines, unbounded)
	if budget > 0 && len(text) > budget {
		return "", nil
	}
	return text, nil
}
