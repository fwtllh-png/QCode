package turnkernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

// WorkItem is the Kernel-owned next-step fact for one Turn. It persists with
// Domain Facts / Snapshots. The public progress signature stays a path-set
// plus coverage facts. Anti-spin additionally fingerprints workspace content
// versions and the latest tool-result digest so identity changes cannot
// hide a cycle.
type WorkItem struct {
	GoalDigest     string                  `json:"goal_digest,omitempty"`
	KnownReads     map[string]WorkItemRead `json:"known_reads,omitempty"`
	KnownEdits     map[string]WorkItemEdit `json:"known_edits,omitempty"`
	Open           WorkItemOpen            `json:"open"`
	RequiredAction string                  `json:"required_action,omitempty"`
}

// WorkItemRead is the durable read record of one path: where the read
// window sits, which content version it saw, where the spilled result lives,
// and which call produced it. Window/ContentDigest legacy encodings stay
// authoritative for records persisted before the structured fields existed.
type WorkItemRead struct {
	Window        string `json:"window,omitempty"`
	ContentDigest string `json:"content_digest,omitempty"`
	Turn          uint64 `json:"turn,omitempty"`
	StartLine     int    `json:"start_line,omitempty"`
	EndLine       int    `json:"end_line,omitempty"`
	ResultHandle  string `json:"result_handle,omitempty"`
	CallID        string `json:"call_id,omitempty"`
}

type WorkItemEdit struct {
	MutationRevision uint64 `json:"mutation_revision"`
	ContentDigest    string `json:"content_digest,omitempty"`
}

type WorkItemOpen struct {
	UnverifiedPaths []string `json:"unverified_paths,omitempty"`
	Sessions        []string `json:"sessions,omitempty"`
	CoveredPaths    []string `json:"covered_paths,omitempty"`
}

// WorkItemObservation is the tool-result evidence the reducer applies to a
// Work Item. Paths should already be workspace-relative when the Engine can
// normalize them.
type WorkItemObservation struct {
	ReadPath      string   `json:"read_path,omitempty"`
	ReadWindow    string   `json:"read_window,omitempty"`
	ReadStart     int      `json:"read_start,omitempty"`
	ReadEnd       int      `json:"read_end,omitempty"`
	ResultHandle  string   `json:"result_handle,omitempty"`
	CallID        string   `json:"call_id,omitempty"`
	ContentDigest string   `json:"content_digest,omitempty"`
	OpenSession   string   `json:"open_session,omitempty"`
	CloseSession  string   `json:"close_session,omitempty"`
	CoveredPaths  []string `json:"covered_paths,omitempty"`
	ResultDigest  string   `json:"result_digest,omitempty"`
}

func GoalDigest(goal string) string {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(goal))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (w WorkItem) HasKnown() bool {
	return len(w.KnownReads) > 0 || len(w.KnownEdits) > 0
}

func (w WorkItem) RequiredActionOr(fallback string) string {
	if action := strings.TrimSpace(w.RequiredAction); action != "" {
		return action
	}
	return fallback
}

func (w WorkItem) HasKnownOrOpen() bool {
	return w.HasKnown() ||
		len(w.Open.UnverifiedPaths) > 0 ||
		len(w.Open.Sessions) > 0 ||
		len(w.Open.CoveredPaths) > 0
}

func (w WorkItem) KnownRead(path string) (WorkItemRead, bool) {
	path = strings.TrimSpace(path)
	if path == "" || w.KnownReads == nil {
		return WorkItemRead{}, false
	}
	read, ok := w.KnownReads[path]
	return read, ok
}

func DeriveRequiredAction(state State) string {
	item := state.WorkItem
	switch {
	case len(item.Open.Sessions) > 0:
		return "write_stdin"
	case len(item.KnownEdits) > 0 &&
		state.Verification.Status != VerificationPassed &&
		state.Verification.Action != VerificationActionReported &&
		state.Verification.Action != VerificationActionReverted:
		return "exec_command"
	case len(item.KnownReads) > 0 && len(item.KnownEdits) == 0:
		return "file_edit"
	case state.Completion != nil && state.Completion.Accepted:
		return "turn_complete"
	case len(item.KnownReads) > 0:
		return "turn_history"
	default:
		return ""
	}
}

func cloneWorkItem(item WorkItem) WorkItem {
	cloned := item
	if item.KnownReads != nil {
		cloned.KnownReads = maps.Clone(item.KnownReads)
	}
	if item.KnownEdits != nil {
		cloned.KnownEdits = maps.Clone(item.KnownEdits)
	}
	cloned.Open.UnverifiedPaths = append([]string(nil), item.Open.UnverifiedPaths...)
	cloned.Open.Sessions = append([]string(nil), item.Open.Sessions...)
	cloned.Open.CoveredPaths = append([]string(nil), item.Open.CoveredPaths...)
	return cloned
}

func applyBindWorkItem(
	transition *Transition,
	current State,
	command BindWorkItem,
) error {
	if err := requirePhase(
		current,
		command,
		PhaseCreated,
		PhasePreparing,
		PhaseSampling,
	); err != nil {
		return err
	}
	if current.ActiveSampleID != "" {
		return illegal(current, command, "model sample is active")
	}
	item := cloneWorkItem(current.WorkItem)
	if digest := strings.TrimSpace(command.GoalDigest); digest != "" {
		item.GoalDigest = digest
	} else if goal := strings.TrimSpace(command.Goal); goal != "" {
		item.GoalDigest = GoalDigest(goal)
	}
	if len(command.KnownReads) != 0 {
		if item.KnownReads == nil {
			item.KnownReads = make(map[string]WorkItemRead, len(command.KnownReads))
		}
		for path, read := range command.KnownReads {
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			if _, exists := item.KnownReads[path]; exists {
				continue
			}
			item.KnownReads[path] = read
		}
	}
	if len(command.KnownEdits) != 0 {
		if item.KnownEdits == nil {
			item.KnownEdits = make(map[string]WorkItemEdit, len(command.KnownEdits))
		}
		for path, edit := range command.KnownEdits {
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			if _, exists := item.KnownEdits[path]; exists {
				continue
			}
			item.KnownEdits[path] = edit
		}
	}
	item.Open.UnverifiedPaths = mergeUniqueSorted(
		item.Open.UnverifiedPaths,
		command.Open.UnverifiedPaths...,
	)
	item.Open.Sessions = mergeUniqueSorted(
		item.Open.Sessions,
		command.Open.Sessions...,
	)
	item.Open.CoveredPaths = mergeUniqueSorted(
		item.Open.CoveredPaths,
		command.Open.CoveredPaths...,
	)
	item.RequiredAction = DeriveRequiredAction(withWorkItem(transition.State, item))
	transition.State.WorkItem = item
	return nil
}

func applyWorkItemObservation(
	state *State,
	call ToolCallState,
	changes []ObservedChange,
	observation WorkItemObservation,
	isError bool,
) {
	if isError {
		state.WorkItem.RequiredAction = DeriveRequiredAction(*state)
		return
	}
	item := cloneWorkItem(state.WorkItem)
	if observation.ReadPath == "" && call.Name == "file_read" {
		path, start, ok := ParseFileReadWindow(call.Arguments)
		if ok {
			observation.ReadPath = path
			if observation.ReadWindow == "" && start > 0 {
				observation.ReadWindow = strconv.Itoa(start)
			}
		}
	}
	if path := strings.TrimSpace(observation.ReadPath); path != "" {
		if item.KnownReads == nil {
			item.KnownReads = make(map[string]WorkItemRead)
		}
		previous, exists := item.KnownReads[path]
		read := previous
		if observation.ReadWindow != "" {
			read.Window = observation.ReadWindow
		} else if !exists {
			read.Window = "full"
		}
		if observation.ContentDigest != "" {
			read.ContentDigest = observation.ContentDigest
		}
		read.StartLine = observation.ReadStart
		read.EndLine = observation.ReadEnd
		if handle := strings.TrimSpace(observation.ResultHandle); handle != "" {
			read.ResultHandle = handle
		}
		if callID := strings.TrimSpace(observation.CallID); callID != "" {
			read.CallID = callID
		}
		item.KnownReads[path] = read
	}
	for _, change := range changes {
		path := strings.TrimSpace(change.Path)
		if path == "" {
			continue
		}
		if item.KnownEdits == nil {
			item.KnownEdits = make(map[string]WorkItemEdit)
		}
		edit := item.KnownEdits[path]
		edit.MutationRevision = state.MutationRevision
		if digest := strings.TrimSpace(change.ContentDigest); digest != "" {
			edit.ContentDigest = digest
		}
		item.KnownEdits[path] = edit
		item.Open.UnverifiedPaths = mergeUniqueSorted(
			item.Open.UnverifiedPaths,
			path,
		)
	}
	if session := strings.TrimSpace(observation.OpenSession); session != "" {
		item.Open.Sessions = mergeUniqueSorted(item.Open.Sessions, session)
	}
	if session := strings.TrimSpace(observation.CloseSession); session != "" {
		item.Open.Sessions = removeSorted(item.Open.Sessions, session)
	}
	if len(observation.CoveredPaths) != 0 {
		item.Open.CoveredPaths = mergeUniqueSorted(
			item.Open.CoveredPaths,
			observation.CoveredPaths...,
		)
		item.Open.UnverifiedPaths = subtractSorted(
			item.Open.UnverifiedPaths,
			observation.CoveredPaths...,
		)
	}
	item.RequiredAction = DeriveRequiredAction(withWorkItem(*state, item))
	state.WorkItem = item
}

func ObserveWorkItemResult(
	call ToolCallState,
	result tool.Result,
) WorkItemObservation {
	var observation WorkItemObservation
	observation.ResultDigest = ResultObservationDigest(result)
	if result.IsError {
		return observation
	}
	if path, start, ok := ParseFileReadWindow(call.Arguments); ok &&
		(call.Name == "file_read" || ObservedFileRead(result) != "") {
		observation.ReadPath = path
		if start > 0 {
			observation.ReadWindow = strconv.Itoa(start)
		}
		observation.ReadStart = start
	}
	if result.Outcome != nil && result.Outcome.Facts != nil {
		observation.ResultHandle = strings.TrimSpace(
			result.Outcome.Facts.ResultHandle,
		)
		if read := result.Outcome.Facts.WorkspaceRead; read != nil {
			if observation.ReadPath == "" {
				observation.ReadPath = strings.TrimSpace(read.Path)
			}
			observation.ContentDigest = strings.TrimSpace(read.Digest)
		}
		if session := result.Outcome.Facts.ProcessSession; session != nil &&
			strings.TrimSpace(session.SessionID) != "" {
			if session.Running {
				observation.OpenSession = strings.TrimSpace(session.SessionID)
			} else {
				observation.CloseSession = strings.TrimSpace(session.SessionID)
			}
		}
	}
	if call.Name == "file_read" {
		// The file tool reports how much of the window the read returned; a
		// window that stopped short of the end of file is bounded, anything
		// else reaches EOF.
		returned, _ := result.Metadata["returned_lines"].(int)
		if more, _ := result.Metadata["has_more"].(bool); more && returned > 0 {
			observation.ReadEnd = observation.ReadStart + returned
		} else {
			observation.ReadEnd = 0
		}
		if observation.ReadPath == "" {
			path, start, ok := ParseFileReadWindow(call.Arguments)
			if ok {
				observation.ReadPath = path
				observation.ReadStart = start
				if start > 0 {
					observation.ReadWindow = strconv.Itoa(start)
				}
			}
		}
	}
	observation.CallID = call.ID
	var covered struct {
		CoveredPaths []string `json:"covered_paths"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &covered); err == nil {
		observation.CoveredPaths = append(
			[]string(nil),
			covered.CoveredPaths...,
		)
	}
	return observation
}

func ParseFileReadWindow(raw string) (string, int, bool) {
	var input struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
	}
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		return "", 0, false
	}
	path := strings.TrimSpace(input.Path)
	return path, input.StartLine, path != ""
}

// EmptySampleIdentity names a Sample that proposed no tool calls. It is a
// real identity so a later ObserveProgress does not inherit the previous
// tool batch through PendingIdentity.
const EmptySampleIdentity = "none"

// FormatToolCallsIdentity names one Sample's tool batch. The identity is one
// coordinate of the Turn-scoped work-state key; changing it does not itself
// renew the no-progress clocks.
func FormatToolCallsIdentity(calls []ToolCallState) string {
	if len(calls) == 0 {
		return EmptySampleIdentity
	}
	parts := make([]string, 0, len(calls))
	for _, call := range calls {
		name := strings.TrimSpace(call.Name)
		if name == "" {
			continue
		}
		parts = append(parts, name+"\n"+canonicalizeToolArguments(call.Arguments))
	}
	if len(parts) == 0 {
		return EmptySampleIdentity
	}
	slices.Sort(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1e")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func canonicalizeToolArguments(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return raw
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return string(encoded)
}

func FormatWorkItemSignature(
	state State,
	completedPlanSteps int,
	openImplement bool,
) string {
	completionAccepted := false
	if state.Completion != nil {
		completionAccepted = state.Completion.Accepted
	}
	reads := ""
	if IsResearchIntent(state.Intent) && !openImplement {
		reads = joinSortedKeys(state.WorkItem.KnownReads)
	}
	return fmt.Sprintf(
		"goal=%s;reads=%s;edits=%s;verify=%s/%s;coverage=%s;"+
			"plan_done=%d;completion=%t;sessions=%s",
		state.WorkItem.GoalDigest,
		reads,
		joinSortedKeys(state.WorkItem.KnownEdits),
		state.Verification.Status,
		state.Verification.Action,
		strings.Join(append([]string(nil), state.WorkItem.Open.CoveredPaths...), ","),
		completedPlanSteps,
		completionAccepted,
		strings.Join(append([]string(nil), state.WorkItem.Open.Sessions...), ","),
	)
}

func joinSortedKeys[T any](values map[string]T) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}

func mergeUniqueSorted(existing []string, extras ...string) []string {
	seen := make(map[string]struct{}, len(existing)+len(extras))
	merged := make([]string, 0, len(existing)+len(extras))
	for _, value := range append(append([]string{}, existing...), extras...) {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		merged = append(merged, value)
	}
	slices.Sort(merged)
	return merged
}

func removeSorted(values []string, drop string) []string {
	return subtractSorted(values, drop)
}

func subtractSorted(values []string, drop ...string) []string {
	if len(values) == 0 || len(drop) == 0 {
		return append([]string(nil), values...)
	}
	remove := make(map[string]struct{}, len(drop))
	for _, value := range drop {
		value = strings.TrimSpace(value)
		if value != "" {
			remove[value] = struct{}{}
		}
	}
	kept := make([]string, 0, len(values))
	for _, value := range values {
		if _, drop := remove[value]; drop {
			continue
		}
		kept = append(kept, value)
	}
	return kept
}

// ReadWindowIsFull reports whether a read record covers the entire file.
func ReadWindowIsFull(read WorkItemRead) bool {
	if strings.TrimSpace(read.Window) == "full" {
		return true
	}
	return read.StartLine == 0 && strings.TrimSpace(read.Window) == ""
}

// ReadWindowCovers reports whether a recorded window covers a read starting
// at startLine. EndLine 0 means the window reaches the end of file; a
// full-file request is only covered by a full-file record.
func ReadWindowCovers(read WorkItemRead, startLine int) bool {
	if ReadWindowIsFull(read) {
		return true
	}
	start := read.StartLine
	if start == 0 {
		parsed, err := strconv.Atoi(strings.TrimSpace(read.Window))
		if err != nil || parsed <= 0 {
			return false
		}
		start = parsed
	}
	if startLine <= 0 || startLine < start {
		return false
	}
	return read.EndLine == 0 || startLine < read.EndLine
}

func withWorkItem(state State, item WorkItem) State {
	state.WorkItem = item
	return state
}
