package agentcontext

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// ConversationSource is one immutable, closed user-visible answer. MessageID
// names the terminal answer slot within its durable Turn, never a history index.
type ConversationSource struct {
	ID            string          `json:"id"`
	ThreadID      string          `json:"thread_id"`
	TurnID        string          `json:"turn_id"`
	Turn          uint64          `json:"turn"`
	MessageID     string          `json:"message_id"`
	ContentDigest string          `json:"content_digest"`
	Text          string          `json:"text"`
	Title         string          `json:"title,omitempty"`
	Items         []ReferenceItem `json:"items"`
	Structured    bool            `json:"structured,omitempty"`
}

// ReferenceItem identifies a byte range in the immutable source. Label is the
// original Markdown label; ParentID disambiguates nested lists and sections.
type ReferenceItem struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id,omitempty"`
	Label    string `json:"label,omitempty"`
	Kind     string `json:"kind"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
}

type ConversationSelection struct {
	GroupIDs          []string `json:"group_ids,omitempty"`
	ItemIDs           []string `json:"item_ids,omitempty"`
	SourceTurn        uint64   `json:"source_turn"`
	SourceTurnID      string   `json:"source_turn_id"`
	UserRequestDigest string   `json:"user_request_digest"`
}

type ConversationReplacement struct {
	OldGroupID        string `json:"old_group_id"`
	NewGroupID        string `json:"new_group_id"`
	SourceTurn        uint64 `json:"source_turn"`
	SourceTurnID      string `json:"source_turn_id"`
	UserRequestDigest string `json:"user_request_digest"`
}

// ConversationState records conversation sources and focus, not execution or
// verification facts. An explicit empty selection means a cleared focus.
type ConversationState struct {
	Sources      map[string]ConversationSource `json:"sources"`
	Selection    *ConversationSelection        `json:"selection,omitempty"`
	Replacements []ConversationReplacement     `json:"replacements,omitempty"`
}

func conversationID(kind string, value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return kind + ":" + hex.EncodeToString(sum[:])
}

func (s ConversationSource) identity() string {
	return conversationID("source", []any{s.ThreadID, s.TurnID, s.Turn, s.MessageID, s.ContentDigest})
}

func CloneConversation(value *ConversationState) *ConversationState {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.Sources = make(map[string]ConversationSource, len(value.Sources))
	for id, source := range value.Sources {
		source.Items = append([]ReferenceItem(nil), source.Items...)
		cloned.Sources[id] = source
	}
	if value.Selection != nil {
		selection := *value.Selection
		selection.GroupIDs = append([]string(nil), selection.GroupIDs...)
		selection.ItemIDs = append([]string(nil), selection.ItemIDs...)
		cloned.Selection = &selection
	}
	cloned.Replacements = append([]ConversationReplacement(nil), value.Replacements...)
	return &cloned
}

func (s *ConversationState) Superseded(id string) bool {
	if s == nil {
		return false
	}
	for _, replacement := range s.Replacements {
		if replacement.OldGroupID == id {
			return true
		}
	}
	return false
}

func (s *ConversationState) FindItem(id string) (ConversationSource, ReferenceItem, bool) {
	if s != nil {
		for _, source := range s.Sources {
			for _, item := range source.Items {
				if item.ID == id {
					return source, item, true
				}
			}
		}
	}
	return ConversationSource{}, ReferenceItem{}, false
}

// SourcesForTurn exposes immutable sources through the existing turn-history
// lookup even after a focus switch. It does not reactivate superseded sources.
func (s *ConversationState) SourcesForTurn(turn uint64) []ConversationSource {
	var sources []ConversationSource
	if s == nil {
		return nil
	}
	for _, source := range s.Sources {
		if source.Turn == turn {
			source.Items = slices.Clone(source.Items)
			sources = append(sources, source)
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	return sources
}

func (s *ConversationState) ValidateReferences(groups, items []string) error {
	seen := make(map[string]bool)
	for _, id := range groups {
		if s == nil {
			return errors.New("conversation source is unavailable")
		}
		if _, ok := s.Sources[id]; !ok || s.Superseded(id) || seen[id] {
			return fmt.Errorf("invalid or superseded conversation group %q", id)
		}
		seen[id] = true
	}
	for _, id := range items {
		source, _, ok := s.FindItem(id)
		if !ok || s.Superseded(source.ID) || seen[id] {
			return fmt.Errorf("invalid or superseded conversation item %q", id)
		}
		seen[id] = true
	}
	return nil
}

func (s *ConversationState) Validate() error {
	if s == nil {
		return nil
	}
	itemIDs := make(map[string]bool)
	for id, source := range s.Sources {
		if id != source.ID || id != source.identity() || source.ThreadID == "" || source.TurnID == "" || source.MessageID == "" || source.Turn == 0 ||
			source.Text == "" || !utf8.ValidString(source.Text) || source.ContentDigest != digestString(source.Text) || len(source.Items) == 0 {
			return errors.New("conversation source identity or content is invalid")
		}
		local := make(map[string]ReferenceItem)
		for _, item := range source.Items {
			if item.Start < 0 || item.End <= item.Start || item.End > len(source.Text) ||
				!utf8.ValidString(source.Text[item.Start:item.End]) || item.ID != referenceItemID(source.ID, item) || itemIDs[item.ID] {
				return errors.New("conversation item identity or range is invalid")
			}
			local[item.ID], itemIDs[item.ID] = item, true
		}
		for _, item := range source.Items {
			if item.ParentID != "" {
				parent, ok := local[item.ParentID]
				if !ok || parent.ID == item.ID || parent.Start > item.Start || parent.End < item.End || parent.Start == item.Start && parent.End == item.End {
					return errors.New("conversation item parent is invalid")
				}
			}
		}
	}
	replaced := make(map[string]bool)
	for _, replacement := range s.Replacements {
		_, oldOK := s.Sources[replacement.OldGroupID]
		_, newOK := s.Sources[replacement.NewGroupID]
		if !oldOK || !newOK || replacement.OldGroupID == replacement.NewGroupID || replaced[replacement.OldGroupID] ||
			replacement.SourceTurn == 0 || replacement.SourceTurnID == "" || replacement.UserRequestDigest == "" {
			return errors.New("conversation replacement is invalid")
		}
		replaced[replacement.OldGroupID] = true
	}
	// Replacement edges must be acyclic, including forged restored state.
	for id := range replaced {
		seen := make(map[string]bool)
		for id != "" {
			if seen[id] {
				return errors.New("conversation replacements contain a cycle")
			}
			seen[id] = true
			next := ""
			for _, replacement := range s.Replacements {
				if replacement.OldGroupID == id {
					next = replacement.NewGroupID
					break
				}
			}
			id = next
		}
	}
	if selection := s.Selection; selection != nil {
		if selection.SourceTurn == 0 || selection.SourceTurnID == "" || selection.UserRequestDigest == "" {
			return errors.New("conversation selection lacks user provenance")
		}
		return s.ValidateReferences(selection.GroupIDs, selection.ItemIDs)
	}
	return nil
}

func referenceItemID(sourceID string, item ReferenceItem) string {
	return conversationID("item", []any{sourceID, item.Kind, item.Start, item.End})
}

func (s *ConversationState) Add(source ConversationSource) error {
	if s == nil {
		return errors.New("conversation state is required")
	}
	if s.Sources == nil {
		s.Sources = make(map[string]ConversationSource)
	}
	if prior, exists := s.Sources[source.ID]; exists {
		if prior.ContentDigest != source.ContentDigest {
			return errors.New("conversation source conflict")
		}
		return nil
	}
	candidate := &ConversationState{Sources: map[string]ConversationSource{source.ID: source}}
	if err := candidate.Validate(); err != nil {
		return err
	}
	source.Items = slices.Clone(source.Items)
	s.Sources[source.ID] = source
	return nil
}

// CandidateSources keeps structured reports across progress answers. Without
// an explicit binding it exposes candidates to the main model, never guesses
// which report an ordinal names. The latest unstructured answer stays usable.
func (s *ConversationState) CandidateSources(plan Plan) []ConversationSource {
	if s == nil {
		return nil
	}
	wanted := make(map[string]bool)
	if s.Selection != nil {
		for _, id := range s.Selection.GroupIDs {
			wanted[id] = true
		}
		for _, id := range s.Selection.ItemIDs {
			if source, _, ok := s.FindItem(id); ok {
				wanted[source.ID] = true
			}
		}
	} else {
		var latest ConversationSource
		for _, source := range s.Sources {
			if s.Superseded(source.ID) {
				continue
			}
			if source.Structured {
				wanted[source.ID] = true
			}
			if source.Turn > latest.Turn || source.Turn == latest.Turn && source.ID > latest.ID {
				latest = source
			}
		}
		if latest.ID != "" {
			wanted[latest.ID] = true
		}
	}
	for _, step := range plan.Steps {
		if step.Done() {
			continue
		}
		for _, id := range step.ReferenceItemIDs {
			if source, _, ok := s.FindItem(id); ok && !s.Superseded(source.ID) {
				wanted[source.ID] = true
			}
		}
	}
	var sources []ConversationSource
	for id := range wanted {
		if source, ok := s.Sources[id]; ok && !s.Superseded(id) {
			sources = append(sources, source)
		}
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].Turn != sources[j].Turn {
			return sources[i].Turn < sources[j].Turn
		}
		return sources[i].ID < sources[j].ID
	})
	return sources
}

// RequiredSources excludes unbound candidates but retains explicit selection
// and unfinished plan dependencies.
func (s *ConversationState) RequiredSources(plan Plan) []ConversationSource {
	if s == nil {
		return nil
	}
	bound := *s
	if bound.Selection == nil {
		bound.Selection = &ConversationSelection{}
	}
	return bound.CandidateSources(plan)
}

func (s *ConversationState) SelectedItems(source ConversationSource, plan Plan) []ReferenceItem {
	if s.Selection == nil || slices.Contains(s.Selection.GroupIDs, source.ID) {
		return append([]ReferenceItem(nil), source.Items...)
	}
	ids := append([]string(nil), s.Selection.ItemIDs...)
	for _, step := range plan.Steps {
		if !step.Done() {
			ids = append(ids, step.ReferenceItemIDs...)
		}
	}
	var items []ReferenceItem
	for _, item := range source.Items {
		if slices.Contains(ids, item.ID) {
			items = append(items, item)
		}
	}
	return items
}

func (s *ConversationState) Select(selection ConversationSelection, replacements []ConversationReplacement) error {
	if s == nil {
		return errors.New("conversation state is required")
	}
	candidate := CloneConversation(s)
	if candidate == nil {
		candidate = &ConversationState{}
	}
	candidate.Selection = &selection
	candidate.Replacements = append(candidate.Replacements, replacements...)
	if err := candidate.Validate(); err != nil {
		return err
	}
	*s = *CloneConversation(candidate)
	return nil
}

func NewConversationSelection(groups, items []string, turn uint64, turnID, request string) ConversationSelection {
	return ConversationSelection{GroupIDs: slices.Clone(groups), ItemIDs: slices.Clone(items), SourceTurn: turn, SourceTurnID: turnID, UserRequestDigest: digestString(strings.TrimSpace(request))}
}

// Historical completed steps may still cite a superseded definition. Open
// steps must be rebound explicitly in the same update before replacement.
func (s *ConversationState) ValidatePlan(plan *Plan) error {
	if plan == nil {
		return nil
	}
	seen := make(map[string]bool)
	for _, step := range plan.Steps {
		if step.ID != "" {
			if seen[step.ID] {
				return fmt.Errorf("duplicate plan step id %q", step.ID)
			}
			seen[step.ID] = true
		}
		if !step.Done() {
			if err := s.ValidateReferences(nil, step.ReferenceItemIDs); err != nil {
				return err
			}
		} else {
			refs := make(map[string]bool)
			for _, id := range step.ReferenceItemIDs {
				if _, _, ok := s.FindItem(id); !ok || refs[id] {
					return fmt.Errorf("invalid historical plan reference %q", id)
				}
				refs[id] = true
			}
		}
	}
	return nil
}
