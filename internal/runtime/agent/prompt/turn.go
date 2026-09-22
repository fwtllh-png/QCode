package prompt

import (
	"fmt"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/repository"
)

const (
	PartitionRepoMap          = "repo_map"
	PartitionWorkingSetLedger = "working_set_ledger"
	PartitionEvidence         = "evidence"
	PartitionDirectoryRules   = "directory_rules"
)

// truncationNotice is reserved inside each section budget.
const truncationNotice = "(this section was cut to fit its budget; " +
	"what is not listed here may still exist)\n"

// TurnOptions uses workspace-relative paths matching the repository index.
type TurnOptions struct {
	Turn       uint64
	RepoMap    repository.Map
	WorkingSet []agentcontext.WorkingSetEntry
	Evidence   agentcontext.EvidenceSnapshot
	// DirectoryInstructions are the nearest sub-root instruction files for
	// the directories the working set touches. The section carries no turn
	// number: its content only changes when a rule file changes, so the world
	// baseline digest keeps it stable across turns.
	DirectoryInstructions []DirectoryInstruction
	Budgets               map[string]Budget
	Tokens                TokenCounter
}

// TurnContext is a snapshot or replacement delta of repository state.
type TurnContext struct {
	Messages   []provider.Message `json:"messages"`
	Receipts   []Receipt          `json:"receipts"`
	Selections []Selection        `json:"selections,omitempty"`
}

// Selection explains why one working-set path was offered to the model and
// whether the section budget retained its line.
type Selection struct {
	Path             string              `json:"path"`
	Kind             string              `json:"kind"`
	Reasons          []string            `json:"reasons"`
	Evidence         []SelectionEvidence `json:"evidence,omitempty"`
	Score            int                 `json:"score"`
	Critical         bool                `json:"critical,omitempty"`
	FirstTurn        uint64              `json:"first_turn"`
	LastTurn         uint64              `json:"last_turn"`
	Included         bool                `json:"included"`
	Truncated        bool                `json:"truncated,omitempty"`
	TruncationReason string              `json:"truncation_reason,omitempty"`
}

type SelectionEvidence struct {
	Kind   string `json:"kind"`
	Line   int    `json:"line,omitempty"`
	Symbol string `json:"symbol,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Turn   uint64 `json:"turn"`
}

// TurnState is the engine snapshot visible to a context provider.
type TurnState struct {
	Turn       uint64
	WorkingSet []agentcontext.WorkingSetEntry
	Evidence   agentcontext.EvidenceSnapshot
}

// AssembleTurn emits full sections initially and only digest changes afterwards.
func AssembleTurn(options TurnOptions) TurnContext {
	tokens := options.Tokens
	if tokens == nil {
		tokens = HeuristicTokenCounter{}
	}
	var result TurnContext
	appendSection := func(kind, text, sourcePath string) (string, string) {
		// The notice is charged to the budget up front, so a section that runs out
		// of room can still admit it: a model that cannot tell a cut from an
		// absence will read the missing lines as missing code.
		retained, reason := retain(text, options.Budgets[kind], len(truncationNotice), 0, tokens)
		result.Receipts = append(
			result.Receipts,
			newReceipt(kind, sourcePath, text, retained, reason, tokens),
		)
		if reason != "" {
			retained += truncationNotice
		}
		if strings.TrimSpace(retained) != "" {
			result.Messages = append(
				result.Messages,
				provider.TextMessage(provider.RoleSystem, retained),
			)
		}
		return retained, reason
	}
	if options.RepoMap.Status != "" {
		_, _ = appendSection(PartitionRepoMap, renderRepoMap(options), "session://repo-map")
	}
	if len(options.DirectoryInstructions) != 0 {
		_, _ = appendSection(
			PartitionDirectoryRules,
			renderDirectoryRules(options),
			"session://directory-rules",
		)
	}
	if len(options.WorkingSet) != 0 {
		text := renderWorkingSet(options)
		retained, reason := appendSection(
			PartitionWorkingSetLedger, text, "session://working-set",
		)
		result.Selections = explainSelections(options, retained, reason)
	}
	if !options.Evidence.Empty() {
		text := renderEvidence(options)
		_, _ = appendSection(PartitionEvidence, text, "session://evidence")
	}
	return result
}

// renderRepoMap describes the repository, or says why it cannot.
func renderRepoMap(options TurnOptions) string {
	built := options.RepoMap
	var b strings.Builder
	// No turn number: the section digest must only change when the indexed
	// repository changes, so an untouched map stays in the world baseline
	// instead of being re-sent as a fresh delta every turn.
	fmt.Fprintf(&b, "[repo_map index=%s]\n", built.Status)
	if !built.Ready() {
		// Naming the reason matters more than the map: a model that reads "not
		// found" as "does not exist" will draw the wrong conclusion.
		b.WriteString(unavailableRepoMap(built))
		return b.String()
	}
	if built.FileCount == 0 {
		return ""
	}
	fmt.Fprintf(&b, "%d indexed files, %d declarations (lexical).\n", built.FileCount, built.SymbolCount)
	if len(built.Build) != 0 {
		fmt.Fprintf(&b, "build: %s\n", strings.Join(built.Build, ", "))
	}
	if len(built.Entries) != 0 {
		fmt.Fprintf(&b, "entry: %s\n", strings.Join(built.Entries, ", "))
	}
	if len(built.Directories) != 0 {
		b.WriteString("directories:\n")
		for _, directory := range built.Directories {
			fmt.Fprintf(&b, "  %s — %d files, %d declarations", directory.Path, directory.Files, directory.Symbols)
			if len(directory.Languages) != 0 {
				fmt.Fprintf(&b, ", %s", strings.Join(directory.Languages, "/"))
			}
			b.WriteByte('\n')
		}
		if built.OmittedDirectories > 0 {
			fmt.Fprintf(&b, "  (%d more directories not listed)\n", built.OmittedDirectories)
		}
	}
	if len(built.Outlines) != 0 {
		b.WriteString("declarations in the files below:\n")
		for _, outline := range built.Outlines {
			fmt.Fprintf(&b, "  %s\n", outline.Path)
			for _, symbol := range outline.Symbols {
				fmt.Fprintf(&b, "    %d %s %s", symbol.Line, symbol.Kind, symbol.Name)
				if symbol.Container != "" {
					fmt.Fprintf(&b, " (in %s)", symbol.Container)
				}
				// The signature is what the model would otherwise re-read the
				// file for; the resolution suffix keeps a heuristic row from
				// reading as a rule-table one.
				if symbol.Signature != "" {
					fmt.Fprintf(&b, " :: %s", symbol.Signature)
					if symbol.Resolution == repoindex.ResolutionHeuristic {
						b.WriteString(" [heuristic]")
					}
				}
				b.WriteByte('\n')
			}
			if outline.Truncated {
				b.WriteString("    (more declarations not listed)\n")
			}
		}
	}
	return b.String()
}

// renderDirectoryRules injects the local rules of the directories the session
// is actually working in. Root-level rules are not repeated.
func renderDirectoryRules(options TurnOptions) string {
	var b strings.Builder
	b.WriteString("[directory_rules]\n")
	b.WriteString(
		"Local rules for directories this session is working in. " +
			"Workspace-root rules above still apply.\n",
	)
	for _, instruction := range options.DirectoryInstructions {
		fmt.Fprintf(&b, "%s — from %s:\n%s\n", instruction.Dir, instruction.Path, instruction.Text)
	}
	return b.String()
}

// renderWorkingSet lists the paths in play. It carries names and provenance, not
// contents: what the agent read is already in the history, and re-sending it
// would be paid for on every sample.
func renderWorkingSet(options TurnOptions) string {
	if len(options.WorkingSet) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[working_set turn=%d]\n", options.Turn)
	b.WriteString(
		"Paths this session has touched, most relevant first. " +
			"Contents are not included; a listed path does not imply every window was read. " +
			"Reuse prior text when it covers the current question, requested window, " +
			"and file version. Use turn_history or result_get for prior text outside " +
			"this sample; file_read uncovered windows, changed content, or unavailable " +
			"prior text as needed for read-only analysis or edits. " +
			"A dirty git status or git_diff is not a " +
			"reason to file_read. After search_text returns line hits, prefer " +
			"that window and expand only as needed to answer the task.\n",
	)
	for _, entry := range options.WorkingSet {
		b.WriteString(renderWorkingSetEntry(entry))
	}
	return b.String()
}

func renderWorkingSetEntry(entry agentcontext.WorkingSetEntry) string {
	line := fmt.Sprintf(
		"  %s — %s (turn %d)", entry.Path, joinSources(entry.Sources), entry.LastTurn,
	)
	if entry.Critical {
		line += " critical"
	}
	return line + "\n"
}

func explainSelections(options TurnOptions, retained, truncationReason string) []Selection {
	explanations := make([]Selection, 0, len(options.WorkingSet))
	for _, entry := range options.WorkingSet {
		included := truncationReason == "" ||
			strings.Contains(retained, renderWorkingSetEntry(entry))
		selection := Selection{
			Path: entry.Path, Kind: selectionKind(entry.Path, options.Evidence),
			Score: entry.Score, Critical: entry.Critical,
			FirstTurn: entry.FirstTurn, LastTurn: entry.LastTurn,
			Included: included, Truncated: !included,
		}
		for _, source := range entry.Sources {
			selection.Reasons = append(selection.Reasons, string(source))
		}
		for _, fact := range options.Evidence.Facts {
			if fact.Path != entry.Path {
				continue
			}
			selection.Evidence = append(selection.Evidence, SelectionEvidence{
				Kind: string(fact.Kind), Line: fact.Line, Symbol: fact.Symbol,
				Tool: fact.Tool, Turn: fact.Turn,
			})
		}
		if selection.Truncated {
			selection.TruncationReason = truncationReason
		}
		explanations = append(explanations, selection)
	}
	return explanations
}

func selectionKind(path string, snapshot agentcontext.EvidenceSnapshot) string {
	for _, fact := range snapshot.Facts {
		if fact.Path == path && fact.Kind == agentcontext.KindTest {
			return "test"
		}
	}
	lower := strings.ToLower(path)
	if strings.Contains(lower, "/tests/") ||
		strings.Contains(lower, "_test.") ||
		strings.Contains(lower, ".test.") ||
		strings.Contains(lower, ".spec.") {
		return "test"
	}
	return "file"
}

// renderEvidence reports what the thread knows and what it owes.
//
// The order is deliberate and the opposite of how the section was built:
// reminders and risks come before the facts because a budget cut keeps the
// prefix, and advice the model can act on now is worth more than another list of
// paths it has already seen.
func renderEvidence(options TurnOptions) string {
	snapshot := options.Evidence
	var b strings.Builder
	fmt.Fprintf(&b, "[evidence turn=%d]\n", options.Turn)
	if len(snapshot.Reminders) != 0 {
		b.WriteString("wasted effort:\n")
		for _, reminder := range snapshot.Reminders {
			fmt.Fprintf(&b, "  %s\n", reminder.Detail)
		}
	}
	if len(snapshot.Risks) != 0 {
		b.WriteString("unproved, and yours to close:\n")
		for _, risk := range snapshot.Risks {
			fmt.Fprintf(&b, "  %s — %s (turn %d)\n", risk.Path, riskLabel(risk.Kind), risk.Turn)
		}
	}
	if len(snapshot.Facts) != 0 {
		b.WriteString("what lookups established:\n")
		for _, fact := range snapshot.Facts {
			b.WriteString("  " + fact.Describe() + "\n")
		}
		if snapshot.OmittedFacts > 0 {
			fmt.Fprintf(&b, "  (%d more not listed)\n", snapshot.OmittedFacts)
		}
	}
	return b.String()
}

// riskLabel says what is missing in words, because a model asked to close a gap
// should not have to decode an identifier.
func riskLabel(kind string) string {
	switch kind {
	case agentcontext.RiskUnverifiedChange:
		return "changed, nothing verified it"
	case agentcontext.RiskBlindChange:
		return "changed without being read first"
	case agentcontext.RiskOpenDiagnostics:
		return "diagnostics still failing"
	default:
		return kind
	}
}

func joinSources(sources []agentcontext.WorkingSetSource) string {
	if len(sources) == 0 {
		return "touched"
	}
	names := make([]string, 0, len(sources))
	for _, source := range sources {
		names = append(names, string(source))
	}
	return strings.Join(names, ", ")
}

func unavailableRepoMap(built repository.Map) string {
	reason := strings.TrimSpace(built.Detail)
	if reason == "" {
		reason = "no detail was reported"
	}
	return fmt.Sprintf(
		"No repository map is available (%s). "+
			"A path missing from this section may still exist; use search_text to check.\n",
		reason,
	)
}
