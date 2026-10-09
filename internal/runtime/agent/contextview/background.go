package contextview

import "github.com/fwtllh-png/QCode/internal/adapter/provider"

// WithTurnBackground places freshly projected references and omission hints
// before the current user request. They stay at that boundary throughout a
// tool loop without rewriting earlier turns or entering durable history.
func WithTurnBackground(history, background []provider.Message, turn uint64) []provider.Message {
	at := 0
	for i, message := range history {
		if message.Turn == turn && message.Role == provider.RoleUser {
			at = i
			break
		}
	}
	result := make([]provider.Message, 0, len(history)+len(background))
	result = append(result, history[:at]...)
	result = append(result, background...)
	return append(result, history[at:]...)
}
