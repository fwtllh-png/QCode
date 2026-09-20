package prompt

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/adapter/memory"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/platform/tokenestimate"
)

var repositoryInstructionNames = []string{
	"AGENTS.md",
	filepath.Join(".qcode", "instructions.md"),
}

type Options struct {
	BaseSystem, Workspace, ToolPrefix, Constitution string
	Budgets                                         map[string]Budget
	WorkingSet                                      []FileContext
	MemoryEnabled                                   bool
	Memory                                          *memory.Store
	Loader                                          FileLoader
	Tokens                                          TokenCounter
}

const (
	PartitionBase         = "base_system"
	PartitionMode         = "mode"
	PartitionRepository   = "repository_instruction"
	PartitionWorkingSet   = "working_set"
	PartitionSkills       = "skills"
	PartitionUserMemory   = "user_memory"
	PartitionToolPrefix   = "tool_prefix"
	PartitionPlan         = "plan"
	PartitionSessionState = "session_state"
	PartitionNarrative    = "narrative"
	PartitionConstitution = "constitution"
	PartitionTotal        = "total"
)

type Budget struct {
	MaxBytes  int    `json:"max_bytes"`
	MaxTokens uint64 `json:"max_tokens"`
}

type FileContext struct {
	Path     string  `json:"path"`
	Content  *string `json:"-"`
	Critical bool    `json:"critical,omitempty"`
}

type FileLoader interface {
	Load(string) ([]byte, error)
}

type OSFileLoader struct{}

func (OSFileLoader) Load(path string) ([]byte, error) {
	return os.ReadFile(path)
}

type TokenCounter interface {
	Count(string) uint64
}

// HeuristicTokenCounter is the baseline token counter for prompt partitions:
// dense-script text counts per character, everything else at four characters
// per token. See internal/platform/tokenestimate for the rationale.
type HeuristicTokenCounter struct{}

func (HeuristicTokenCounter) Count(value string) uint64 {
	return tokenestimate.Text(value)
}

type Receipt struct {
	Kind             string   `json:"kind"`
	SourcePath       string   `json:"source_path"`
	OriginalBytes    int      `json:"original_bytes"`
	RetainedBytes    int      `json:"retained_bytes"`
	OriginalTokens   uint64   `json:"original_tokens"`
	RetainedTokens   uint64   `json:"retained_tokens"`
	Digest           string   `json:"digest"`
	Truncated        bool     `json:"truncated"`
	TruncationReason string   `json:"truncation_reason,omitempty"`
	Generation       uint64   `json:"generation,omitempty"`
	CandidateCount   int      `json:"candidate_count,omitempty"`
	SelectedIDs      []string `json:"selected_ids,omitempty"`
}

type Context struct {
	Messages      []provider.Message `json:"messages"`
	Receipts      []Receipt          `json:"receipts"`
	WorkingSet    []string           `json:"working_set,omitempty"`
	CriticalPaths []string           `json:"critical_paths,omitempty"`
}

// Assemble builds system context in a stable order and reads instructions only
// from fixed paths rooted inside Workspace.
func Assemble(options Options) (Context, error) {
	workspace, err := canonicalWorkspace(options.Workspace)
	if err != nil {
		return Context{}, err
	}
	loader := options.Loader
	if loader == nil {
		loader = OSFileLoader{}
	}
	tokens := options.Tokens
	if tokens == nil {
		tokens = HeuristicTokenCounter{}
	}
	type usage struct {
		bytes  int
		tokens uint64
	}
	budgets := make(map[string]Budget, len(options.Budgets)+1)
	for name, budget := range options.Budgets {
		budgets[name] = budget
	}
	used := make(map[string]usage)
	var total usage
	var result Context
	totalBudget := budgets[PartitionTotal]
	budgetFor := func(kind string) (Budget, bool) {
		budget := budgets[kind]
		available := true
		if totalBudget.MaxBytes > 0 {
			remaining := max(0, totalBudget.MaxBytes-total.bytes)
			available = available && remaining > 0
			if budget.MaxBytes <= 0 || budget.MaxBytes > remaining {
				budget.MaxBytes = remaining
			}
		}
		if totalBudget.MaxTokens > 0 {
			remaining := totalBudget.MaxTokens -
				min(totalBudget.MaxTokens, total.tokens)
			available = available && remaining > 0
			if budget.MaxTokens == 0 || budget.MaxTokens > remaining {
				budget.MaxTokens = remaining
			}
		}
		return budget, available
	}
	appendSection := func(kind, text, sourcePath string) {
		budget, available := budgetFor(kind)
		current := used[kind]
		retained, reason := "", "shared_budget"
		if available {
			retained, reason = retain(text, budget, current.bytes, current.tokens, tokens)
		}
		receipt := newReceipt(kind, sourcePath, text, retained, reason, tokens)
		result.Receipts = append(result.Receipts, receipt)
		current.bytes += receipt.RetainedBytes
		current.tokens += receipt.RetainedTokens
		used[kind] = current
		total.bytes += receipt.RetainedBytes
		total.tokens += receipt.RetainedTokens
		if strings.TrimSpace(retained) != "" {
			result.Messages = append(result.Messages, provider.TextMessage(provider.RoleSystem, retained))
		}
	}
	appendFragmentSection := func(fragment FragmentKind, kind, text, sourcePath string) {
		budget, available := budgetFor(kind)
		current := used[kind]
		retained, reason := "", "shared_budget"
		if available {
			retained, reason = retain(text, budget, current.bytes, current.tokens, tokens)
		}
		receipt := newReceipt(kind, sourcePath, text, retained, reason, tokens)
		result.Receipts = append(result.Receipts, receipt)
		current.bytes += receipt.RetainedBytes
		current.tokens += receipt.RetainedTokens
		used[kind] = current
		total.bytes += receipt.RetainedBytes
		total.tokens += receipt.RetainedTokens
		if wrapped := WrapFragment(fragment, retained); wrapped != "" {
			result.Messages = append(result.Messages, provider.TextMessage(provider.RoleSystem, wrapped))
		}
	}
	appendSection(PartitionBase, options.BaseSystem, "builtin://base-system")
	for _, name := range repositoryInstructionNames {
		path, resolveErr := canonicalPath(workspace, name, true)
		if errors.Is(resolveErr, os.ErrNotExist) {
			continue
		}
		if resolveErr != nil {
			return Context{}, fmt.Errorf("resolve repository instruction %q: %w", name, resolveErr)
		}
		data, readErr := loader.Load(path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return Context{}, fmt.Errorf("read repository instruction %q: %w", path, readErr)
		}
		appendSection(PartitionRepository, string(data), path)
	}
	files := make([]resolvedFile, 0, len(options.WorkingSet))
	for _, input := range options.WorkingSet {
		path, resolveErr := canonicalPath(workspace, input.Path, input.Content == nil)
		if resolveErr != nil {
			return Context{}, fmt.Errorf("resolve active file %q: %w", input.Path, resolveErr)
		}
		files = append(files, resolvedFile{FileContext: input, canonical: path})
	}
	sort.Slice(files, func(left, right int) bool {
		return files[left].canonical < files[right].canonical
	})
	for _, file := range files {
		var data []byte
		if file.Content != nil {
			data = []byte(*file.Content)
		} else {
			data, err = loader.Load(file.canonical)
			if err != nil {
				return Context{}, fmt.Errorf("load active file %q: %w", file.canonical, err)
			}
		}
		relative, _ := filepath.Rel(workspace, file.canonical)
		text := fmt.Sprintf(
			"[active_file path=%q critical=%t]\n%s",
			filepath.ToSlash(relative),
			file.Critical,
			data,
		)
		appendSection(PartitionWorkingSet, text, file.canonical)
		result.WorkingSet = append(result.WorkingSet, file.canonical)
		if file.Critical {
			result.CriticalPaths = append(result.CriticalPaths, file.canonical)
		}
	}
	if options.MemoryEnabled && options.Memory != nil {
		block, ok, memErr := options.Memory.ComposeBlock()
		if memErr != nil {
			return Context{}, fmt.Errorf("compose user memory: %w", memErr)
		}
		if ok && block != "" {
			budget := budgets[PartitionUserMemory]
			if budget.MaxBytes <= 0 || budget.MaxBytes > memory.MaxPromptBytes {
				budget.MaxBytes = memory.MaxPromptBytes
			}
			budgets[PartitionUserMemory] = budget
			appendSection(PartitionUserMemory, block, options.Memory.Path())
		}
	}
	if strings.TrimSpace(options.Constitution) != "" {
		appendFragmentSection(
			FragmentConstitution, PartitionConstitution, options.Constitution, "session://constitution",
		)
	}
	appendSection(PartitionToolPrefix, options.ToolPrefix, "builtin://tool-prefix")
	return result, nil
}

type resolvedFile struct {
	FileContext
	canonical string
}

func retain(
	value string,
	budget Budget,
	usedBytes int,
	usedTokens uint64,
	tokens TokenCounter,
) (string, string) {
	byteLimit := len(value)
	byteLimited := false
	if budget.MaxBytes > 0 {
		remaining := max(0, budget.MaxBytes-usedBytes)
		if byteLimit > remaining {
			byteLimit = remaining
			byteLimited = true
		}
	}
	candidate := utf8Prefix(value, byteLimit)
	tokenLimited := false
	if budget.MaxTokens > 0 {
		remaining := uint64(0)
		if usedTokens < budget.MaxTokens {
			remaining = budget.MaxTokens - usedTokens
		}
		if tokens.Count(candidate) > remaining {
			candidate = tokenPrefix(candidate, remaining, tokens)
			tokenLimited = true
		}
	}
	switch {
	case byteLimited && tokenLimited:
		return candidate, "byte_and_token_budget"
	case byteLimited:
		return candidate, "byte_budget"
	case tokenLimited:
		return candidate, "token_budget"
	default:
		return candidate, ""
	}
}

func utf8Prefix(value string, limit int) string {
	if limit >= len(value) {
		return value
	}
	if limit <= 0 {
		return ""
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func tokenPrefix(value string, limit uint64, tokens TokenCounter) string {
	if limit == 0 || value == "" {
		return ""
	}
	boundaries := []int{0}
	for index := range value {
		if index != 0 {
			boundaries = append(boundaries, index)
		}
	}
	boundaries = append(boundaries, len(value))
	left, right := 0, len(boundaries)-1
	for left < right {
		middle := (left + right + 1) / 2
		if tokens.Count(value[:boundaries[middle]]) <= limit {
			left = middle
		} else {
			right = middle - 1
		}
	}
	return value[:boundaries[left]]
}

func newReceipt(
	kind, sourcePath, original, retained, reason string,
	tokens TokenCounter,
) Receipt {
	digest := sha256.Sum256([]byte(original))
	return Receipt{
		Kind: kind, SourcePath: sourcePath,
		OriginalBytes: len(original), RetainedBytes: len(retained),
		OriginalTokens: tokens.Count(original), RetainedTokens: tokens.Count(retained),
		Digest:    fmt.Sprintf("sha256:%x", digest[:]),
		Truncated: retained != original, TruncationReason: reason,
	}
}

func canonicalWorkspace(path string) (string, error) {
	if path == "" {
		path = "."
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("workspace must be a directory")
	}
	return filepath.Clean(resolved), nil
}

func canonicalPath(workspace, path string, mustExist bool) (string, error) {
	if path == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		if mustExist || !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if info, lstatErr := os.Lstat(absolute); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("cannot resolve context symlink")
		}
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(absolute))
		if parentErr != nil {
			return "", parentErr
		}
		resolved = filepath.Join(parent, filepath.Base(absolute))
	}
	resolved = filepath.Clean(resolved)
	if !within(workspace, resolved) {
		return "", errors.New("path escapes workspace")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		if !mustExist && errors.Is(err, os.ErrNotExist) {
			return resolved, nil
		}
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("context path must be a regular file")
	}
	return resolved, nil
}

func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}
