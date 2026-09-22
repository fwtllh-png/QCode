package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	sessionhistory "github.com/fwtllh-png/QCode/internal/persist/history"

	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type PlanExecutionPreparation struct {
	Artifact protocol.SessionPlanArtifact
	Prompt   string
}

type TurnRecoveryPreparation struct {
	Prompt         string
	DisplayPrompt  string
	Intent         protocol.TurnIntent
	IdempotencyKey string
	Recovery       protocol.TurnRecoveryContext
}

const turnRecoveryOutputLimit = 16 << 10
const TurnRecoveryEvidenceLimit = 12 << 10
const TurnRecoveryPromptPrefix = "Continue the exact source Turn identified below."
const recoveryCurrentRequestMarker = "Current user request for this recovery. This is " +
	"the active instruction and overrides conflicting requests " +
	"from earlier recovery Turns:\n"

type recoveryToolStart struct {
	Tool            string
	ArgumentsDigest string
	Path            string
}

type RecoveryToolEvidence struct {
	Tool            string                `json:"tool"`
	CallID          string                `json:"call_id"`
	ArgumentsDigest string                `json:"arguments_digest,omitempty"`
	OutputDigest    string                `json:"output_digest"`
	IsError         bool                  `json:"is_error"`
	Path            string                `json:"path,omitempty"`
	Changes         []protocol.FileChange `json:"changes,omitempty"`
	Reason          string                `json:"reason,omitempty"`
}

type RecoveryOutcome struct {
	Tool   string `json:"tool"`
	Status string `json:"status"`
	Path   string `json:"path,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type RecoveryWorkItemCapsule struct {
	KnownReads     []string `json:"known_reads,omitempty"`
	KnownEdits     []string `json:"known_edits,omitempty"`
	OpenSessions   []string `json:"open_sessions,omitempty"`
	RequiredAction string   `json:"required_action,omitempty"`
}

type recoveryReceiptEvidence struct {
	Outcome          protocol.TurnOutcome              `json:"outcome,omitempty"`
	ReadPaths        []string                          `json:"read_paths,omitempty"`
	Changes          []protocol.ReceiptChange          `json:"changes,omitempty"`
	Verification     protocol.ReceiptVerification      `json:"verification"`
	WorkspaceOutcome *protocol.ReceiptWorkspaceOutcome `json:"workspace_outcome,omitempty"`
}

type RecoveryEvidenceCapsule struct {
	Version      int                 `json:"version"`
	SourceTurnID protocol.TurnID     `json:"source_turn_id"`
	Intent       protocol.TurnIntent `json:"intent"`
	Terminal     string              `json:"terminal"`
	// PartialOutput carries the bounded model output a crash-interrupted
	// Turn confirmed but never terminalized, so the navigation capsule keeps
	// the interrupted conclusion instead of only the tool ledger.
	PartialOutput string                   `json:"partial_output,omitempty"`
	WorkItem      *RecoveryWorkItemCapsule `json:"work_item,omitempty"`
	Outcomes      []RecoveryOutcome        `json:"outcomes,omitempty"`
	Tools         []RecoveryToolEvidence   `json:"closed_tools,omitempty"`
	OmittedTools  int                      `json:"omitted_tools,omitempty"`
	Receipt       *recoveryReceiptEvidence `json:"receipt,omitempty"`
}

func (r *Service) PrepareTurnRecovery(
	ctx context.Context,
	request protocol.TurnRecoveryRequest,
) (TurnRecoveryPreparation, error) {
	if err := request.Validate(); err != nil {
		return TurnRecoveryPreparation{},
			runtimeProblem(protocol.CodeInvalidArgument, err.Error(), err)
	}
	current, err := r.SessionStatus(ctx, request.SessionID)
	if err != nil {
		return TurnRecoveryPreparation{}, err
	}
	if err := ensureSessionQuiescent(current, string(request.Action)); err != nil {
		return TurnRecoveryPreparation{}, err
	}
	if current.LatestTurnWithdrawn && current.LatestTurnID == request.SourceTurnID {
		return TurnRecoveryPreparation{}, runtimeProblem(protocol.CodeConflict, "Turn was withdrawn and cannot be recovered", nil)
	}
	if current.LatestTurnID != "" &&
		current.LatestTurnID != request.SourceTurnID {
		return TurnRecoveryPreparation{}, resourceProblem(
			protocol.CodeConflict,
			"recovery source is not the latest Turn in the Session",
			false,
			protocol.ProblemReasonStaleRecoverySource,
			string(request.SourceTurnID),
		)
	}
	var recoveredProfile *protocol.SessionProfile
	if r.SessionProfilesAvailable() {
		snapshot, err := r.RestoreSessionProfile(
			ctx,
			request.SessionID,
			current.ThreadID,
		)
		if err != nil {
			return TurnRecoveryPreparation{}, fmt.Errorf(
				"restore current session profile for Turn recovery: %w",
				err,
			)
		}
		recoveredProfile = &snapshot.Profile
	}
	events, err := r.ReplayArtifactTurn(ctx, request.SourceTurnID)
	if err != nil {
		return TurnRecoveryPreparation{}, err
	}
	var started *protocol.TurnStartedData
	var submittedPlan *protocol.PlanDeltaData
	toolStarts := make(map[string]recoveryToolStart)
	var closedTools []RecoveryToolEvidence
	var sourceReceipt *protocol.ExecutionReceiptData
	terminal := false
	terminalState := ""
	journalAdmissionFailure := false
	var startedOperationID protocol.OperationID
	var sourceThreadID protocol.ThreadID
	var partialOutput strings.Builder
	for _, event := range events {
		if event.TurnID != request.SourceTurnID {
			continue
		}
		if sourceThreadID == "" {
			sourceThreadID = event.ThreadID
			sourceSessionID, ownerErr := r.SessionForThread(ctx, sourceThreadID)
			if ownerErr != nil || sourceSessionID != request.SessionID {
				return TurnRecoveryPreparation{}, resourceProblem(
					protocol.CodeConflict,
					"source Turn belongs to another Session",
					false,
					protocol.ProblemReasonSessionBusy,
					string(request.SourceTurnID),
				)
			}
		} else if event.ThreadID != sourceThreadID {
			return TurnRecoveryPreparation{}, resourceProblem(
				protocol.CodeConflict,
				"source Turn has inconsistent Thread identity",
				false,
				protocol.ProblemReasonSessionBusy,
				string(request.SourceTurnID),
			)
		}
		switch data := event.Data.(type) {
		case *protocol.TurnStartedData:
			copy := *data
			started = &copy
			startedOperationID = event.OperationID
		case *protocol.OutputDeltaData:
			appendBoundedRecoveryOutput(&partialOutput, data.Text)
		case *protocol.ToolStartData:
			toolStarts[data.CallID] = recoveryToolStart{
				Tool:            data.Tool,
				ArgumentsDigest: RecoveryDigestJSON(data.Arguments),
				Path:            recoveryToolPath(data.Tool, data.Arguments),
			}
		case *protocol.ToolResultData:
			start, ok := toolStarts[data.CallID]
			if !ok || start.Tool != data.Tool {
				continue
			}
			closedTools = append(closedTools, RecoveryToolEvidence{
				Tool: data.Tool, CallID: data.CallID,
				ArgumentsDigest: start.ArgumentsDigest,
				OutputDigest:    RecoveryDigest([]byte(data.Output)),
				IsError:         data.IsError,
				Path:            recoveryToolResultPath(start.Path, data.Changes),
				Changes:         append([]protocol.FileChange(nil), data.Changes...),
				Reason:          recoveryOutcomeReason(data.Tool, data.IsError, data.Output),
			})
			delete(toolStarts, data.CallID)
		case *protocol.PlanDeltaData:
			if data.ArtifactID != "" && data.Purpose.Normalize() == protocol.PlanPurposeExecution {
				copy := *data
				submittedPlan = &copy
			}
		case *protocol.ExecutionReceiptData:
			copy := *data
			sourceReceipt = &copy
		case *protocol.TurnCompletedData:
			terminal = true
			terminalState = "completed"
		case *protocol.TurnFailedData:
			terminal = true
			terminalState = fmt.Sprintf("failed (%s): %s", data.Code, data.Message)
			journalAdmissionFailure = journalAdmissionFailure ||
				isRetainedDraftMessage(data.Message)
		case *protocol.TurnCanceledData:
			terminal = true
			terminalState = "canceled: " + protocol.NormalizeCancelReason(data.Reason)
		case *protocol.OperationRejectedData:
			if isRetainedDraftMessage(data.Message) ||
				(event.OperationID == startedOperationID &&
					protocol.FaultAllowsTurnRecovery(data.Fault)) {
				terminal = true
				terminalState = fmt.Sprintf(
					"interrupted before terminal commit (%s): %s",
					data.Code,
					data.Message,
				)
				journalAdmissionFailure = journalAdmissionFailure ||
					isRetainedDraftMessage(data.Message)
			}
		}
	}
	if !terminal || (started == nil && !journalAdmissionFailure) {
		return TurnRecoveryPreparation{}, resourceProblem(
			protocol.CodeConflict,
			"source Turn is unavailable or not terminal",
			false,
			protocol.ProblemReasonSessionBusy,
			string(request.SourceTurnID),
		)
	}
	if started == nil {
		started = synthesizeJournalAdmissionStart(request)
	}
	rawSourcePrompt := started.Prompt
	if rawSourcePrompt == "" {
		rawSourcePrompt = started.DisplayPrompt
	}
	if strings.TrimSpace(rawSourcePrompt) == "" {
		return TurnRecoveryPreparation{}, runtimeProblem(protocol.CodeConflict, "source Turn has no durable model-visible request", nil)
	}
	sourcePrompt := rawSourcePrompt
	sourceDisplayPrompt := strings.TrimSpace(started.DisplayPrompt)
	if sourceDisplayPrompt == "" {
		sourceDisplayPrompt = rawSourcePrompt
	}
	intent := protocol.NormalizeTurnIntent(started.Intent)
	if !intent.Valid() {
		return TurnRecoveryPreparation{}, runtimeProblem(protocol.CodeConflict, "source Turn has no valid durable intent", nil)
	}
	planID := started.PlanID
	planTransition := started.PlanTransition
	planProfileRevision := started.ProfileRevision
	if planID == "" && submittedPlan != nil && sourceReceipt != nil &&
		strings.TrimSpace(sourceReceipt.Plan) != "" &&
		recoveredProfile != nil {
		planID = submittedPlan.ArtifactID
		planTransition = protocol.PlanTransitionAutopilot
		planProfileRevision = submittedPlan.ProfileRevision
	}
	planStale := planID != "" && recoveredProfile != nil &&
		planProfileRevision != recoveredProfile.Revision
	if planStale {
		planID, planTransition, planProfileRevision = "", "", 0
	}
	prompt := sourcePrompt
	displayPrompt := sourceDisplayPrompt
	if request.Action == protocol.TurnRecoveryContinue {
		sourcePrompt = r.recoveryEffectiveRequest(
			ctx,
			sourceThreadID,
			rawSourcePrompt,
			started.DisplayPrompt,
		)
		sourceDisplayPrompt = sourcePrompt
		currentGoal := strings.TrimSpace(request.Prompt)
		if currentGoal == "" {
			displayPrompt = "Continue: " + sourceDisplayPrompt
			currentGoal = sourceDisplayPrompt
		} else {
			displayPrompt = currentGoal
		}
		prompt = currentGoal
		if capsule := RenderRecoveryEvidence(
			request.SourceTurnID,
			intent,
			terminalState,
			closedTools,
			sourceReceipt,
			partialOutput.String(),
		); capsule != "" {
			prompt += "\n\n<recovery_evidence>\n" + capsule +
				"\n</recovery_evidence>"
		}
		prompt += fmt.Sprintf(
			"\n<source_request turn=%q/>",
			request.SourceTurnID,
		)
	}
	if planStale {
		prompt += "\n\nThe prior structured Plan was invalidated by a " +
			"Session Profile change. Submit a fresh structured Plan before " +
			"performing consequential actions."
	}
	return TurnRecoveryPreparation{
		Prompt:         prompt,
		DisplayPrompt:  displayPrompt,
		Intent:         intent,
		IdempotencyKey: request.IdempotencyKey,
		Recovery: protocol.TurnRecoveryContext{
			Action: request.Action, SourceTurnID: request.SourceTurnID,
			PlanID: planID, PlanTransition: planTransition,
			ProfileRevision: planProfileRevision,
		},
	}, nil
}

func (r *Service) recoveryEffectiveRequest(
	ctx context.Context,
	threadID protocol.ThreadID,
	modelPrompt string,
	displayPrompt string,
) string {
	prompt := strings.TrimSpace(modelPrompt)
	display := strings.TrimSpace(displayPrompt)
	visited := make(map[protocol.TurnID]struct{})
	for strings.HasPrefix(prompt, TurnRecoveryPromptPrefix) {
		if current, ok := recoveryCurrentRequest(prompt); ok {
			return current
		}
		sourceTurnID, ok := recoverySourceTurnID(prompt)
		if !ok {
			break
		}
		if _, duplicate := visited[sourceTurnID]; duplicate {
			break
		}
		visited[sourceTurnID] = struct{}{}
		ancestors, err := r.ReplayArtifactTurn(ctx, sourceTurnID)
		if err != nil {
			break
		}
		var source *protocol.TurnStartedData
		for _, event := range ancestors {
			if event.ThreadID != threadID || event.TurnID != sourceTurnID {
				continue
			}
			if data, ok := event.Data.(*protocol.TurnStartedData); ok {
				copy := *data
				source = &copy
			}
		}
		if source == nil {
			break
		}
		prompt = strings.TrimSpace(source.Prompt)
		display = strings.TrimSpace(source.DisplayPrompt)
	}
	if display != "" {
		for strings.HasPrefix(display, "Continue: ") {
			display = strings.TrimSpace(strings.TrimPrefix(display, "Continue: "))
		}
		return display
	}
	return RecoverySourcePrompt(prompt)
}

func recoveryCurrentRequest(prompt string) (string, bool) {
	offset := strings.LastIndex(prompt, recoveryCurrentRequestMarker)
	if offset < 0 {
		return "", false
	}
	decoder := json.NewDecoder(strings.NewReader(
		prompt[offset+len(recoveryCurrentRequestMarker):],
	))
	var request string
	if err := decoder.Decode(&request); err != nil ||
		strings.TrimSpace(request) == "" {
		return "", false
	}
	return strings.TrimSpace(request), true
}

func recoverySourceTurnID(prompt string) (protocol.TurnID, bool) {
	const marker = "\nSource Turn ID: "
	if _, value, ok := strings.Cut(prompt, marker); ok {
		value, _, _ = strings.Cut(value, "\n")
		turnID := protocol.TurnID(strings.TrimSpace(value))
		if turnID != "" {
			return turnID, true
		}
	}
	const pointer = `<source_request turn="`
	if _, value, ok := strings.Cut(prompt, pointer); ok {
		value, _, _ = strings.Cut(value, `"`)
		turnID := protocol.TurnID(strings.TrimSpace(value))
		if turnID != "" {
			return turnID, true
		}
	}
	return "", false
}

func RecoverySourcePrompt(prompt string) string {
	value := strings.TrimSpace(prompt)
	extracted, ok := recoveryTaggedSection(value, "source_request")
	if !strings.HasPrefix(value, TurnRecoveryPromptPrefix) || !ok {
		return value
	}
	return RecoverySourcePrompt(extracted)
}
func RecoveryDisplayPrompt(modelPrompt string, displayPrompt string) string {
	modelValue := strings.TrimSpace(modelPrompt)
	if strings.HasPrefix(modelValue, TurnRecoveryPromptPrefix) {
		return RecoverySourcePrompt(modelValue)
	}
	value := strings.TrimSpace(displayPrompt)
	if value == "" {
		value = modelValue
	}
	value = RecoverySourcePrompt(value)
	for strings.HasPrefix(value, "Continue: ") {
		value = strings.TrimSpace(strings.TrimPrefix(value, "Continue: "))
	}
	return value
}
func recoveryTaggedSection(prompt string, tag string) (string, bool) {
	open, close := "<"+tag+">", "</"+tag+">"
	_, body, ok := strings.Cut(prompt, open)
	if !ok {
		return "", false
	}
	for end := 0; ; end += len(close) {
		offset := strings.Index(body[end:], close)
		if offset < 0 {
			return "", false
		}
		end += offset
		if section := body[:end]; strings.Count(section, open) ==
			strings.Count(section, close) {
			return section, true
		}
	}
}
func RenderRecoveryEvidence(
	sourceTurnID protocol.TurnID,
	intent protocol.TurnIntent,
	terminal string,
	tools []RecoveryToolEvidence,
	receipt *protocol.ExecutionReceiptData,
	partialOutput string,
) string {
	capsule := RecoveryEvidenceCapsule{
		Version:       3,
		SourceTurnID:  sourceTurnID,
		Intent:        intent,
		Terminal:      terminal,
		PartialOutput: partialOutput,
		WorkItem:      recoveryWorkItemCapsule(tools, receipt),
		Outcomes:      recoveryOutcomes(tools),
		Tools:         append([]RecoveryToolEvidence(nil), tools...),
	}
	if receipt != nil {
		capsule.Receipt = &recoveryReceiptEvidence{
			Outcome:      receipt.Outcome,
			ReadPaths:    append([]string(nil), receipt.ReadPaths...),
			Changes:      append([]protocol.ReceiptChange(nil), receipt.Changes...),
			Verification: receipt.Verification,
		}
		if receipt.WorkspaceOutcome != nil {
			copy := *receipt.WorkspaceOutcome
			capsule.Receipt.WorkspaceOutcome = &copy
		}
		if terminal != "completed" {
			capsule.Receipt.Outcome = ""
		}
	}
	for {
		data, err := json.Marshal(capsule)
		if err != nil {
			return ""
		}
		if len(data) <= TurnRecoveryEvidenceLimit {
			return string(data)
		}
		switch {
		case len(capsule.Tools) != 0:
			capsule.Tools = capsule.Tools[:len(capsule.Tools)-1]
			capsule.OmittedTools++
		case len(capsule.Outcomes) != 0:
			capsule.Outcomes = capsule.Outcomes[:len(capsule.Outcomes)-1]
		case capsule.Receipt != nil && len(capsule.Receipt.ReadPaths) != 0:
			capsule.Receipt.ReadPaths =
				capsule.Receipt.ReadPaths[:len(capsule.Receipt.ReadPaths)-1]
		case capsule.Receipt != nil && len(capsule.Receipt.Changes) != 0:
			capsule.Receipt.Changes =
				capsule.Receipt.Changes[:len(capsule.Receipt.Changes)-1]
		case capsule.PartialOutput != "":
			// The interrupted conclusion is the last evidence to drop: the
			// tool ledger above is recoverable from the event log, the
			// partial output is not.
			capsule.PartialOutput = ""
		default:
			return ""
		}
	}
}

func recoveryWorkItemCapsule(
	tools []RecoveryToolEvidence,
	receipt *protocol.ExecutionReceiptData,
) *RecoveryWorkItemCapsule {
	var reads, edits, sessions []string
	if receipt != nil {
		reads = append(reads, receipt.ReadPaths...)
		for _, change := range receipt.Changes {
			edits = append(edits, change.Path)
		}
	}
	for _, evidence := range tools {
		if evidence.Path != "" &&
			(evidence.Tool == "file_read" || evidence.Tool == "search_text" ||
				evidence.Tool == "search_definition") {
			reads = append(reads, evidence.Path)
		}
		for _, change := range evidence.Changes {
			edits = append(edits, change.Path)
		}
		if evidence.Tool == "exec_command" || evidence.Tool == "write_stdin" {
			if strings.Contains(strings.ToLower(evidence.Reason), "running") ||
				strings.Contains(evidence.Reason, "write_stdin") {
				if evidence.Path != "" {
					sessions = append(sessions, evidence.Path)
				}
			}
		}
	}
	reads = uniqueNonEmpty(reads)
	edits = uniqueNonEmpty(edits)
	sessions = uniqueNonEmpty(sessions)
	if len(reads) == 0 && len(edits) == 0 && len(sessions) == 0 {
		return nil
	}
	action := "turn_history"
	switch {
	case len(sessions) > 0:
		action = "write_stdin"
	case len(edits) > 0:
		action = "exec_command"
	case len(reads) > 0:
		action = "file_edit"
	}
	return &RecoveryWorkItemCapsule{
		KnownReads:     reads,
		KnownEdits:     edits,
		OpenSessions:   sessions,
		RequiredAction: action,
	}
}

func recoveryOutcomes(tools []RecoveryToolEvidence) []RecoveryOutcome {
	outcomes := make([]RecoveryOutcome, 0, len(tools))
	for _, evidence := range tools {
		status := "ok"
		if evidence.IsError {
			status = "error"
		}
		reason := evidence.Reason
		if strings.Contains(strings.ToLower(reason), "cancel") {
			status = "canceled"
		} else if strings.Contains(strings.ToLower(reason), "running") {
			status = "running"
		}
		outcomes = append(outcomes, RecoveryOutcome{
			Tool:   evidence.Tool,
			Status: status,
			Path:   evidence.Path,
			Reason: reason,
		})
	}
	return outcomes
}

func recoveryToolPath(tool string, arguments json.RawMessage) string {
	if len(arguments) == 0 {
		return ""
	}
	var input struct {
		Path      string `json:"path"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return ""
	}
	if path := strings.TrimSpace(input.Path); path != "" {
		return path
	}
	if tool == "write_stdin" || tool == "exec_command" {
		return strings.TrimSpace(input.SessionID)
	}
	return ""
}

func recoveryToolResultPath(startPath string, changes []protocol.FileChange) string {
	if startPath != "" {
		return startPath
	}
	if len(changes) != 0 {
		return strings.TrimSpace(changes[0].Path)
	}
	return ""
}

func recoveryOutcomeReason(tool string, isError bool, output string) string {
	line := strings.TrimSpace(output)
	if line == "" {
		if isError {
			return tool + " failed"
		}
		return ""
	}
	if cut := strings.IndexAny(line, "\n\r"); cut >= 0 {
		line = strings.TrimSpace(line[:cut])
	}
	const limit = 120
	if len(line) > limit {
		line = strings.TrimSpace(line[:limit])
	}
	return line
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func RecoveryWorkItemGoal(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return ""
	}
	if goal, rest, ok := strings.Cut(prompt, "\n\n<recovery_evidence>"); ok {
		goal = strings.TrimSpace(goal)
		if goal != "" && !strings.Contains(rest, "<recovery_evidence>") {
			return goal
		}
		if goal != "" {
			return goal
		}
	}
	if current, ok := recoveryCurrentRequest(prompt); ok {
		return current
	}
	return ""
}

func ParseRecoveryWorkItem(prompt string) (reads []string, edits []string) {
	section, ok := recoveryTaggedSection(prompt, "recovery_evidence")
	if !ok {
		return nil, nil
	}
	var capsule RecoveryEvidenceCapsule
	if err := json.Unmarshal([]byte(section), &capsule); err != nil {
		return nil, nil
	}
	if capsule.WorkItem != nil {
		return append([]string(nil), capsule.WorkItem.KnownReads...),
			append([]string(nil), capsule.WorkItem.KnownEdits...)
	}
	if capsule.Receipt != nil {
		reads = append(reads, capsule.Receipt.ReadPaths...)
		for _, change := range capsule.Receipt.Changes {
			edits = append(edits, change.Path)
		}
	}
	return uniqueNonEmpty(reads), uniqueNonEmpty(edits)
}
func RecoveryDigestJSON(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	if !json.Valid(data) {
		return RecoveryDigest(data)
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return RecoveryDigest(data)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return RecoveryDigest(data)
	}
	return RecoveryDigest(canonical)
}
func RecoveryDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func appendBoundedRecoveryOutput(builder *strings.Builder, text string) {
	if text == "" || builder.Len() >= turnRecoveryOutputLimit {
		return
	}
	remaining := turnRecoveryOutputLimit - builder.Len()
	if len(text) > remaining {
		text = text[:remaining]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	builder.WriteString(text)
}

func (r *Service) Checkpoints(
	ctx context.Context,
	sessionID string,
	limit int,
) (protocol.CheckpointList, error) {
	if r.ArtifactStore() == nil {
		return protocol.CheckpointList{}, runtimeProblem(protocol.CodeUnavailable, "Session Checkpoints are unavailable", nil)
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		return protocol.CheckpointList{}, runtimeProblem(protocol.CodeInvalidArgument, "Checkpoint limit exceeds 1000", nil)
	}
	current, err := r.SessionStatus(ctx, sessionID)
	if err != nil {
		return protocol.CheckpointList{}, err
	}
	values, err := r.ArtifactStore().ListCheckpoints(ctx, sessionID, limit)
	if err != nil {
		return protocol.CheckpointList{}, err
	}
	profile, err := r.SessionProfile(ctx, sessionID)
	if err != nil {
		return protocol.CheckpointList{}, err
	}
	quiescent := ensureSessionQuiescent(current, "restore") == nil
	for index := range values {
		compatible := values[index].ProfileRevision == profile.Profile.Revision
		values[index].CanRestore = compatible && quiescent
		values[index].CanFork = compatible && quiescent
	}
	result := protocol.CheckpointList{
		Version:     protocol.CheckpointProtocolVersion,
		SessionID:   sessionID,
		Checkpoints: values,
	}
	if err := result.Validate(); err != nil {
		return protocol.CheckpointList{}, err
	}
	return result, nil
}

func (r *Service) Checkpoint(
	ctx context.Context,
	sessionID, checkpointID string,
) (protocol.SessionCheckpoint, error) {
	if r.ArtifactStore() == nil {
		return protocol.SessionCheckpoint{}, runtimeProblem(protocol.CodeUnavailable, "Session Checkpoints are unavailable", nil)
	}
	current, err := r.SessionStatus(ctx, sessionID)
	if err != nil {
		return protocol.SessionCheckpoint{}, err
	}
	checkpoint, _, _, err := r.ArtifactStore().GetCheckpoint(
		ctx,
		checkpointID,
	)
	if err != nil {
		return protocol.SessionCheckpoint{}, err
	}
	if checkpoint.SessionID != sessionID {
		return protocol.SessionCheckpoint{}, runtimeProblem(protocol.CodeInvalidArgument, "Checkpoint does not belong to the Session", nil)
	}
	if err := r.requireRetainedSource(ctx, checkpoint.ThreadID, checkpoint.TurnID); err != nil {
		checkpoint.CanRestore, checkpoint.CanFork = false, false
		return checkpoint, nil
	}
	profile, err := r.SessionProfile(ctx, sessionID)
	if err != nil {
		return protocol.SessionCheckpoint{}, err
	}
	compatible := profile.Profile.Revision == checkpoint.ProfileRevision &&
		ensureSessionQuiescent(current, "restore") == nil
	checkpoint.CanRestore = compatible
	checkpoint.CanFork = compatible
	return checkpoint, nil
}

func (r *Service) RestoreCheckpoint(
	ctx context.Context,
	sessionID, checkpointID string,
) (protocol.CheckpointRestoreResult, error) {
	release, err := r.beginContextMutation()
	if err != nil {
		return protocol.CheckpointRestoreResult{}, err
	}
	defer release()
	current, checkpoint, history, contextSnapshot, err := r.checkpointState(
		ctx,
		sessionID,
		checkpointID,
		"restore",
	)
	if err != nil {
		return protocol.CheckpointRestoreResult{}, err
	}
	manager, ok := r.CheckpointRuntime().(CheckpointEngine)
	if !ok {
		return protocol.CheckpointRestoreResult{}, resourceProblem(
			protocol.CodeUnavailable,
			"Checkpoint restore is unsupported by this engine",
			false,
			protocol.ProblemReasonUnsupported,
			checkpointID,
		)
	}
	decoded, err := sessionhistory.DecodeCompactedHistory(history)
	if err != nil {
		return protocol.CheckpointRestoreResult{}, err
	}
	previous, err := manager.History(current.ThreadID)
	if err != nil {
		return protocol.CheckpointRestoreResult{}, err
	}
	var (
		reconciliation  agentcontext.ReconciliationReceipt
		previousContext *agentcontext.ContextSnapshot
	)
	if contextSnapshot != nil {
		contextManager, supported := manager.(ContextCheckpointEngine)
		if !supported {
			return protocol.CheckpointRestoreResult{}, resourceProblem(
				protocol.CodeUnavailable,
				"Context checkpoint restore is unsupported by this engine",
				false,
				protocol.ProblemReasonUnsupported,
				checkpointID,
			)
		}
		value, exportErr := contextManager.ContextSnapshot(current.ThreadID)
		if exportErr != nil {
			return protocol.CheckpointRestoreResult{}, exportErr
		}
		previousContext = &value
		reconciliation, err = contextManager.RestoreContext(
			current.ThreadID,
			*contextSnapshot,
		)
	} else {
		err = manager.RestoreCheckpoint(current.ThreadID, decoded)
	}
	if err != nil {
		return protocol.CheckpointRestoreResult{}, err
	}
	operationID := protocol.OperationID(stableArtifactID(
		"op",
		sessionID,
		checkpointID,
		"restore",
	))
	currentCommitID := ""
	var committedContext *agentcontext.ContextSnapshot
	if contextSnapshot != nil && r.Durable() {
		store, supported := r.ContextRebaseStore().(CurrentContextStore)
		if !supported {
			_, rollbackErr := manager.(ContextCheckpointEngine).RestoreContext(
				current.ThreadID,
				*previousContext,
			)
			return protocol.CheckpointRestoreResult{}, errors.Join(
				errors.New("durable current context store is unavailable"),
				rollbackErr,
			)
		}
		restored, exportErr := manager.(ContextCheckpointEngine).ContextSnapshot(
			current.ThreadID,
		)
		if exportErr != nil {
			_, rollbackErr := manager.(ContextCheckpointEngine).RestoreContext(
				current.ThreadID,
				*previousContext,
			)
			return protocol.CheckpointRestoreResult{}, errors.Join(
				exportErr,
				rollbackErr,
			)
		}
		currentCommitID = stableArtifactID(
			"context",
			string(operationID),
			restored.Digest,
		)
		commitErr := store.CommitCurrentContext(
			ctx,
			agentcontext.CurrentContextCommit{
				ID:       currentCommitID,
				ThreadID: current.ThreadID,
				TurnID:   checkpoint.TurnID,
				Snapshot: restored,
			},
		)
		if commitErr != nil {
			_, rollbackErr := manager.(ContextCheckpointEngine).RestoreContext(
				current.ThreadID,
				*previousContext,
			)
			return protocol.CheckpointRestoreResult{}, errors.Join(
				commitErr,
				rollbackErr,
			)
		}
		committedContext = &restored
	}
	itemID := protocol.ItemID(stableArtifactID(
		"item",
		sessionID,
		checkpointID,
		"restore",
	))
	var contextDigest string
	var contextRevision, stateEpoch uint64
	if committedContext != nil {
		contextDigest = committedContext.Digest
		contextRevision = committedContext.Revision
		stateEpoch = committedContext.Epoch
	}
	publishErr := r.PublishArtifactEvent(
		operationID,
		current.ThreadID,
		checkpoint.TurnID,
		itemID,
		&protocol.CheckpointRestoredData{
			CheckpointID:   checkpoint.ID,
			SourceThreadID: checkpoint.ThreadID,
			SourceTurnID:   checkpoint.TurnID,
			SourceCursor:   checkpoint.Cursor,
			ReplacementHistory: append(
				[]protocol.CompactedMessage(nil),
				history...,
			),
			SideEffectsReplayed: false,
			ExactContext:        contextSnapshot != nil,
			WorkspaceClaimsValid: contextSnapshot != nil &&
				reconciliation.Stale == 0,
			InvalidatedClaims: reconciliation.Invalidated,
			StaleClaims:       reconciliation.Stale,
			ContextCommitID:   currentCommitID,
			ContextDigest:     contextDigest,
			ContextRevision:   contextRevision,
			StateEpoch:        stateEpoch,
		},
	)
	if publishErr != nil {
		var rollbackErr error
		if previousContext != nil {
			contextManager := manager.(ContextCheckpointEngine)
			_, rollbackErr = contextManager.RestoreContext(
				current.ThreadID,
				*previousContext,
			)
			if rollbackErr == nil && currentCommitID != "" {
				rollback, exportErr := contextManager.ContextSnapshot(
					current.ThreadID,
				)
				if exportErr != nil {
					rollbackErr = exportErr
				} else {
					store := r.ContextRebaseStore().(CurrentContextStore)
					rollbackErr = store.CommitCurrentContext(
						ctx,
						agentcontext.CurrentContextCommit{
							ID: stableArtifactID(
								"context",
								string(operationID),
								"rollback",
								rollback.Digest,
							),
							ThreadID: current.ThreadID,
							TurnID:   checkpoint.TurnID,
							Snapshot: rollback,
						},
					)
				}
			}
		} else {
			rollbackErr = manager.RestoreCheckpoint(current.ThreadID, previous)
		}
		return protocol.CheckpointRestoreResult{}, errors.Join(
			publishErr,
			rollbackErr,
		)
	}
	return protocol.CheckpointRestoreResult{
		Version:             protocol.CheckpointProtocolVersion,
		Checkpoint:          checkpoint,
		ThreadID:            current.ThreadID,
		RestoredCursor:      checkpoint.Cursor,
		SideEffectsReplayed: false,
		ExactContext:        contextSnapshot != nil,
		WorkspaceClaimsValid: contextSnapshot != nil &&
			reconciliation.Stale == 0,
		InvalidatedClaims: reconciliation.Invalidated,
		StaleClaims:       reconciliation.Stale,
	}, nil
}

func (r *Service) ForkCheckpoint(
	ctx context.Context,
	sessionID, checkpointID, title string,
) (protocol.CheckpointForkResult, error) {
	release, err := r.beginContextMutation()
	if err != nil {
		return protocol.CheckpointForkResult{}, err
	}
	defer release()
	_, checkpoint, history, contextSnapshot, err := r.checkpointState(
		ctx,
		sessionID,
		checkpointID,
		"fork",
	)
	if err != nil {
		return protocol.CheckpointForkResult{}, err
	}
	manager, ok := r.CheckpointRuntime().(CheckpointEngine)
	if !ok {
		return protocol.CheckpointForkResult{}, resourceProblem(
			protocol.CodeUnavailable,
			"Checkpoint Fork is unsupported by this engine",
			false,
			protocol.ProblemReasonUnsupported,
			checkpointID,
		)
	}
	decoded, err := sessionhistory.DecodeCompactedHistory(history)
	if err != nil {
		return protocol.CheckpointForkResult{}, err
	}
	newThreadID, err := protocol.NewThreadID()
	if err != nil {
		return protocol.CheckpointForkResult{}, err
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Checkpoint Fork"
	}
	title = boundedArtifactText(title, 256)
	operationID := protocol.OperationID(stableArtifactID(
		"op",
		sessionID,
		checkpointID,
		string(newThreadID),
	))
	itemID := protocol.ItemID(stableArtifactID(
		"item",
		sessionID,
		checkpointID,
		string(newThreadID),
	))
	var reconciliation agentcontext.ReconciliationReceipt
	currentCommitID := ""
	var committedContext *agentcontext.ContextSnapshot
	if contextSnapshot != nil {
		contextManager, supported := manager.(ContextCheckpointEngine)
		if !supported {
			return protocol.CheckpointForkResult{}, resourceProblem(
				protocol.CodeUnavailable,
				"Context checkpoint Fork is unsupported by this engine",
				false,
				protocol.ProblemReasonUnsupported,
				checkpointID,
			)
		}
		reconciliation, err = contextManager.ForkContext(
			checkpoint.ThreadID,
			newThreadID,
			*contextSnapshot,
		)
		if err == nil && r.Durable() {
			store, durable := r.ContextRebaseStore().(CurrentContextStore)
			if !durable {
				manager.Release(newThreadID)
				return protocol.CheckpointForkResult{},
					errors.New("durable current context store is unavailable")
			}
			forked, exportErr := contextManager.ContextSnapshot(newThreadID)
			if exportErr != nil {
				manager.Release(newThreadID)
				return protocol.CheckpointForkResult{}, exportErr
			}
			currentCommitID = stableArtifactID(
				"context",
				string(operationID),
				forked.Digest,
			)
			err = store.CommitCurrentContext(
				ctx,
				agentcontext.CurrentContextCommit{
					ID:             currentCommitID,
					ThreadID:       newThreadID,
					TurnID:         checkpoint.TurnID,
					SessionID:      sessionID,
					ParentThreadID: checkpoint.ThreadID,
					Title:          title,
					SourceCursor:   checkpoint.Cursor,
					Snapshot:       forked,
				},
			)
			if err != nil {
				manager.Release(newThreadID)
				return protocol.CheckpointForkResult{}, err
			}
			committedContext = &forked
		}
	} else {
		err = manager.ForkCheckpoint(
			checkpoint.ThreadID,
			newThreadID,
			decoded,
		)
	}
	if err != nil {
		return protocol.CheckpointForkResult{}, err
	}
	var contextDigest string
	var contextRevision, stateEpoch uint64
	if committedContext != nil {
		contextDigest = committedContext.Digest
		contextRevision = committedContext.Revision
		stateEpoch = committedContext.Epoch
	}
	if err := r.PublishArtifactEvent(
		operationID,
		checkpoint.ThreadID,
		checkpoint.TurnID,
		itemID,
		&protocol.CheckpointForkedData{
			CheckpointID: checkpoint.ID,
			NewThreadID:  newThreadID,
			Title:        title,
			SourceCursor: checkpoint.Cursor,
			ReplacementHistory: append(
				[]protocol.CompactedMessage(nil),
				history...,
			),
			ExactContext: contextSnapshot != nil,
			WorkspaceClaimsValid: contextSnapshot != nil &&
				reconciliation.Stale == 0,
			InvalidatedClaims: reconciliation.Invalidated,
			StaleClaims:       reconciliation.Stale,
			ContextCommitID:   currentCommitID,
			ContextDigest:     contextDigest,
			ContextRevision:   contextRevision,
			StateEpoch:        stateEpoch,
		},
	); err != nil {
		if currentCommitID != "" {
			store := r.ContextRebaseStore().(CurrentContextStore)
			err = errors.Join(
				err,
				store.DeleteCurrentContext(
					ctx,
					newThreadID,
					currentCommitID,
					true,
				),
			)
		}
		manager.Release(newThreadID)
		return protocol.CheckpointForkResult{}, err
	}
	if _, err := r.ActivateThread(
		ctx,
		sessionID,
		newThreadID,
	); err != nil {
		return protocol.CheckpointForkResult{}, err
	}
	return protocol.CheckpointForkResult{
		Version:      protocol.CheckpointProtocolVersion,
		Checkpoint:   checkpoint,
		SessionID:    sessionID,
		ThreadID:     newThreadID,
		ParentID:     checkpoint.ThreadID,
		ExactContext: contextSnapshot != nil,
		WorkspaceClaimsValid: contextSnapshot != nil &&
			reconciliation.Stale == 0,
		InvalidatedClaims: reconciliation.Invalidated,
		StaleClaims:       reconciliation.Stale,
	}, nil
}

func (r *Service) SessionPlan(
	ctx context.Context,
	sessionID string,
) (protocol.SessionPlanSnapshot, error) {
	if r.ArtifactStore() == nil {
		return protocol.SessionPlanSnapshot{}, runtimeProblem(protocol.CodeUnavailable, "Session Plan Artifacts are unavailable", nil)
	}
	current, err := r.SessionStatus(ctx, sessionID)
	if err != nil {
		return protocol.SessionPlanSnapshot{}, err
	}
	artifact, found, err := r.ArtifactStore().LatestPlan(
		ctx,
		sessionID,
		current.ThreadID,
	)
	if err != nil {
		return protocol.SessionPlanSnapshot{}, err
	}
	if !found {
		return protocol.SessionPlanSnapshot{
			Version: protocol.CheckpointProtocolVersion,
		}, nil
	}
	if err := validateStructuredPlan(artifact.Body, true); err != nil {
		return protocol.SessionPlanSnapshot{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"Plan Artifact is not a structured Plan Document",
			err,
		)
	}
	profile, err := r.SessionProfile(ctx, sessionID)
	if err != nil {
		return protocol.SessionPlanSnapshot{}, err
	}
	compatible := planProfileCompatible(artifact, profile.Profile) &&
		r.ensurePlanExecutionReady(ctx, current, artifact) == nil
	artifact.CanImplement = artifact.CanImplement && compatible
	artifact.CanAutopilot = artifact.CanAutopilot && compatible
	return protocol.SessionPlanSnapshot{
		Version:  protocol.CheckpointProtocolVersion,
		Artifact: &artifact,
	}, nil
}

func (r *Service) PreparePlanExecution(
	ctx context.Context,
	sessionID, planID string,
	transition protocol.PlanTransition,
) (PlanExecutionPreparation, error) {
	current, err := r.SessionStatus(ctx, sessionID)
	if err != nil {
		return PlanExecutionPreparation{}, err
	}
	artifact, err := r.ArtifactStore().GetPlan(ctx, planID)
	if err != nil {
		return PlanExecutionPreparation{}, err
	}
	if artifact.Purpose.Normalize() != protocol.PlanPurposeExecution {
		return PlanExecutionPreparation{}, runtimeProblem(protocol.CodeConflict,
			"deliverable Plan must be submitted as an execution Plan before implementation", nil)
	}
	if err := validateStructuredPlan(artifact.Body, true); err != nil {
		return PlanExecutionPreparation{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"Plan Artifact is not a structured Plan Document",
			err,
		)
	}
	if artifact.SessionID != sessionID || artifact.ThreadID != current.ThreadID {
		return PlanExecutionPreparation{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"Plan Artifact does not belong to the active Session Thread",
			nil,
		)
	}
	if err := r.ensurePlanExecutionReady(ctx, current, artifact); err != nil {
		return PlanExecutionPreparation{}, err
	}
	profile, err := r.SessionProfile(ctx, sessionID)
	if err != nil {
		return PlanExecutionPreparation{}, err
	}
	if !planProfileCompatible(artifact, profile.Profile) {
		return PlanExecutionPreparation{}, retryableProblem(
			protocol.CodeConflict,
			"Plan Artifact Profile Revision is stale",
		)
	}
	switch transition {
	case protocol.PlanTransitionImplement:
		if !artifact.CanImplement {
			return PlanExecutionPreparation{}, runtimeProblem(
				protocol.CodeConflict,
				"Plan Artifact cannot start implementation",
				nil,
			)
		}
	case protocol.PlanTransitionAutopilot:
		if !artifact.CanAutopilot {
			return PlanExecutionPreparation{}, runtimeProblem(
				protocol.CodeConflict,
				"Plan Artifact cannot start Autopilot",
				nil,
			)
		}
	default:
		return PlanExecutionPreparation{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"Plan transition is invalid",
			nil,
		)
	}
	prompt := "Implement the approved structured Plan below. " +
		"Do not repeat completed external side effects; inspect current " +
		"workspace state before each consequential action.\n\n" +
		artifact.Body
	return PlanExecutionPreparation{
		Artifact: artifact,
		Prompt:   prompt,
	}, nil
}

func (r *Service) ensurePlanExecutionReady(
	ctx context.Context,
	current protocol.SessionSummary,
	artifact protocol.SessionPlanArtifact,
) error {
	if err := r.requireRetainedSource(ctx, artifact.ThreadID, artifact.TurnID); err != nil {
		return err
	}
	if readiness, ok := r.ArtifactRuntime.(interface {
		EnsurePlanExecutionReady(
			context.Context,
			string,
			protocol.ThreadID,
			protocol.TurnID,
		) error
	}); ok {
		return readiness.EnsurePlanExecutionReady(
			ctx,
			current.SessionID,
			artifact.ThreadID,
			artifact.TurnID,
		)
	}
	return ensureSessionQuiescent(current, "implement Plan")
}

func (r *Service) PreparePlanExecutionTo(
	ctx context.Context,
	sourceSessionID, targetSessionID, planID string,
	transition protocol.PlanTransition,
) (PlanExecutionPreparation, error) {
	artifact, err := r.ArtifactStore().GetPlan(ctx, planID)
	if err != nil {
		return PlanExecutionPreparation{}, err
	}
	if artifact.Purpose.Normalize() != protocol.PlanPurposeExecution {
		return PlanExecutionPreparation{}, runtimeProblem(protocol.CodeConflict,
			"deliverable Plan must be submitted as an execution Plan before implementation", nil)
	}
	if err := validateStructuredPlan(artifact.Body, true); err != nil {
		return PlanExecutionPreparation{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"Plan Artifact is not a structured Plan Document",
			err,
		)
	}
	if artifact.SessionID != sourceSessionID {
		return PlanExecutionPreparation{}, resourceProblem(
			protocol.CodeInvalidArgument,
			"Plan Artifact does not belong to the source Session",
			false,
			protocol.ProblemReasonWrongSession,
			planID,
		)
	}
	if err := r.requireRetainedSource(ctx, artifact.ThreadID, artifact.TurnID); err != nil {
		return PlanExecutionPreparation{}, err
	}
	sourceProfile, err := r.SessionProfile(ctx, sourceSessionID)
	if err != nil {
		return PlanExecutionPreparation{}, err
	}
	if !planProfileCompatible(artifact, sourceProfile.Profile) {
		return PlanExecutionPreparation{}, revisionProblem(
			"Plan Artifact Profile Revision is stale",
			planID,
			artifact.ProfileRevision,
			sourceProfile.Profile.Revision,
		)
	}
	target, err := r.SessionStatus(ctx, targetSessionID)
	if err != nil {
		return PlanExecutionPreparation{}, err
	}
	if err := ensureSessionQuiescent(target, "implement Plan"); err != nil {
		return PlanExecutionPreparation{}, err
	}
	if sourceSessionID == targetSessionID &&
		target.ParentThreadID != artifact.ThreadID {
		return PlanExecutionPreparation{}, resourceProblem(
			protocol.CodeInvalidArgument,
			"Plan Artifact does not belong to the parent Fork Thread",
			false,
			protocol.ProblemReasonWrongSession,
			planID,
		)
	}
	targetProfile, err := r.SessionProfile(ctx, targetSessionID)
	if err != nil {
		return PlanExecutionPreparation{}, err
	}
	if !samePlanTargetProfile(sourceProfile.Profile, targetProfile.Profile) {
		return PlanExecutionPreparation{}, resourceProblem(
			protocol.CodeConflict,
			"target Session Profile does not match the Plan source Profile",
			false,
			protocol.ProblemReasonWrongSession,
			targetSessionID,
		)
	}
	switch transition {
	case protocol.PlanTransitionImplement:
		if !artifact.CanImplement {
			return PlanExecutionPreparation{}, runtimeProblem(
				protocol.CodeConflict,
				"Plan Artifact cannot start implementation",
				nil,
			)
		}
	case protocol.PlanTransitionAutopilot:
		if !artifact.CanAutopilot {
			return PlanExecutionPreparation{}, runtimeProblem(
				protocol.CodeConflict,
				"Plan Artifact cannot start Autopilot",
				nil,
			)
		}
	default:
		return PlanExecutionPreparation{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"Plan transition is invalid",
			nil,
		)
	}
	return PlanExecutionPreparation{
		Artifact: artifact,
		Prompt: "Implement the approved structured Plan below. " +
			"Do not repeat completed external side effects; inspect current " +
			"workspace state before each consequential action.\n\n" +
			artifact.Body,
	}, nil
}

func samePlanTargetProfile(
	source, target protocol.SessionProfile,
) bool {
	return source.Mode == target.Mode &&
		source.PlanningPolicy == target.PlanningPolicy &&
		source.Provider == target.Provider &&
		source.Model == target.Model &&
		source.ReasoningEffort == target.ReasoningEffort &&
		slices.Equal(source.EnabledToolIDs, target.EnabledToolIDs) &&
		source.ApprovalPosture == target.ApprovalPosture &&
		source.ExecutionTarget == target.ExecutionTarget &&
		source.MaxSteps == target.MaxSteps
}

func planProfileCompatible(
	artifact protocol.SessionPlanArtifact,
	profile protocol.SessionProfile,
) bool {
	if artifact.ExecutionProfileDigest == "" {
		return profile.Revision == artifact.ProfileRevision
	}
	digest, err := PlanExecutionProfileDigest(profile)
	return err == nil && digest == artifact.ExecutionProfileDigest
}
func (r *Service) checkpointState(
	ctx context.Context,
	sessionID, checkpointID, action string,
) (
	protocol.SessionSummary,
	protocol.SessionCheckpoint,
	[]protocol.CompactedMessage,
	*agentcontext.ContextSnapshot,
	error,
) {
	if r.ArtifactStore() == nil {
		return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil,
			runtimeProblem(protocol.CodeUnavailable, "Session Checkpoints are unavailable", nil)
	}
	current, err := r.SessionStatus(ctx, sessionID)
	if err != nil {
		return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil, err
	}
	if err := ensureSessionQuiescent(current, action); err != nil {
		return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil, err
	}
	checkpoint, history, checkpointProfile, err :=
		r.ArtifactStore().GetCheckpoint(ctx, checkpointID)
	if err != nil {
		return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil, err
	}
	if checkpoint.SessionID != sessionID {
		return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil,
			resourceProblem(
				protocol.CodeInvalidArgument,
				"Checkpoint does not belong to the Session",
				false,
				protocol.ProblemReasonWrongSession,
				checkpointID,
			)
	}
	if err := r.requireRetainedSource(ctx, checkpoint.ThreadID, checkpoint.TurnID); err != nil {
		return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil, err
	}
	currentProfile, err := r.SessionProfile(ctx, sessionID)
	if err != nil {
		return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil, err
	}
	if currentProfile.Profile.Revision != checkpoint.ProfileRevision ||
		checkpointProfile.Revision != checkpoint.ProfileRevision {
		return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil,
			revisionProblem(
				"Checkpoint Profile Revision is stale",
				checkpointID,
				checkpoint.ProfileRevision,
				currentProfile.Profile.Revision,
			)
	}
	var contextSnapshot *agentcontext.ContextSnapshot
	if checkpoint.ContextDigest != "" {
		store, ok := r.ArtifactStore().(ContextSessionArtifactStore)
		if !ok {
			return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil,
				errors.New("context checkpoint store is unavailable")
		}
		contextCheckpoint, snapshot, storedProfile, contextErr :=
			store.GetContextCheckpoint(ctx, checkpointID)
		if contextErr != nil {
			return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil,
				contextErr
		}
		if contextCheckpoint.ID != checkpoint.ID ||
			storedProfile.Revision != checkpointProfile.Revision {
			return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil,
				errors.New("context checkpoint lookup is inconsistent")
		}
		contextSnapshot = &snapshot
	}
	return current, checkpoint, history, contextSnapshot, nil
}
func stableArtifactID(prefix string, values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return prefix + "_" + hex.EncodeToString(sum[:])
}
func boundedArtifactText(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
func (r *Service) DecoratePlanArtifact(
	ctx context.Context,
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
	data *protocol.PlanDeltaData,
) error {
	if r.ArtifactStore() == nil || !r.SessionPersistenceAvailable() ||
		data == nil || !data.Done ||
		strings.TrimSpace(data.Body) == "" {
		return nil
	}
	if err := validateStructuredPlan(data.Body, false); err != nil {
		return runtimeProblem(
			protocol.CodeInvalidArgument,
			"Plan output must come from submit_plan",
			err,
		)
	}
	purpose, err := protocol.PlanPurposeFromBody(data.Body)
	if err != nil {
		return err
	}
	data.Purpose = purpose
	sessionID, err := r.SessionForThread(ctx, threadID)
	if err != nil {
		return err
	}
	profile, err := r.StoredProfile(ctx, sessionID, r.DefaultProfile())
	if err != nil {
		return err
	}
	latest, found, err := r.ArtifactStore().LatestPlan(ctx, sessionID, threadID)
	if err != nil {
		return err
	}
	source := []byte(data.Body)
	var document map[string]any
	if json.Unmarshal(source, &document) == nil && document["steps"] != nil {
		revision := uint64(1)
		if found {
			revision = planDocumentRevision(json.RawMessage(latest.Body)) + 1
			document["supersedes_id"] = latest.ID
		}
		document["revision"] = revision
		source, err = json.Marshal(document)
		if err != nil {
			return err
		}
		data.Body = string(source)
	}
	digest := sha256.Sum256(source)
	sourceDigest := hex.EncodeToString(digest[:])
	data.ArtifactID = stableArtifactID(
		"plan",
		sessionID,
		string(threadID),
		string(turnID),
		sourceDigest,
	)
	data.ProfileRevision = profile.Revision
	data.Status = string(protocol.PlanArtifactReady)
	data.CanImplement = purpose == protocol.PlanPurposeExecution
	data.CanAutopilot = purpose == protocol.PlanPurposeExecution
	return nil
}
func (r *Service) PersistSessionArtifact(
	ctx context.Context,
	event protocol.Event,
) {
	if r.ArtifactStore() == nil || !r.SessionPersistenceAvailable() {
		return
	}
	switch data := event.Data.(type) {
	case *protocol.PlanDeltaData:
		if data.ArtifactID == "" {
			return
		}
		sessionID, err := r.SessionForThread(
			ctx,
			event.ThreadID,
		)
		if err != nil {
			r.LogArtifactError("resolve Plan Session", event, err)
			return
		}
		profile, err := r.StoredProfile(ctx, sessionID, r.DefaultProfile())
		if err != nil {
			r.LogArtifactError("resolve Plan Profile", event, err)
			return
		}
		executionProfileDigest, err := PlanExecutionProfileDigest(profile)
		if err != nil {
			r.LogArtifactError("digest Plan Profile", event, err)
			return
		}
		_, err = r.ArtifactStore().SavePlan(ctx, protocol.SessionPlanArtifact{
			Version:                protocol.CheckpointProtocolVersion,
			ID:                     data.ArtifactID,
			SessionID:              sessionID,
			ThreadID:               event.ThreadID,
			TurnID:                 event.TurnID,
			Cursor:                 event.Sequence,
			Status:                 protocol.PlanArtifactReady,
			Purpose:                data.Purpose,
			Body:                   data.Body,
			ProfileRevision:        data.ProfileRevision,
			ExecutionProfileDigest: executionProfileDigest,
			CanImplement:           data.CanImplement,
			CanAutopilot:           data.CanAutopilot,
			CreatedAt:              event.CreatedAt,
		})
		if err != nil {
			r.LogArtifactError("save Plan Artifact", event, err)
		}
	case *protocol.TurnCompletedData:
		r.persistTerminalCheckpoint(
			ctx,
			event,
			protocol.CheckpointCompleted,
			data.Text,
		)
	case *protocol.TurnCanceledData:
		if protocol.NormalizeCancelReason(data.Reason) !=
			protocol.CancelReasonUserInterrupted {
			return
		}
		r.persistTerminalCheckpoint(
			ctx,
			event,
			protocol.CheckpointInterrupted,
			"Interrupted by the user; safe paired history was retained",
		)
	}
}

func planDocumentRevision(document json.RawMessage) uint64 {
	var lineage struct {
		Revision uint64 `json:"revision"`
	}
	if json.Unmarshal(document, &lineage) != nil || lineage.Revision == 0 {
		return 1
	}
	return lineage.Revision
}
func (r *Service) PersistTerminalArtifactForTurn(
	ctx context.Context,
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
) {
	if r.ArtifactStore() == nil {
		return
	}
	events, err := r.ReplayArtifactTurn(ctx, turnID)
	if err != nil {
		r.LogArtifactError(
			"replay terminal event for Checkpoint",
			protocol.Event{ThreadID: threadID, TurnID: turnID},
			err,
		)
		return
	}
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.ThreadID == threadID && event.TurnID == turnID &&
			protocol.IsTerminalEvent(event.Kind) {
			r.PersistSessionArtifact(ctx, event)
			return
		}
	}
}
func (r *Service) persistTerminalCheckpoint(
	ctx context.Context,
	event protocol.Event,
	status protocol.CheckpointStatus,
	summary string,
) {
	manager, ok := r.CheckpointRuntime().(CheckpointEngine)
	if !ok {
		return
	}
	history, err := manager.History(event.ThreadID)
	if err != nil || len(history) == 0 {
		if err != nil {
			r.LogArtifactError("read Checkpoint history", event, err)
		}
		return
	}
	encoded, err := sessionhistory.EncodeCompactedHistory(history)
	if err != nil {
		r.LogArtifactError("encode Checkpoint history", event, err)
		return
	}
	sessionID, err := r.SessionForThread(ctx, event.ThreadID)
	if err != nil {
		r.LogArtifactError("resolve Checkpoint Session", event, err)
		return
	}
	profile, err := r.StoredProfile(ctx, sessionID, r.DefaultProfile())
	if err != nil {
		r.LogArtifactError("read Checkpoint Profile", event, err)
		return
	}
	changed, external, note, parentCheckpointID, receipt := r.checkpointEffects(
		ctx,
		event.ThreadID,
		event.TurnID,
	)
	summary = boundedArtifactText(strings.TrimSpace(summary), 2048)
	if summary == "" {
		summary = fmt.Sprintf("%s Turn %s", status, event.TurnID)
	}
	checkpoint := protocol.SessionCheckpoint{
		Version: protocol.CheckpointProtocolVersion,
		ID: stableArtifactID(
			"checkpoint",
			sessionID,
			string(event.ThreadID),
			string(event.TurnID),
			fmt.Sprint(event.Sequence),
		),
		SessionID:           sessionID,
		ThreadID:            event.ThreadID,
		TurnID:              event.TurnID,
		Cursor:              event.Sequence,
		Status:              status,
		Summary:             summary,
		ProfileRevision:     profile.Revision,
		ParentCheckpointID:  parentCheckpointID,
		ChangeReceipt:       receipt,
		ChangedFiles:        changed,
		ExternalSideEffects: external,
		SideEffectNote:      note,
		CanRestore:          true,
		CanFork:             true,
		CreatedAt:           event.CreatedAt,
	}
	var saved protocol.SessionCheckpoint
	if contextManager, ok := manager.(ContextCheckpointEngine); ok {
		contextSnapshot, snapshotErr := contextManager.ContextSnapshot(
			event.ThreadID,
		)
		if snapshotErr != nil {
			r.LogArtifactError("snapshot Checkpoint context", event, snapshotErr)
			return
		}
		store, supported := r.ArtifactStore().(ContextSessionArtifactStore)
		if !supported {
			r.LogArtifactError(
				"save Checkpoint context",
				event,
				errors.New("context checkpoint store is unavailable"),
			)
			return
		}
		checkpoint.StateEpoch = contextSnapshot.Epoch
		checkpoint.ContextDigest = contextSnapshot.Digest
		checkpoint.WorkspaceDigest = contextSnapshot.Workspace.SparseDigest
		saved, err = store.SaveContextCheckpoint(
			ctx,
			checkpoint,
			encoded,
			contextSnapshot,
			profile,
		)
	} else {
		saved, err = r.ArtifactStore().SaveCheckpoint(
			ctx,
			checkpoint,
			encoded,
			profile,
		)
	}
	if err != nil {
		r.LogArtifactError("save Session Checkpoint", event, err)
		return
	}
	if err := r.PublishArtifactEvent(
		event.OperationID,
		event.ThreadID,
		event.TurnID,
		protocol.ItemID(stableArtifactID(
			"item",
			saved.ID,
			"created",
		)),
		&protocol.CheckpointCreatedData{Checkpoint: saved},
	); err != nil {
		r.LogArtifactError("publish Session Checkpoint", event, err)
	}
}
func (r *Service) checkpointEffects(
	ctx context.Context,
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
) (int, bool, string, string, *protocol.ReceiptReference) {
	events, err := r.ReplayArtifactTurn(ctx, turnID)
	if err != nil {
		return 0, true, "Side-effect receipt could not be read", "", nil
	}
	forks, err := r.ReplayArtifactKind(ctx, protocol.EventCheckpointForked)
	if err != nil {
		return 0, true, "Side-effect receipt could not be read", "", nil
	}
	changed := make(map[string]struct{})
	external := false
	parentCheckpointID := ""
	var reference *protocol.ReceiptReference
	for _, event := range forks {
		if fork, ok := event.Data.(*protocol.CheckpointForkedData); ok &&
			fork.NewThreadID == threadID {
			parentCheckpointID = fork.CheckpointID
		}
	}
	for _, event := range events {
		if event.TurnID != turnID {
			continue
		}
		receipt, ok := event.Data.(*protocol.ExecutionReceiptData)
		if !ok || receipt == nil {
			continue
		}
		reference = &protocol.ReceiptReference{
			EventID: event.ID, TurnID: event.TurnID, Cursor: event.Sequence,
		}
		for _, change := range receipt.Changes {
			changed[change.Path] = struct{}{}
		}
		external = external || len(receipt.ToolsSucceeded) > 0
	}
	note := ""
	if external {
		note = "Completed Tool effects remain applied and are never replayed by Restore"
	}
	return len(changed), external, note, parentCheckpointID, reference
}
func (r *Service) LogArtifactError(
	action string,
	event protocol.Event,
	err error,
) {
	r.ReportArtifactError(action, event, err)
}

func isRetainedDraftMessage(message string) bool {
	return strings.Contains(message, workspacejournal.ErrRetainedDraft.Error())
}

func synthesizeJournalAdmissionStart(
	request protocol.TurnRecoveryRequest,
) *protocol.TurnStartedData {
	prompt := strings.TrimSpace(request.Prompt)
	if prompt == "" {
		prompt = "Settle the retained workspace journal draft, then continue the user's request."
	}
	return &protocol.TurnStartedData{
		Prompt: prompt, DisplayPrompt: prompt,
		Intent: protocol.TurnIntentAnswer,
	}
}
