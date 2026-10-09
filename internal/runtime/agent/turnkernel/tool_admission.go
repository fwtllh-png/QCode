package turnkernel

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func applyToolBatchAdmissionRejection(transition *Transition, current State, command ToolBatchAdmissionRejected) error {
	if err := requirePhase(current, command, PhaseExecutingTools); err != nil {
		return err
	}
	var result tool.Result
	if len(command.CallIDs) == 0 || json.Unmarshal([]byte(command.Result), &result) != nil ||
		!result.IsError || strings.TrimSpace(result.Content) == "" || len(ObservedFileChanges(result)) != 0 {
		return illegal(current, command, "invalid admission rejection")
	}
	seen := map[string]bool{}
	for _, id := range command.CallIDs {
		call, ok := current.OpenCalls[id]
		if !ok || seen[id] || call.AdmissionRejection != "" && call.AdmissionRejection != command.Result {
			return illegal(current, command, "admission rejection does not match open calls")
		}
		if call.AdmissionRejection == "" {
			for _, effect := range current.PendingEffects {
				if effect.CallID == id && effect.Status != EffectRequested {
					return illegal(current, command, "cannot reject a started effect")
				}
			}
		}
		seen[id] = true
		call.AdmissionRejection = command.Result
		transition.State.OpenCalls[id] = call
	}
	return nil
}

func (s *RuntimeKernel) recordToolAdmissionRejection(calls []provider.ToolCall, result tool.Result) error {
	if err := s.StartTools(calls); err != nil {
		return err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	ids := make([]string, len(calls))
	for i, call := range calls {
		ids[i] = call.ID
	}
	return s.applyAuthoritative(ToolBatchAdmissionRejected{CallIDs: ids, Result: string(encoded)})
}

func (s *RuntimeKernel) toolAdmissionRejections(calls []provider.ToolCall) (map[string]tool.Result, []provider.ToolCall, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rejected := make(map[string]tool.Result)
	var undecided []provider.ToolCall
	for _, call := range calls {
		raw := s.state.OpenCalls[call.ID].AdmissionRejection
		if raw == "" {
			undecided = append(undecided, call)
			continue
		}
		var result tool.Result
		if err := json.Unmarshal([]byte(raw), &result); err != nil || !result.IsError {
			return nil, nil, errors.New("invalid durable tool admission rejection")
		}
		rejected[call.ID] = result
	}
	return rejected, undecided, nil
}
