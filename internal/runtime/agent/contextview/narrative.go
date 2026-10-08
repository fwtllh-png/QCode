package contextview

import (
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

// SelectNarrative admits a complete cached representation only while its
// sources remain valid and absent from the selected raw/extractive context.
// A multi-source item is indivisible: partial overlap omits the candidate,
// preserving coverage rather than rewriting model-authored text.
func SelectNarrative(artifact *agentcontext.NarrativeArtifact, enabled bool, route, window string,
	history []provider.Message, projection agentcontext.ProjectionResult, now time.Time,
	fits func(provider.Message) (bool, error),
) (*provider.Message, []agentcontext.ReferenceCoverage, error) {
	if artifact == nil {
		return nil, nil, nil
	}
	var rows []agentcontext.ReferenceCoverage
	for _, c := range artifact.Coverage {
		rows = append(rows, agentcontext.ReferenceCoverage{
			SourceID: c.Source.MessageID, ContentDigest: c.Source.Digest,
			Ranges:         []agentcontext.ReferenceRange{{Start: c.Source.Start, End: c.Source.End}},
			Representation: "semantic", Status: "covered",
		})
	}
	omit := func(reason string) (*provider.Message, []agentcontext.ReferenceCoverage, error) {
		for i := range rows {
			rows[i].Status, rows[i].Reason = "omitted", reason
		}
		return nil, rows, nil
	}
	if !enabled {
		return omit("digest_disabled")
	}
	if err := artifact.Validate(now); err != nil {
		return omit("invalid_or_expired")
	}
	if artifact.RouteDigest != route || artifact.WindowID != window {
		return omit("route_or_window_changed")
	}
	current, err := agentcontext.BuildNarrativeInput(artifact.ThreadID, window, artifact.AuthorityDigest,
		route, history, agentcontext.NarrativeLimits{}, now, artifact.ExpiresAt.Sub(now))
	if err != nil {
		return omit("source_unavailable")
	}
	raw := map[string]bool{}
	indices := map[uint64]int{}
	selected := map[int]bool{}
	for _, s := range projection.Selected {
		selected[s.Index] = true
	}
	for i, message := range history {
		if agentcontext.IsWorldStateMessage(message) {
			continue
		}
		index := indices[message.Turn]
		indices[message.Turn]++
		if selected[i] {
			raw[agentcontext.StableMessageID(artifact.ThreadID, message, index)] = true
		}
	}
	for i, c := range artifact.Coverage {
		found := false
		for _, source := range current.Excerpts {
			if source.Source.MessageID == c.Source.MessageID && source.Source.Digest == c.Source.Digest {
				found = true
				rows[i].SourceTurn = source.Turn
				break
			}
		}
		if !found {
			return omit("source_unavailable")
		}
		if raw[c.Source.MessageID] {
			return omit("duplicate_representation")
		}
		for _, reference := range projection.References {
			if reference.ContentDigest == c.Source.Digest {
				return omit("duplicate_representation")
			}
		}
	}
	text, err := agentcontext.RenderNarrativeDigest(*artifact, 0)
	if err != nil || text == "" {
		return omit("invalid_or_empty")
	}
	message := provider.TextMessage(provider.RoleSystem, text)
	ok, err := fits(message)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return omit("context_capacity")
	}
	return &message, rows, nil
}
