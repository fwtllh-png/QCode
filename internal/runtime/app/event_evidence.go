package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// evidenceReplayPage is the replay page size for evidence scans. It only
// bounds one read; scans continue until the committed head.
const evidenceReplayPage = 1000

// ToolOutputClaim asserts that a Thread's tool call produced Output, whose
// hex SHA-256 is Digest. Hosts use it to accept client-supplied tool output
// only when the Runtime actually issued it.
type ToolOutputClaim struct {
	CallID string
	Digest string
	Output string
}

// ThreadDiagnostic is one diagnostics receipt committed on a Thread.
type ThreadDiagnostic struct {
	CallID  string
	Tool    string
	Receipt protocol.DiagnosticReceipt
}

// ToolOutputsIssued reports whether every claim matches a committed
// tool.result on threadID by call ID, output digest, and exact output.
func (r *EventService) ToolOutputsIssued(
	ctx context.Context,
	threadID protocol.ThreadID,
	claims []ToolOutputClaim,
) (bool, error) {
	pending := make(map[string]string, len(claims))
	for _, claim := range claims {
		pending[claim.CallID+"\x00"+claim.Digest] = claim.Output
	}
	if len(pending) == 0 {
		return true, nil
	}
	err := r.eachThreadEvent(ctx, threadID, func(event protocol.Event) bool {
		result, ok := event.Data.(*protocol.ToolResultData)
		if !ok {
			return true
		}
		digest := sha256.Sum256([]byte(result.Output))
		key := result.CallID + "\x00" + hex.EncodeToString(digest[:])
		if output, wanted := pending[key]; wanted && output == result.Output {
			delete(pending, key)
		}
		return len(pending) != 0
	})
	return len(pending) == 0, err
}

// EachThreadDiagnostic visits threadID's committed diagnostics receipts in log
// order. A visit error stops the scan and is returned.
func (r *EventService) EachThreadDiagnostic(
	ctx context.Context,
	threadID protocol.ThreadID,
	visit func(ThreadDiagnostic) error,
) error {
	var visitErr error
	err := r.eachThreadEvent(ctx, threadID, func(event protocol.Event) bool {
		diagnostics, ok := event.Data.(*protocol.DiagnosticsData)
		if !ok {
			return true
		}
		for _, receipt := range diagnostics.Receipts {
			visitErr = visit(ThreadDiagnostic{
				CallID: diagnostics.CallID, Tool: diagnostics.Tool, Receipt: receipt,
			})
			if visitErr != nil {
				return false
			}
		}
		return true
	})
	if visitErr != nil {
		return visitErr
	}
	return err
}

// eachThreadEvent pages committed history without holding Runtime locks and
// stops when visit returns false.
func (r *EventService) eachThreadEvent(
	ctx context.Context,
	threadID protocol.ThreadID,
	visit func(protocol.Event) bool,
) error {
	cursor := protocol.Cursor(0)
	for {
		events, more, err := r.runtime.ReplayEvents(ctx, cursor, evidenceReplayPage)
		if err != nil {
			return err
		}
		for _, event := range events {
			cursor = event.Sequence
			if event.ThreadID == threadID && !visit(event) {
				return nil
			}
		}
		if !more || len(events) == 0 {
			return nil
		}
	}
}
