package agentcontext

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	TruthSchemaVersion        = 2
	TruthMarkerStart          = "<qcode_truth_capsule>"
	TruthMarkerEnd            = "</qcode_truth_capsule>"
	DownshiftRuntimeTruthOnly = "runtime_truth_only"

	EntityGoal          = "goal"
	EntityTodo          = "todo"
	EntityFailure       = "failure"
	EntityChange        = "change"
	EntityCriticalPath  = "critical_path"
	EntityFact          = "fact"
	EntityPendingInput  = "pending_input"
	EntityContentHandle = "content_handle"
)

var entityKinds = map[string]struct{}{
	EntityGoal: {}, EntityTodo: {}, EntityFailure: {}, EntityChange: {},
	EntityCriticalPath: {}, EntityFact: {}, EntityPendingInput: {},
	EntityContentHandle: {},
}

type TruthEntity struct {
	ID                   string               `json:"i"`
	Kind                 string               `json:"k"`
	Key                  string               `json:"x"`
	Value                string               `json:"v"`
	Source               string               `json:"s"`
	Owner                string               `json:"u"`
	Retention            RetentionClass       `json:"l"`
	Status               string               `json:"a,omitempty"`
	Turn                 uint64               `json:"t,omitempty"`
	Count                int                  `json:"n,omitempty"`
	Read                 bool                 `json:"r,omitempty"`
	Verified             bool                 `json:"q,omitempty"`
	Diagnostics          bool                 `json:"f,omitempty"`
	Consumed             bool                 `json:"o,omitempty"`
	VerificationSource   string               `json:"z,omitempty"`
	WorkspacePath        string               `json:"wp,omitempty"`
	WorkspaceDigest      string               `json:"wd,omitempty"`
	WorkspaceClaimStatus WorkspaceClaimStatus `json:"ws,omitempty"`
}

func NewTruthEntity(
	kind string,
	key string,
	value string,
	source string,
) TruthEntity {
	key = strings.TrimSpace(key)
	entity := TruthEntity{
		ID: StableEntityID(kind, key), Kind: kind, Key: key,
		Value: strings.TrimSpace(value), Source: strings.TrimSpace(source),
	}
	entity.normalizeLifecycle()
	return entity
}

func StableEntityID(kind, key string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + strings.TrimSpace(key)))
	return kind + ":" + hex.EncodeToString(sum[:12])
}

func (e TruthEntity) Validate() error {
	if _, ok := entityKinds[e.Kind]; !ok {
		return fmt.Errorf("unknown truth entity kind %q", e.Kind)
	}
	if e.Key == "" || e.Value == "" || e.Source == "" ||
		e.Owner == "" || !e.Retention.Valid() ||
		e.ID != StableEntityID(e.Kind, e.Key) {
		return fmt.Errorf("invalid %s truth entity identity", e.Kind)
	}
	if e.Verified && e.VerificationSource != "runtime.evidence" {
		return errors.New("verified truth requires runtime evidence")
	}
	if !e.Verified && e.VerificationSource != "" {
		return errors.New("unverified truth cannot name verification evidence")
	}
	if e.WorkspaceClaimStatus != "" &&
		e.WorkspaceClaimStatus != WorkspaceClaimCurrent &&
		e.WorkspaceClaimStatus != WorkspaceClaimStale {
		return errors.New("workspace claim status is invalid")
	}
	if e.WorkspaceClaimStatus != "" &&
		(e.WorkspacePath == "" || e.WorkspaceDigest == "") {
		return errors.New("workspace claim binding is incomplete")
	}
	if e.WorkspaceClaimStatus == WorkspaceClaimStale && e.Verified {
		return errors.New("stale workspace truth cannot remain verified")
	}
	return nil
}

type TruthCapsule struct {
	SchemaVersion     int           `json:"v"`
	Generation        uint64        `json:"g"`
	CompatibilityHash string        `json:"c"`
	ModelID           string        `json:"m"`
	ContextTokens     uint64        `json:"w"`
	DownshiftPolicy   string        `json:"p"`
	Entities          []TruthEntity `json:"e,omitempty"`
	Omissions         []Omission    `json:"o,omitempty"`
	Digest            string        `json:"d"`
}

func (c *TruthCapsule) Seal() {
	entities := make(map[string]TruthEntity, len(c.Entities))
	for _, entity := range c.Entities {
		entity.normalizeLifecycle()
		entities[entity.ID] = entity
	}
	c.Entities = c.Entities[:0]
	for _, entity := range entities {
		c.Entities = append(c.Entities, entity)
	}
	sort.Slice(c.Entities, func(i, j int) bool {
		if c.Entities[i].Kind != c.Entities[j].Kind {
			return c.Entities[i].Kind < c.Entities[j].Kind
		}
		return c.Entities[i].ID < c.Entities[j].ID
	})
	sort.Slice(c.Omissions, func(i, j int) bool {
		if c.Omissions[i].Class != c.Omissions[j].Class {
			return c.Omissions[i].Class < c.Omissions[j].Class
		}
		if c.Omissions[i].Kind != c.Omissions[j].Kind {
			return c.Omissions[i].Kind < c.Omissions[j].Kind
		}
		return c.Omissions[i].Reason < c.Omissions[j].Reason
	})
	c.Digest = c.digest()
}

func (c TruthCapsule) Validate() error {
	if c.SchemaVersion != TruthSchemaVersion {
		return errors.New("truth capsule schema version is invalid")
	}
	if c.Generation == 0 || c.CompatibilityHash == "" || c.ModelID == "" ||
		c.DownshiftPolicy != DownshiftRuntimeTruthOnly {
		return errors.New("truth capsule header is invalid")
	}
	if c.Digest == "" || c.Digest != c.digest() {
		return errors.New("truth capsule digest is invalid")
	}
	seen := make(map[string]struct{}, len(c.Entities))
	for _, entity := range c.Entities {
		if err := entity.Validate(); err != nil {
			return err
		}
		if _, exists := seen[entity.ID]; exists {
			return fmt.Errorf("duplicate truth entity %q", entity.ID)
		}
		seen[entity.ID] = struct{}{}
	}
	for _, omission := range c.Omissions {
		if err := omission.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (c TruthCapsule) digest() string {
	c.Digest = ""
	encoded, _ := json.Marshal(c)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func ParseTruthCapsule(text string) (TruthCapsule, bool, error) {
	start := strings.Index(text, TruthMarkerStart)
	if start < 0 {
		return TruthCapsule{}, false, nil
	}
	rest := text[start+len(TruthMarkerStart):]
	end := strings.Index(rest, TruthMarkerEnd)
	if end < 0 {
		return TruthCapsule{}, true, errors.New("truth capsule is not closed")
	}
	var capsule TruthCapsule
	if err := json.Unmarshal([]byte(strings.TrimSpace(rest[:end])), &capsule); err != nil {
		return TruthCapsule{}, true, fmt.Errorf("decode truth capsule: %w", err)
	}
	if err := capsule.Validate(); err != nil {
		return TruthCapsule{}, true, err
	}
	return capsule, true, nil
}

type Narrative struct {
	Items []NarrativeItem `json:"items,omitempty"`
	// Lines is retained only for deterministic legacy render callers. New
	// semantic artifacts always use Items and never recursively summarize Lines.
	Lines []string `json:"-"`
}

type StructuredRender struct {
	Text              string
	Truncated         bool
	Sections          []string
	NarrativeIncluded bool
	CapsuleBytes      int
}

func RenderStructured(
	summary Summary, capsule TruthCapsule, narrative Narrative, budget int,
) (StructuredRender, error) {
	if err := capsule.Validate(); err != nil {
		return StructuredRender{}, err
	}
	encoded, err := json.Marshal(capsule)
	if err != nil {
		return StructuredRender{}, err
	}
	header := fmt.Sprintf(
		"Compacted truth; replaces %d message(s).\n",
		summary.Window,
	)
	truthBlock := TruthMarkerStart + "\n" + string(encoded) + "\n" + TruthMarkerEnd + "\n"
	mandatory := MarkerStart + "\n" + header + truthBlock
	minimum := len(mandatory) + len(MarkerEnd)
	if budget > 0 && minimum > budget {
		return StructuredRender{}, fmt.Errorf(
			"truth capsule requires %d bytes; summary budget is %d",
			minimum,
			budget,
		)
	}
	// Digest stays optional and last so a tight budget drops the oldest
	// transcript lines first. The next compaction merges the structured
	// digest through CarriedDigest instead of flattening the whole summary.
	blocks := summary.blocks()
	room := unbounded
	if budget > 0 {
		room = max(0, budget-minimum-len(truncationNotice))
	}
	var optional strings.Builder
	sections := []string{SectionTruth}
	truncated := false
	for _, block := range blocks {
		text := block.render(remaining(room, optional.Len()))
		if text == "" {
			truncated = true
			continue
		}
		optional.WriteString(text)
		sections = append(sections, block.name)
		if block.partial {
			truncated = true
		}
	}
	narrativeIncluded := false
	narrativeLines := narrative.renderLines()
	if len(narrativeLines) != 0 {
		text, partial := renderNarrative(
			narrativeLines,
			remaining(room, optional.Len()),
		)
		if text == "" {
			truncated = true
		} else {
			optional.WriteString(text)
			sections = append(sections, SectionNarrative)
			narrativeIncluded = true
			truncated = truncated || partial
		}
	}
	var output strings.Builder
	output.WriteString(mandatory)
	output.WriteString(optional.String())
	if truncated {
		output.WriteString(truncationNotice)
	}
	output.WriteString(MarkerEnd)
	return StructuredRender{
		Text: output.String(), Truncated: truncated, Sections: sections,
		NarrativeIncluded: narrativeIncluded, CapsuleBytes: len(truthBlock),
	}, nil
}

func (n Narrative) renderLines() []string {
	if len(n.Items) == 0 {
		return append([]string(nil), n.Lines...)
	}
	result := make([]string, 0, len(n.Items))
	for _, item := range n.Items {
		result = append(result, item.Kind+": "+item.Text)
	}
	return result
}

func renderNarrative(lines []string, room int) (string, bool) {
	header := "Narrative context (non-authoritative, newest first):\n"
	if room != unbounded && len(header) >= room {
		return "", true
	}
	var output strings.Builder
	output.WriteString(header)
	for index, line := range lines {
		entry := "  " + collapse(line) + "\n"
		if room != unbounded && output.Len()+len(entry) > room {
			return output.String(), index != len(lines)
		}
		output.WriteString(entry)
	}
	return output.String(), false
}
