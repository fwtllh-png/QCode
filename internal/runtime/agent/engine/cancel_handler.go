package engine

import (
	"context"
	"errors"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const maxMailboxBacklog = 2 * turnkernel.DefaultMailboxCapacity

// EnqueueMailbox queues an inter-agent mailbox message.
// triggerTurn=true injects into the current turn (cancels sampling, like Steer).
// triggerTurn=false buffers until the next turn begins (avoids late-mail pollution).
func (e *Engine) EnqueueMailbox(prompt string, triggerTurn bool) error {
	if prompt == "" {
		return errors.New("mailbox prompt is required")
	}
	item := PendingInput{Source: PendingMailbox, Prompt: prompt, TriggerTurn: triggerTurn}
	if !triggerTurn {
		return e.holdMailbox(item)
	}
	scope := e.runningScope()
	if scope == nil {
		return e.holdMailbox(item)
	}
	scope.mu.Lock()
	if scope.state.steeringClosed {
		scope.mu.Unlock()
		return e.holdMailbox(item)
	}
	err := scope.state.mailbox.Offer(item)
	cancel := scope.state.sampleCancel
	scope.mu.Unlock()
	if err != nil {
		return err
	}
	if cancel != nil {
		cancel(errors.New("mailbox input"))
	}
	return nil
}

func (e *Engine) holdMailbox(item PendingInput) error {
	e.scopeMu.Lock()
	defer e.scopeMu.Unlock()
	if len(e.mailboxHold) >= maxMailboxBacklog {
		return protocol.NewProblem(
			protocol.CodeResourceExhausted,
			"turn mailbox backlog is full",
			true,
			nil,
		)
	}
	e.mailboxHold = append(e.mailboxHold, item)
	return nil
}

func (e *Engine) appendSteering(history *[]provider.Message) bool {
	pending := e.drainPending()
	e.appendPendingInputs(history, pending)
	return len(pending) != 0
}

func (e *Engine) drainPending() []PendingInput {
	scope := e.executionScope()
	if scope == nil {
		return nil
	}
	scope.mu.Lock()
	pending := scope.state.mailbox.Drain()
	scope.mu.Unlock()
	return pending
}

func (e *Engine) appendPendingInputs(history *[]provider.Message, pending []PendingInput) {
	for _, item := range pending {
		text := item.Prompt
		if item.Source == PendingMailbox {
			text = "[mailbox] " + item.Prompt
		}
		message := provider.TextMessage(provider.RoleUser, text)
		message.Turn = e.turn
		*history = append(*history, message)
	}
}

func (e *Engine) setSampleCancel(cancel context.CancelCauseFunc) {
	e.setCancelSlot(func(state *scopeState) { state.sampleCancel = cancel })
}

func (e *Engine) clearSampleCancel() {
	e.setCancelSlot(func(state *scopeState) { state.sampleCancel = nil })
}

func (e *Engine) setToolCancel(cancel context.CancelCauseFunc) {
	e.setCancelSlot(func(state *scopeState) { state.toolCancel = cancel })
}

func (e *Engine) clearToolCancel() {
	e.setCancelSlot(func(state *scopeState) { state.toolCancel = nil })
}

func (e *Engine) setCancelSlot(update func(*scopeState)) {
	scope := e.runningScope()
	if scope == nil {
		return
	}
	scope.mu.Lock()
	update(&scope.state)
	scope.mu.Unlock()
}

func (e *Engine) cancellationReason() string {
	scope := e.executionScope()
	if scope == nil {
		return ""
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	return scope.state.cancelReason
}

func retainCanceledHistory(messages []provider.Message) []provider.Message {
	retained, _, err := agentcontext.NormalizePairs(messages)
	if err != nil {
		return nil
	}
	return retained
}

// retainedFailedTurnExchanges keeps the failed Turn's closed exchanges so a
// Turn that explored for several rounds before failing does not lose its
// completed evidence. Dangling tool traffic from the failing batch is dropped
// by pair normalization; the prior durable history is appended separately by
// the caller and must not be double-projected here.
func retainedFailedTurnExchanges(
	transaction []provider.Message,
	turn uint64,
) []provider.Message {
	var currentTurn []provider.Message
	for _, message := range transaction {
		if message.Turn == turn {
			currentTurn = append(currentTurn, message)
		}
	}
	return retainCanceledHistory(currentTurn)
}
