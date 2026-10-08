package turnkernel

type CommandFamily string

const (
	CommandFamilyLifecycle    CommandFamily = "lifecycle"
	CommandFamilySampling     CommandFamily = "sampling"
	CommandFamilyTool         CommandFamily = "tool"
	CommandFamilyInteraction  CommandFamily = "interaction"
	CommandFamilyVerification CommandFamily = "verification"
	CommandFamilyTerminal     CommandFamily = "terminal"
	CommandFamilyEffect       CommandFamily = "effect"
)

// CommandContract is the machine-readable ownership index for Reducer
// commands. Empty AllowedPhases means the command is fenced by pending effect
// identity or a command-specific state predicate in addition to phase.
type CommandContract struct {
	Name          string
	Family        CommandFamily
	AllowedPhases []Phase
}

var commandContracts = []CommandContract{
	{Name: "start_turn", Family: CommandFamilyLifecycle, AllowedPhases: []Phase{PhaseCreated}},
	{Name: "preparation_finished", Family: CommandFamilyLifecycle, AllowedPhases: []Phase{PhasePreparing}},
	{Name: "model_sample_requested", Family: CommandFamilySampling, AllowedPhases: []Phase{PhaseSampling}},
	{Name: "model_sample_started", Family: CommandFamilySampling, AllowedPhases: []Phase{PhaseSampling}},
	{Name: "model_sample_finished", Family: CommandFamilySampling, AllowedPhases: []Phase{PhaseSampling}},
	{Name: "model_sample_progress_recorded", Family: CommandFamilySampling, AllowedPhases: []Phase{PhaseSampling}},
	{Name: "model_sample_result_received", Family: CommandFamilySampling, AllowedPhases: []Phase{PhaseSampling}},
	{Name: "supplemental_usage_recorded", Family: CommandFamilySampling},
	{Name: "provider_retry_requested", Family: CommandFamilySampling, AllowedPhases: []Phase{PhaseSampling}},
	{Name: "model_text_received", Family: CommandFamilySampling, AllowedPhases: []Phase{PhaseSampling}},
	{Name: "release_provisional_output", Family: CommandFamilySampling, AllowedPhases: []Phase{PhaseSampling}},
	{Name: "discard_provisional_output", Family: CommandFamilySampling, AllowedPhases: []Phase{PhaseSampling}},
	{Name: "repair_requested", Family: CommandFamilySampling, AllowedPhases: []Phase{PhaseSampling}},
	{Name: "evaluate_turn_step", Family: CommandFamilySampling},
	{Name: "observe_progress", Family: CommandFamilySampling, AllowedPhases: []Phase{PhaseSampling}},
	{Name: "bind_work_item", Family: CommandFamilyLifecycle, AllowedPhases: []Phase{PhaseCreated, PhasePreparing, PhaseSampling}},
	{Name: "continuation_recorded", Family: CommandFamilyLifecycle},
	{Name: "convergence_requested", Family: CommandFamilySampling},
	{Name: "convergence_finalization_started", Family: CommandFamilySampling, AllowedPhases: []Phase{PhaseSampling}},
	{Name: "tool_calls_proposed", Family: CommandFamilyTool, AllowedPhases: []Phase{PhaseSampling}},
	{Name: "tool_result_received", Family: CommandFamilyTool, AllowedPhases: []Phase{PhaseExecutingTools}},
	{Name: "abort_open_calls", Family: CommandFamilyTool},
	{Name: "approval_required", Family: CommandFamilyInteraction, AllowedPhases: []Phase{PhaseExecutingTools}},
	{Name: "approval_resolved", Family: CommandFamilyInteraction, AllowedPhases: []Phase{PhaseAwaitingApproval}},
	{Name: "approval_result_received", Family: CommandFamilyInteraction, AllowedPhases: []Phase{PhaseAwaitingApproval}},
	{Name: "input_required", Family: CommandFamilyInteraction, AllowedPhases: []Phase{PhaseExecutingTools}},
	{Name: "input_resolved", Family: CommandFamilyInteraction, AllowedPhases: []Phase{PhaseAwaitingInput}},
	{Name: "input_result_received", Family: CommandFamilyInteraction, AllowedPhases: []Phase{PhaseAwaitingInput}},
	{Name: "workspace_reconciled", Family: CommandFamilyVerification, AllowedPhases: []Phase{PhaseSampling}},
	{Name: "verification_started", Family: CommandFamilyVerification, AllowedPhases: []Phase{PhaseSampling}},
	{Name: "verification_finished", Family: CommandFamilyVerification, AllowedPhases: []Phase{PhaseVerifying}},
	{Name: "completion_evaluated", Family: CommandFamilyVerification},
	{Name: "completion_invalidated", Family: CommandFamilyVerification},
	{Name: "cancel_requested", Family: CommandFamilyInteraction},
	{Name: "recovery_requested", Family: CommandFamilyInteraction},
	{Name: "effect_started", Family: CommandFamilyEffect},
	{Name: "effect_requeued", Family: CommandFamilyEffect},
	{Name: "effect_result_received", Family: CommandFamilyEffect},
	{Name: "persistence_result_received", Family: CommandFamilyEffect, AllowedPhases: []Phase{PhaseCommitting}},
	{Name: "terminal_requested", Family: CommandFamilyTerminal},
	{Name: "journal_finalized", Family: CommandFamilyTerminal, AllowedPhases: []Phase{PhaseCommitting}},
	{Name: "journal_result_received", Family: CommandFamilyTerminal, AllowedPhases: []Phase{PhaseCommitting}},
	{Name: "finish_terminal", Family: CommandFamilyTerminal, AllowedPhases: []Phase{PhaseCommitting}},
}

func CommandContracts() []CommandContract {
	result := make([]CommandContract, len(commandContracts))
	for index, contract := range commandContracts {
		result[index] = contract
		result[index].AllowedPhases = append([]Phase(nil), contract.AllowedPhases...)
	}
	return result
}
