package agentcontext

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

const (
	CheckpointMarkerStart = "<qcode_turn_checkpoint>"
	CheckpointMarkerEnd   = "</qcode_turn_checkpoint>"

	CheckpointCompleted = "completed"
	CheckpointFailed    = "failed"
	CheckpointCanceled  = "canceled"
	CheckpointUnknown   = "unknown"

	TurnHistoryToolName = "turn_history"
	TurnHistorySource   = "runtime.turn_history"
)

// TurnCheckpoint is one write-once Dynamic block for a closed turn.
// Later samples never rewrite or merge an existing turn's text.
type TurnCheckpoint struct {
	Turn             uint64       `json:"turn"`
	Status           string       `json:"status"`
	Text             string       `json:"text"`
	HistoryHandle    string       `json:"history_handle,omitempty"`
	SourceMessageIDs []string     `json:"source_message_ids,omitempty"`
	Findings         TurnFindings `json:"findings,omitzero"`
}

// TurnFindings is the sourced index for a closed turn. It is durable and
// used by turn_history / continuity, but is not copied into the model-visible
// checkpoint text so older conversation lists cannot leak into later samples.
type TurnFindings struct {
	Conclusion  string   `json:"conclusion,omitempty"`
	Sites       []string `json:"sites,omitempty"`
	SourceTurns []uint64 `json:"source_turns,omitempty"`
}

// IsZero keeps absent findings absent when restoring digest-bound checkpoints.
// Empty and nil slices encode identically; source-only findings remain durable.
func (f TurnFindings) IsZero() bool {
	return f.Conclusion == "" && len(f.Sites) == 0 && len(f.SourceTurns) == 0
}

type CheckpointOpenItem struct {
	Title            string   `json:"title"`
	Kind             string   `json:"kind,omitempty"`
	Status           string   `json:"status,omitempty"`
	SourceMessageIDs []string `json:"source_message_ids,omitempty"`
}

type CheckpointHistoryRef struct {
	Tool string `json:"tool"`
	Turn uint64 `json:"turn"`
}

type CheckpointBody struct {
	Version   int                  `json:"v"`
	Turn      uint64               `json:"turn"`
	Status    string               `json:"status"`
	Goal      string               `json:"goal,omitempty"`
	Open      []CheckpointOpenItem `json:"open,omitempty"`
	Failure   string               `json:"failure,omitempty"`
	ReadPaths []string             `json:"read_paths,omitempty"`
	History   CheckpointHistoryRef `json:"history"`
}

type CheckpointRenderInput struct {
	Turn             uint64
	Status           string
	Goal             string
	Plan             Plan
	Items            []NarrativeItem
	Failure          string
	ReadPaths        []string
	HistoryHandle    string
	SourceMessageIDs []string
	Findings         TurnFindings
	Budget           int
}

func CloneTurnCheckpoints(checkpoints []TurnCheckpoint) []TurnCheckpoint {
	if len(checkpoints) == 0 {
		return nil
	}
	cloned := make([]TurnCheckpoint, len(checkpoints))
	for index, checkpoint := range checkpoints {
		cloned[index] = checkpoint
		cloned[index].SourceMessageIDs = append(
			[]string(nil),
			checkpoint.SourceMessageIDs...,
		)
		cloned[index].Findings = CloneTurnFindings(checkpoint.Findings)
	}
	return cloned
}

func (c TurnCheckpoint) Validate() error {
	if c.Turn == 0 {
		return errors.New("turn checkpoint turn is required")
	}
	switch c.Status {
	case CheckpointCompleted, CheckpointFailed, CheckpointCanceled, CheckpointUnknown:
	default:
		return errors.New("turn checkpoint status is invalid")
	}
	if strings.TrimSpace(c.Text) == "" {
		return errors.New("turn checkpoint text is required")
	}
	return nil
}

func ValidateTurnCheckpoints(checkpoints []TurnCheckpoint) error {
	seen := make(map[uint64]struct{}, len(checkpoints))
	for _, checkpoint := range checkpoints {
		if err := checkpoint.Validate(); err != nil {
			return err
		}
		if _, exists := seen[checkpoint.Turn]; exists {
			return fmt.Errorf("turn checkpoint %d is duplicated", checkpoint.Turn)
		}
		seen[checkpoint.Turn] = struct{}{}
	}
	return nil
}

func ResolveCheckpointBudget(checkpointMaxBytes, summaryMaxBytes, itemMaxBytes int) int {
	if checkpointMaxBytes > 0 {
		return checkpointMaxBytes
	}
	if summaryMaxBytes > 0 {
		return summaryMaxBytes
	}
	if itemMaxBytes > 0 {
		return itemMaxBytes
	}
	return DefaultNarrativeLimits().ItemMaxBytes
}

func MessagesForTurn(history []provider.Message, turn uint64) []provider.Message {
	if turn == 0 {
		return nil
	}
	var messages []provider.Message
	for _, message := range history {
		if message.Turn != turn || IsWorldStateMessage(message) {
			continue
		}
		messages = append(messages, message)
	}
	return messages
}

func RenderTurnTranscript(messages []provider.Message) string {
	var output strings.Builder
	for _, message := range messages {
		if IsWorldStateMessage(message) {
			continue
		}
		text := strings.TrimSpace(narrativeMessageText(message, nil))
		if text == "" {
			text = strings.TrimSpace(message.Text())
		}
		if text == "" {
			continue
		}
		fmt.Fprintf(&output, "[turn %d %s] %s\n", message.Turn, message.Role, text)
	}
	return output.String()
}

func RenderTurnCheckpoint(input CheckpointRenderInput) (TurnCheckpoint, error) {
	status := input.Status
	if status == "" {
		status = CheckpointCompleted
	}
	switch status {
	case CheckpointCompleted, CheckpointFailed, CheckpointCanceled, CheckpointUnknown:
	default:
		return TurnCheckpoint{}, errors.New("turn checkpoint status is invalid")
	}
	if input.Turn == 0 {
		return TurnCheckpoint{}, errors.New("turn checkpoint turn is required")
	}
	body := CheckpointBody{
		Version: 1, Turn: input.Turn, Status: status,
		Goal:    strings.TrimSpace(input.Goal),
		Failure: strings.TrimSpace(input.Failure),
		History: CheckpointHistoryRef{
			Tool: TurnHistoryToolName, Turn: input.Turn,
		},
	}
	if body.Goal == "" {
		body.Goal = strings.TrimSpace(input.Plan.Objective)
	}
	if body.Goal == "" {
		body.Goal = strings.TrimSpace(input.Plan.Title)
	}
	sources := append([]string(nil), input.SourceMessageIDs...)
	for _, item := range input.Items {
		if !IsOpenWorkNarrativeKind(item.Kind) ||
			strings.TrimSpace(item.Text) == "" ||
			len(item.SourceMessageIDs) == 0 {
			continue
		}
		body.Open = append(body.Open, CheckpointOpenItem{
			Title:            strings.TrimSpace(item.Text),
			Kind:             item.Kind,
			Status:           StepPending,
			SourceMessageIDs: append([]string(nil), item.SourceMessageIDs...),
		})
		sources = append(sources, item.SourceMessageIDs...)
	}
	if status == CheckpointCompleted && len(body.Open) == 0 {
		for _, step := range input.Plan.Steps {
			if step.Done() || strings.TrimSpace(step.Title) == "" {
				continue
			}
			body.Open = append(body.Open, CheckpointOpenItem{
				Title:  strings.TrimSpace(step.Title),
				Status: step.Status,
			})
		}
	}
	if status == CheckpointCanceled || status == CheckpointFailed {
		body.Open = nil
		if next := FormatCanceledCheckpointNext(input.Plan); next != "" {
			body.Open = []CheckpointOpenItem{{
				Title:  next,
				Status: StepPending,
			}}
		}
		body.ReadPaths = uniqueNonEmpty(input.ReadPaths)
	}
	budget := input.Budget
	if budget <= 0 {
		budget = ResolveCheckpointBudget(0, 0, 0)
	}
	text, err := encodeCheckpoint(body, budget, input.HistoryHandle)
	if err != nil {
		return TurnCheckpoint{}, err
	}
	return TurnCheckpoint{
		Turn:             input.Turn,
		Status:           status,
		Text:             text,
		HistoryHandle:    input.HistoryHandle,
		SourceMessageIDs: uniqueNonEmpty(sources),
		Findings:         CloneTurnFindings(input.Findings),
	}, nil
}

func CloneTurnFindings(findings TurnFindings) TurnFindings {
	return TurnFindings{
		Conclusion:  findings.Conclusion,
		Sites:       append([]string(nil), findings.Sites...),
		SourceTurns: append([]uint64(nil), findings.SourceTurns...),
	}
}

func (f TurnFindings) Empty() bool {
	return strings.TrimSpace(f.Conclusion) == "" && len(f.Sites) == 0
}

func RenderTurnFindings(turn uint64, findings TurnFindings) string {
	if findings.Empty() {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[turn %d findings]\n", turn)
	if conclusion := strings.TrimSpace(findings.Conclusion); conclusion != "" {
		fmt.Fprintf(&b, "conclusion: %s\n", conclusion)
	}
	if len(findings.Sites) > 0 {
		fmt.Fprintf(&b, "sites: %s\n", strings.Join(findings.Sites, ", "))
	}
	if turns := UniqueTurns(findings.SourceTurns); len(turns) > 0 {
		fmt.Fprintf(&b, "source_turns: %s\n", formatTurnList(turns))
	}
	return b.String()
}

func encodeCheckpoint(body CheckpointBody, budget int, handle string) (string, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	text := CheckpointMarkerStart + "\n" + string(encoded) + "\n" + CheckpointMarkerEnd + "\n"
	if budget <= 0 || len(text) <= budget {
		return text, nil
	}
	summary := CheckpointBody{
		Version: 1, Turn: body.Turn, Status: body.Status,
		Goal: TruncateUTF8(body.Goal, max(32, budget/4)),
		Failure: TruncateUTF8(
			body.Failure,
			max(0, budget/8),
		),
		History: body.History,
	}
	if handle != "" {
		summary.Open = []CheckpointOpenItem{{
			Title: "details: result_get " + handle,
		}}
	} else if len(body.Open) != 0 {
		summary.Open = []CheckpointOpenItem{{
			Title: TruncateUTF8(body.Open[0].Title, max(32, budget/4)),
		}}
	}
	if n := len(body.ReadPaths); n != 0 {
		summary.ReadPaths = []string{body.ReadPaths[0]}
		if n > 1 {
			summary.ReadPaths = append(summary.ReadPaths, fmt.Sprintf(
				"(%d more already-read paths omitted)", n-1,
			))
		}
	}
	encoded, err = json.Marshal(summary)
	if err != nil {
		return "", err
	}
	text = CheckpointMarkerStart + "\n" + string(encoded) + "\n" + CheckpointMarkerEnd + "\n"
	if len(text) <= budget {
		return text, nil
	}
	return TruncateUTF8(text, budget), nil
}

func uniqueNonEmpty(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func CheckpointMessages(checkpoints []TurnCheckpoint) []provider.Message {
	if len(checkpoints) == 0 {
		return nil
	}
	messages := make([]provider.Message, 0, len(checkpoints))
	for _, checkpoint := range checkpoints {
		message := provider.TextMessage(provider.RoleSystem, checkpoint.Text)
		message.Turn = checkpoint.Turn
		messages = append(messages, message)
	}
	return messages
}

func OmittedTurnIDs(history []provider.Message, tailTurns int) []uint64 {
	return UniqueMessageTurns(OmittedHistory(history, tailTurns))
}

func FormatOmittedTurnHint(turns []uint64) string {
	return FormatOmittedTurnHintPreferred(turns, PreferredOmittedTurn(turns, nil))
}

func FormatOmittedTurnHintPreferred(turns []uint64, preferred uint64) string {
	if len(turns) == 0 {
		return ""
	}
	if preferred == 0 {
		preferred = turns[len(turns)-1]
	}
	first, last := turns[0], turns[len(turns)-1]
	if first == last {
		return fmt.Sprintf(
			"Older turn %d is omitted from the visible raw tail. Call %s with turn=%d to read its findings index and tail. The first page ends with conclusion and sites. If truncated, page with result_get mode=tail or mode=query (for example query=sites). Do not search the repository for conversation-only lists.",
			first, TurnHistoryToolName, first,
		)
	}
	return fmt.Sprintf(
		"Older turns %d-%d are omitted from the visible raw tail. Call %s with a closed turn id in that range (preferred_turn=%d) to read that turn's findings index and tail. The first page ends with conclusion and sites. If truncated, page with result_get mode=tail or mode=query (for example query=sites). Do not search the repository for conversation-only lists.",
		first, last, TurnHistoryToolName, preferred,
	)
}

func PreferredOmittedTurn(turns []uint64, checkpoints []TurnCheckpoint) uint64 {
	if len(turns) == 0 {
		return 0
	}
	byTurn := make(map[uint64]TurnFindings, len(checkpoints))
	for _, checkpoint := range checkpoints {
		byTurn[checkpoint.Turn] = checkpoint.Findings
	}
	for index := len(turns) - 1; index >= 0; index-- {
		if !byTurn[turns[index]].Empty() {
			return turns[index]
		}
	}
	return turns[len(turns)-1]
}

func OmittedTurnRetrievalEntity(turns []uint64) (TruthEntity, bool) {
	return OmittedTurnRetrievalEntityPreferred(turns, PreferredOmittedTurn(turns, nil))
}

func OmittedTurnRetrievalEntityPreferred(
	turns []uint64, preferred uint64,
) (TruthEntity, bool) {
	hint := FormatOmittedTurnHintPreferred(turns, preferred)
	if hint == "" {
		return TruthEntity{}, false
	}
	entity := NewTruthEntity(EntityFact, "omitted_turns", hint, TurnHistorySource)
	entity.normalizeLifecycle()
	return entity, true
}

func SessionStateRetrievalHint(capsule TruthCapsule) string {
	for _, entity := range capsule.Entities {
		if entity.Kind == EntityFact && entity.Source == TurnHistorySource {
			return entity.Value
		}
	}
	return ""
}

func HistoryHasSessionStateHint(history []provider.Message, hint string) bool {
	if strings.TrimSpace(hint) == "" {
		return false
	}
	for _, message := range history {
		id, ok := WorldMessageID(message)
		if ok && id == "session_state" && strings.Contains(message.Text(), hint) {
			return true
		}
	}
	return false
}
