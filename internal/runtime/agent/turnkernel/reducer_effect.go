package turnkernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

func applyEffectStarted(
	transition *Transition,
	current State,
	command EffectStarted,
) error {
	effect, ok := current.PendingEffects[command.EffectID]
	if !ok {
		return illegal(current, command, "effect is not pending")
	}
	if effect.Status != EffectRequested || command.Attempt == 0 {
		return illegal(current, command, "effect cannot start")
	}
	effect.Status = EffectRunning
	effect.Attempt = command.Attempt
	effect.Error = ""
	transition.State.PendingEffects[command.EffectID] = effect
	if effect.Kind == EffectSampleProvider {
		sample, ok := current.SampleLedger[effect.CallID]
		if !ok || sample.Status != SampleRequested {
			return illegal(current, command, "sample effect is not requested")
		}
		sample.Status = SampleRunning
		sample.Attempt = command.Attempt
		sample.Retry = nil
		sample.RetryBudget.WaitUntil = time.Time{}
		sample.Error = ""
		transition.State.SampleLedger[effect.CallID] = sample
		transition.State.ActiveSampleID = effect.CallID
	}
	transition.Events = append(transition.Events, Event{
		Kind: EventEffectStarted, EffectID: command.EffectID,
	})
	return nil
}

func applyEffectRequeued(
	transition *Transition,
	current State,
	command EffectRequeued,
) error {
	effect, ok := current.PendingEffects[command.EffectID]
	if !ok {
		return illegal(current, command, "effect is not pending")
	}
	if effect.Status != EffectRunning {
		return illegal(current, command, "only a running effect can be requeued")
	}
	effect.Status = EffectRequested
	transition.State.PendingEffects[command.EffectID] = effect
	if effect.Kind == EffectSampleProvider {
		sample, ok := current.SampleLedger[effect.CallID]
		if !ok || sample.Status != SampleRunning {
			return illegal(current, command, "running sample effect has no sample")
		}
		sample.Status = SampleRequested
		transition.State.SampleLedger[effect.CallID] = sample
		transition.State.ActiveSampleID = ""
	}
	transition.Events = append(transition.Events, Event{
		Kind: EventEffectRequeued, EffectID: command.EffectID,
	})
	return nil
}

func applyEffectResult(
	transition *Transition,
	current State,
	command EffectResultReceived,
) error {
	if err := finishEffect(
		transition,
		command.EffectID,
		command.Success,
		command.Error,
	); err != nil {
		return illegal(current, command, err.Error())
	}
	return nil
}

func applyPersistenceResult(
	transition *Transition,
	current State,
	command PersistenceResultReceived,
) error {
	if err := finishEffect(
		transition,
		command.EffectID,
		command.Success,
		command.Error,
	); err != nil {
		return illegal(current, command, err.Error())
	}
	return nil
}

func requestEffect(
	transition *Transition,
	kind EffectKind,
	payload any,
	idempotencyKey string,
	callID string,
) Effect {
	transition.State.NextEffectSequence++
	encoded, err := json.Marshal(payload)
	if err != nil {
		encoded = fmt.Appendf(nil, "%T", payload)
	}
	sum := sha256.Sum256(encoded)
	effect := Effect{
		ID: fmt.Sprintf(
			"effect-%016x",
			transition.State.NextEffectSequence,
		),
		Kind:           kind,
		Payload:        append(json.RawMessage(nil), encoded...),
		PayloadDigest:  "sha256:" + hex.EncodeToString(sum[:]),
		IdempotencyKey: idempotencyKey,
		Status:         EffectRequested,
		CallID:         callID,
	}
	transition.State.PendingEffects[effect.ID] = effect
	transition.Effects = append(transition.Effects, effect)
	transition.Events = append(transition.Events, Event{
		Kind: EventEffectRequested, EffectID: effect.ID, CallID: callID,
	})
	return effect
}

func finishEffect(
	transition *Transition,
	effectID string,
	success bool,
	message string,
) error {
	effect, ok := transition.State.PendingEffects[effectID]
	if !ok {
		if _, completed := transition.State.CompletedEffects[effectID]; completed {
			return errors.New("effect result is duplicated")
		}
		return errors.New("effect result does not match a pending effect")
	}
	if effect.Status != EffectRequested && effect.Status != EffectRunning {
		return errors.New("effect is not awaiting a result")
	}
	if !success && strings.TrimSpace(message) == "" {
		return errors.New("failed effect result has no error")
	}
	effect.Status = EffectSucceeded
	effect.Error = ""
	if !success {
		effect.Status = EffectFailed
		effect.Error = message
	}
	delete(transition.State.PendingEffects, effectID)
	transition.State.CompletedEffects[effectID] = effect
	transition.Events = append(transition.Events, Event{
		Kind: EventEffectFinished, EffectID: effect.ID, CallID: effect.CallID,
	})
	return nil
}

func closeEffectByCall(
	transition *Transition,
	kind EffectKind,
	callID string,
	success bool,
	message string,
) {
	for _, effectID := range sortedEffectIDs(transition.State.PendingEffects) {
		effect := transition.State.PendingEffects[effectID]
		if effect.Kind == kind && effect.CallID == callID {
			_ = finishEffect(transition, effectID, success, nonEmptyEffectError(success, message))
			return
		}
	}
}

func closeEffectByIdentity(
	transition *Transition,
	kind EffectKind,
	identity string,
	success bool,
	message string,
) {
	wantKey := ""
	switch kind {
	case EffectAwaitApproval:
		wantKey = "approval:" + identity
	case EffectAwaitInput:
		wantKey = "input:" + identity
	}
	for _, effectID := range sortedEffectIDs(transition.State.PendingEffects) {
		effect := transition.State.PendingEffects[effectID]
		if effect.Kind == kind && effect.IdempotencyKey == wantKey {
			_ = finishEffect(transition, effectID, success, nonEmptyEffectError(success, message))
			return
		}
	}
}

func closeFirstEffectByKind(
	transition *Transition,
	kind EffectKind,
	success bool,
	message string,
) {
	for _, effectID := range sortedEffectIDs(transition.State.PendingEffects) {
		if transition.State.PendingEffects[effectID].Kind == kind {
			_ = finishEffect(transition, effectID, success, nonEmptyEffectError(success, message))
			return
		}
	}
}

func closeEffectsByKind(
	transition *Transition,
	kind EffectKind,
	success bool,
	message string,
) {
	for _, effectID := range sortedEffectIDs(transition.State.PendingEffects) {
		if transition.State.PendingEffects[effectID].Kind == kind {
			_ = finishEffect(transition, effectID, success, nonEmptyEffectError(success, message))
		}
	}
}

func sortedEffectIDs(effects map[string]Effect) []string {
	ids := make([]string, 0, len(effects))
	for effectID := range effects {
		ids = append(ids, effectID)
	}
	slices.Sort(ids)
	return ids
}

func nonEmptyEffectError(success bool, message string) string {
	if success || strings.TrimSpace(message) != "" {
		return message
	}
	return "effect failed"
}
