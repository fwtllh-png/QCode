package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const turnRecoveryOutputLimit = 16 << 10

const TurnRecoveryEvidenceLimit = 12 << 10

const TurnRecoveryPromptPrefix = "Continue the exact source Turn identified below."

const recoveryCurrentRequestMarker = "Current user request for this recovery. This is " +
	"the active instruction and overrides conflicting requests " +
	"from earlier recovery Turns:\n"

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
