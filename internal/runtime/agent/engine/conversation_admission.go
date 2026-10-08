package engine

// Directory recovery is a context precondition, enforced inside the existing
// tool execution lifecycle. It does not grant tools any additional authority.
func conversationRecoveryTool(name string) bool {
	switch name {
	case "turn_history", "result_get", "update_plan", "request_user_input":
		return true
	default:
		return false
	}
}

func (e *Engine) referenceRecoveryOnly() bool {
	scope := e.executionScope()
	if scope == nil {
		return false
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	return scope.state.referenceRecoveryOnly
}
