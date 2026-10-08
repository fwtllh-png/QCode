package engine

import (
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func (e *Engine) sessionStatePartition(history []provider.Message) (string, error) {
	capsule := agentcontext.MandatorySessionState(
		e.buildTruthCapsule(e.buildCompactSummary(nil), history),
	)
	if len(capsule.Entities) == 0 {
		return "", nil
	}
	rendered, err := agentcontext.RenderSessionState(capsule, e.sessionStateBudget())
	if err != nil {
		return "", err
	}
	return rendered.Text, nil
}

func (e *Engine) sessionStateBudget() int {
	if e.options.Context.TruthRetention.TruthMaxBytes > 0 {
		return e.options.Context.TruthRetention.TruthMaxBytes
	}
	return e.summaryBudget()
}
