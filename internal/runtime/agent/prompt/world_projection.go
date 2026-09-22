package prompt

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

type MemorySnapshot struct {
	Generation     uint64
	Body           string
	Source         string
	Digest         string
	CandidateCount int
	SelectedIDs    []string
	Truncated      bool
	FailureReason  string
}

type SkillSummary struct {
	Name           string
	Description    string
	Source         string
	Path           string
	Handle         string
	PackageHandle  string
	ResourceHandle string
}

type SkillSelectionMetrics struct {
	Method                string
	CatalogSize           int
	CandidateSize         int
	VisibleSize           int
	ExplicitMatches       int
	QueryTerms            int
	QueryTruncated        bool
	CandidateSetTruncated bool
	OriginalTokens        uint64
	ProjectedTokens       uint64
	TokenSavings          float64
	Recall                float64
	Precision             float64
	CacheHit              bool
}

type RepositoryContext interface {
	Build(context.Context, TurnState) TurnContext
}

type WorldProjectionInput struct {
	Context      context.Context
	History      []provider.Message
	Stable       []provider.Message
	Catalog      tool.CatalogSnapshot
	Advertised   map[string]bool
	Baseline     agentcontext.WorldBaseline
	Turn         uint64
	Mode         string
	ImageInput   bool
	Policy       *policy.Runtime
	CodingPolicy bool
	Memory       MemorySnapshot
	Skills       []SkillSummary
	Budgets      map[string]Budget
	Repository   RepositoryContext
	WorkingSet   []agentcontext.WorkingSetEntry
	Evidence     agentcontext.EvidenceSnapshot
	PlanText     string
	PlanReceipt  *Receipt
	SessionState string
	Narrative    string
}

type WorldProjectionResult struct {
	Stable     []provider.Message
	Delta      []provider.Message
	Receipts   []Receipt
	Projection agentcontext.WorldProjection
	Selections []Selection
}

func FrozenWorld(
	stable []provider.Message,
	receipts []Receipt,
	baseline agentcontext.WorldBaseline,
) ([]provider.Message, []provider.Message, []Receipt, agentcontext.WorldProjection, error) {
	return agentcontext.CloneMessages(stable), nil, append([]Receipt(nil), receipts...),
		agentcontext.WorldProjection{Mode: agentcontext.WorldPatch, Baseline: baseline}, nil
}

func ProjectWorldState(
	input WorldProjectionInput,
) (WorldProjectionResult, error) {
	var sections []agentcontext.WorldSection
	var receipts []Receipt
	budget := func(kind string) Budget {
		return input.Budgets[kind]
	}
	appendSection := func(id, source, body, digest string) {
		messages, receipt := AssembleWorldText(
			id,
			source,
			body,
			budget(id),
		)
		if digest != "" {
			receipt.Digest = digest
		}
		sections = append(
			sections,
			WorldSectionFromReceipt(receipt, FirstMessage(messages), input.Turn),
		)
		receipts = append(receipts, receipt)
	}
	appendSection(
		PartitionMode,
		"session://profile.mode",
		ModeInstructionPack(input.Mode, input.ImageInput),
		"",
	)
	policySection := NewPolicySection(input.Policy)
	appendSection(
		policySection.ID(),
		"worldstate://"+policySection.ID(),
		policySection.Render(),
		policySection.Digest(),
	)
	if input.CodingPolicy {
		coding := NewCodingPolicySection()
		appendSection(
			coding.ID(),
			"worldstate://"+coding.ID(),
			coding.Render(),
			coding.Digest(),
		)
	}
	if section, receipt, ok := MemoryWorldSection(
		input.Memory,
		budget(PartitionUserMemory),
		input.Turn,
	); ok {
		if section.ID != "" {
			sections = append(sections, section)
		}
		receipts = append(receipts, receipt)
	}
	if body := RenderSkillWorld(input.Skills); body != "" {
		appendSection(PartitionSkills, "skill://catalog", body, "")
	}
	catalogMessages, catalogReceipt := AssembleToolCatalog(
		NewToolCatalogSectionFromSnapshot(input.Catalog, input.Advertised),
		budget(PartitionToolCatalog),
	)
	sections = append(sections, WorldSectionFromReceipt(
		catalogReceipt,
		FirstMessage(catalogMessages),
		input.Turn,
	))
	if catalogReceipt.OriginalBytes != 0 ||
		WorldBaselineHas(input.Baseline, PartitionToolCatalog) {
		receipts = append(receipts, catalogReceipt)
	}
	var built TurnContext
	if input.Repository != nil {
		built = input.Repository.Build(input.Context, TurnState{
			Turn: input.Turn, WorkingSet: input.WorkingSet,
			Evidence: input.Evidence,
		})
	}
	sections = append(
		sections,
		WorldSectionsFromTurnContext(built, input.Turn)...,
	)
	receipts = append(receipts, built.Receipts...)
	if input.PlanReceipt != nil {
		message := provider.TextMessage(provider.RoleSystem, input.PlanText)
		sections = append(sections, WorldSectionFromReceipt(
			*input.PlanReceipt,
			&message,
			input.Turn,
		))
		receipts = append(receipts, *input.PlanReceipt)
	}
	if strings.TrimSpace(input.SessionState) != "" {
		appendSection(
			PartitionSessionState,
			"session://session-state",
			input.SessionState,
			"",
		)
	}
	if strings.TrimSpace(input.Narrative) != "" {
		appendSection(
			PartitionNarrative,
			"session://narrative",
			input.Narrative,
			"",
		)
	}
	projection, err := agentcontext.ProjectWorld(
		sections,
		input.Baseline,
		input.History,
	)
	if err != nil {
		return WorldProjectionResult{}, err
	}
	return WorldProjectionResult{
		Stable:     agentcontext.CloneMessages(input.Stable),
		Delta:      agentcontext.CloneMessages(projection.Messages),
		Receipts:   ProjectWorldReceipts(receipts, projection.Changed),
		Projection: projection,
		Selections: append([]Selection(nil), built.Selections...),
	}, nil
}

func MemoryWorldSection(
	memory MemorySnapshot,
	budget Budget,
	turn uint64,
) (agentcontext.WorldSection, Receipt, bool) {
	if strings.TrimSpace(memory.Body) == "" {
		if memory.FailureReason != "" {
			return agentcontext.WorldSection{}, Receipt{
				Kind:             PartitionUserMemory,
				SourcePath:       memory.Source,
				Truncated:        true,
				TruncationReason: memory.FailureReason,
				Generation:       memory.Generation,
			}, true
		}
		return agentcontext.WorldSection{}, Receipt{}, false
	}
	messages, receipt := AssembleWorldText(
		PartitionUserMemory,
		memory.Source,
		memory.Body,
		budget,
	)
	if memory.Digest != "" {
		receipt.Digest = memory.Digest
	}
	receipt.Generation = memory.Generation
	receipt.CandidateCount = memory.CandidateCount
	receipt.SelectedIDs = append([]string(nil), memory.SelectedIDs...)
	receipt.Truncated = receipt.Truncated || memory.Truncated
	return WorldSectionFromReceipt(
		receipt,
		FirstMessage(messages),
		turn,
	), receipt, true
}

func RenderSkillWorld(values []SkillSummary) string {
	if len(values) == 0 {
		return ""
	}
	values = append([]SkillSummary(nil), values...)
	sort.Slice(values, func(i, j int) bool {
		if values[i].Name != values[j].Name {
			return values[i].Name < values[j].Name
		}
		if values[i].Source != values[j].Source {
			return values[i].Source < values[j].Source
		}
		return values[i].Path < values[j].Path
	})
	var builder strings.Builder
	builder.WriteString(
		"Selected skills (metadata only). Use skills_read with any exact advertised handle (handle, package, or resource) before following instructions. Use skills_list for more.\n",
	)
	for _, value := range values {
		builder.WriteString("- name=")
		builder.WriteString(strconv.Quote(value.Name))
		builder.WriteString(" description=")
		description := strings.TrimSpace(value.Description)
		runes := []rune(description)
		if len(runes) > 160 {
			description = string(runes[:157]) + "..."
		}
		builder.WriteString(strconv.Quote(description))
		builder.WriteString(" source=")
		builder.WriteString(strconv.Quote(value.Source))
		builder.WriteString(" source_path=")
		builder.WriteString(strconv.Quote(value.Path))
		builder.WriteString(" handle=")
		builder.WriteString(strconv.Quote(value.Handle))
		builder.WriteString(" package=")
		builder.WriteString(strconv.Quote(value.PackageHandle))
		builder.WriteString(" resource=")
		builder.WriteString(strconv.Quote(value.ResourceHandle))
		builder.WriteByte('\n')
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

func ProjectWorldReceipts(
	receipts []Receipt,
	changed []string,
) []Receipt {
	changedSet := make(map[string]struct{}, len(changed))
	for _, id := range changed {
		changedSet[id] = struct{}{}
	}
	result := append([]Receipt(nil), receipts...)
	for index := range result {
		if _, changed := changedSet[result[index].Kind]; changed {
			continue
		}
		result[index].RetainedBytes = 0
		result[index].RetainedTokens = 0
	}
	return result
}

func WorldSectionsFromTurnContext(
	built TurnContext,
	turn uint64,
) []agentcontext.WorldSection {
	var result []agentcontext.WorldSection
	messageIndex := 0
	for _, receipt := range built.Receipts {
		var message *provider.Message
		if receipt.RetainedBytes != 0 && messageIndex < len(built.Messages) {
			copy := agentcontext.CloneMessages(
				[]provider.Message{built.Messages[messageIndex]},
			)[0]
			messageIndex++
			message = &copy
		}
		result = append(result, WorldSectionFromReceipt(receipt, message, turn))
	}
	return result
}

func WorldSectionFromReceipt(
	receipt Receipt,
	message *provider.Message,
	turn uint64,
) agentcontext.WorldSection {
	if message != nil {
		message.Turn = turn
	}
	return agentcontext.WorldSection{
		ID: receipt.Kind, Digest: receipt.Digest,
		Present: receipt.OriginalBytes != 0, Message: message,
	}
}

func FirstMessage(messages []provider.Message) *provider.Message {
	if len(messages) == 0 {
		return nil
	}
	message := agentcontext.CloneMessages(messages[:1])[0]
	return &message
}

func WorldBaselineHas(baseline agentcontext.WorldBaseline, id string) bool {
	for _, entry := range baseline.Entries {
		if entry.ID == id {
			return true
		}
	}
	return false
}
