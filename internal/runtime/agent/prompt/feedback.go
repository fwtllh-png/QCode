package prompt

import (
	"encoding/json"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func IncompleteOutputFeedback(
	reason provider.StopReason,
	fragments []provider.ToolCallFragment,
	turn uint64,
) provider.Message {
	instruction := `Continue exactly from the captured response. Do not repeat
completed content. Finish the pending user-facing answer.`
	if len(fragments) != 0 {
		encoded, _ := json.Marshal(fragments)
		instruction = fmt.Sprintf(
			`The following tool call fragments were retained but were not
executed because the provider response did not close them:
%s
Continue from these exact fragments. Emit only complete, independently valid
tool calls. Split the operation into smaller calls when it is oversized. Do
not assume that any retained fragment already ran.`,
			encoded,
		)
	}
	return feedback(turn, fmt.Sprintf(
		`[continue_after_incomplete stop_reason=%s]
The provider stopped the previous response before completion. %s`,
		reason,
		instruction,
	))
}

func ConvergenceFeedback(
	turn uint64,
	cause string,
	used uint32,
	limit uint32,
	repairKind string,
	hasProvisionalOutput bool,
) provider.Message {
	return feedback(turn, fmt.Sprintf(
		"[convergence_finalization]\n"+
			"cause=%s\nused=%d\nlimit=%d\nrepair_kind=%s\n"+
			"captured_output=%t\nrequired_action=choose_structured_turn_state\n"+
			"This is the single reserved finalization sample outside the normal "+
			"work budget. Do not continue exploration, implementation, or a long "+
			"user-facing body. Call turn_complete now. If all requested work is "+
			"complete and captured_output=true, use status=complete, "+
			"output_mode=preserve_provisional, a concise closing summary, and "+
			"pending_actions=[]. If the captured output is unavailable, use "+
			"output_mode=exact with the complete concise answer in summary. If any "+
			"work remains, use status=incomplete with a concrete progress summary "+
			"and pending_actions; Runtime will record a resumable blocked outcome. "+
			"Use request_user_input only when completion genuinely depends on the user.",
		cause,
		used,
		limit,
		repairKind,
		hasProvisionalOutput,
	))
}

func WorkspaceChangeRequiredFeedback(turn uint64) provider.Message {
	return feedback(turn,
		"[completion_check]\n"+
			"required_action=perform_workspace_mutation\n"+
			"observed_changes=0\n"+
			"retry_original=false\n"+
			"The workspace_change contract is not complete. Use a guarded mutation tool, "+
			"then verify the observed changed paths before answering.")
}

func CompletionDeclarationFeedback(turn uint64) provider.Message {
	return feedback(turn,
		"[completion_declaration_required]\n"+
			"required_action=choose_structured_turn_state\n"+
			"retry_original=false\n"+
			"Provider message_stop ended only the previous model sample; it did not "+
			"complete this Turn. If request_user_input is available and progress requires "+
			"a user answer, call it now and wait in this same Turn. Otherwise report the "+
			"actual work state through turn_complete. Use status=complete only when every "+
			"requested action is finished, put the exact user-facing final response in "+
			"summary, and set pending_actions=[]. Your previous narration was preserved: "+
			"when it already states the final answer, call turn_complete with "+
			"output_mode=preserve_provisional and a one-line closing summary; the runtime "+
			"appends it to the preserved body instead of rewriting the answer. If any "+
			"work remains, use status=incomplete and list each concrete pending action; "+
			"the runtime will continue this same Turn. The runtime binds any changed "+
			"paths and accepted verification evidence automatically. Do not move "+
			"requested work to a future turn.")
}

func CompletionFeedback(turn uint64) provider.Message {
	return feedback(turn, `[completion_required]
Your previous model sample did not produce a user-facing answer. Continue with
the next required tool, call request_user_input if available and genuinely
blocked on the user, or stop calling tools and write the final answer as
ordinary assistant text. turn_complete is optional.`)
}

func ToolFailureCompletionFeedback(turn uint64) provider.Message {
	return feedback(turn, `[tool_failure_resolution_required]
The latest tool batch contained an explicit failure. Do not stop after
describing a future retry. Follow required_action and retry_original from the
failed Tool Result. Never repeat the same call when retry_original=false.
Otherwise call the required tool now to resolve the failure, or provide a
concise final answer that clearly reports the unresolved failure and its impact.`)
}

func NoProgressFeedback(
	turn uint64,
	samples uint32,
	stage string,
	stallKind string,
) provider.Message {
	reason := "The last model samples repeated the same tool-call identity. " +
		"Do not resubmit that exact call. Change the arguments, pick a " +
		"different tool, or call turn_complete."
	if stallKind == "cycle" {
		reason = "These steps already produced the same workspace content " +
			"and tool-result digest. Do not cycle through previously seen " +
			"calls. Advance the workspace, read a new window, or call " +
			"turn_complete."
	}
	return feedback(turn, fmt.Sprintf(
		"[no_progress]\n"+
			"steps_without_structured_progress=%d\n"+
			"stage=%s\n"+
			"stall_kind=%s\n"+
			"required_action=converge\n"+
			"%s Distinct edits or checks that change file content or "+
			"result digests are still progress. If the work cannot "+
			"continue, call turn_complete with status=incomplete and concrete "+
			"pending_actions.",
		samples,
		stage,
		stallKind,
		reason,
	))
}

func feedback(turn uint64, text string) provider.Message {
	message := provider.TextMessage(provider.RoleUser, text)
	message.Turn = turn
	return message
}
