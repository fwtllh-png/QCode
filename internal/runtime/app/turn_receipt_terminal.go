package app

import (
	"errors"
	"fmt"
	"time"

	"github.com/fwtllh-png/QCode/internal/observability/trace"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func validateTerminalReceipt(
	receipt *protocol.ExecutionReceiptData,
	completed bool,
) error {
	if receipt == nil {
		return nil
	}
	if !completed {
		if receipt.Outcome != "" {
			return fmt.Errorf(
				"failed turn receipt carries success outcome %q",
				receipt.Outcome,
			)
		}
		return nil
	}
	want := protocol.OutcomeForIntent(receipt.Intent)
	if receipt.Outcome != want {
		return fmt.Errorf(
			"completed turn receipt outcome %q does not match intent %q",
			receipt.Outcome,
			receipt.Intent,
		)
	}
	if protocol.NormalizeTurnIntent(receipt.Intent) !=
		protocol.TurnIntentWorkspaceChange {
		return nil
	}
	if len(receipt.Changes) == 0 {
		return errors.New(
			"completed workspace_change receipt has no observed changes",
		)
	}
	if receipt.WorkspaceOutcome == nil ||
		receipt.WorkspaceOutcome.Status != "changed" {
		return errors.New(
			"completed workspace_change receipt has no changed workspace outcome",
		)
	}
	return nil
}

func freezeTerminalMeasurement(
	frozen trace.FrozenMeasurement,
	usage turnkernel.UsageState,
) (turnkernel.TerminalMeasurementSnapshot, error) {
	frozenAt := frozen.FrozenAt
	var latency *turnkernel.TerminalLatencyMeasurement
	if frozen.Recorded {
		latency = &turnkernel.TerminalLatencyMeasurement{
			Turn:         durationMeasurement(frozen.Latency.Total),
			Provider:     durationMeasurement(frozen.Latency.Provider),
			Tool:         durationMeasurement(frozen.Latency.Tool),
			ApprovalWait: durationMeasurement(frozen.Latency.ApprovalWait),
			Verification: durationMeasurement(frozen.Latency.Verify),
		}
		if frozen.Latency.FirstToken != nil {
			latency.FirstOutput = durationMeasurement(
				*frozen.Latency.FirstToken,
			)
		}
	} else {
		frozenAt = time.Now().UTC()
	}
	return turnkernel.NewTerminalMeasurementSnapshot(
		frozenAt,
		latency,
		usage,
		true,
	)
}

func durationMeasurement(value time.Duration) turnkernel.DurationMeasurement {
	return turnkernel.DurationMeasurement{
		Recorded:     true,
		Milliseconds: value.Milliseconds(),
	}
}

func terminalTraceStatus(terminal protocol.EventData) trace.Status {
	switch terminal.(type) {
	case *protocol.TurnCompletedData:
		return trace.StatusOK
	case *protocol.TurnCanceledData:
		return trace.StatusCanceled
	default:
		return trace.StatusError
	}
}
