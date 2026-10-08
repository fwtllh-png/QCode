package engine

import (
	"slices"

	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func contextProjectionReceipt(p agentcontext.ProjectionResult, recoveryOnly bool) *protocol.ReceiptContextProjection {
	r := &protocol.ReceiptContextProjection{
		Digest: p.Digest, SourceHistoryDigest: p.SourceHistoryDigest,
		WindowID: p.SourceWindowID, WindowNumber: p.SourceWindowNumber,
		RouteDigest: p.RouteDigest, ContextDigest: p.ContextDigest, ContextRevision: p.ContextRevision,
		InputTokens: p.InputTokens, OutputReserve: p.OutputReserve,
		RawTokens: p.RawTokens, RawTokenLimit: p.RawTokenLimit, RawTokenLimited: p.RawTokenLimited,
		TailStart: p.TailStart, LimitingConstraint: string(p.LimitingConstraint),
		RecoveryOnly: recoveryOnly,
	}
	source := func(s agentcontext.ProjectionSource) protocol.ReceiptContextSource {
		return protocol.ReceiptContextSource{Index: s.Index, Turn: s.Turn, Digest: s.Digest}
	}
	for _, s := range p.Selected {
		r.Selected = append(r.Selected, source(s))
	}
	for _, o := range p.Omissions {
		row := protocol.ReceiptContextOmission{Source: source(o.Source), Reason: string(o.Reason)}
		if o.Retrieval != nil {
			row.RetrievalTurn = o.Retrieval.Turn
		}
		r.Omissions = append(r.Omissions, row)
	}
	for _, c := range append(slices.Clone(p.References), p.Representations...) {
		row := protocol.ReceiptContextRepresentation{
			SourceID: c.SourceID, SourceTurn: c.SourceTurn, ContentDigest: c.ContentDigest,
			ItemIDs: slices.Clone(c.ItemIDs), Representation: c.Representation, Status: "covered",
		}
		if c.Status != "" {
			row.Status = c.Status
		}
		row.Reason = c.Reason
		for _, span := range c.Ranges {
			row.Ranges = append(row.Ranges, protocol.ReceiptContextRange{Start: span.Start, End: span.End})
		}
		r.References = append(r.References, row)
	}
	return r
}
