package file

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/filebroker"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	pdf "github.com/ledongthuc/pdf"
)

const (
	defaultReadLines = 200
	maxReadLines     = 2000
	// maxReadLineRunes bounds how much of one very long line (a minified
	// bundle, a data blob) enters the model-visible window. The kept prefix
	// is capped in code points so a surrogate pair is never split, and every
	// truncated line number is reported in metadata. Lines longer than the
	// old 1 MiB scanner ceiling previously failed the whole read; they now
	// stream through and truncate like any other long line.
	maxReadLineRunes = 2000
	// readLineTruncationNote is appended to each truncated line so the cut is
	// visible inline, not only in metadata.
	readLineTruncationNote = "…(line truncated)"

	defaultListLimit = 200
	maxListLimit     = 2000
)

type Tools struct {
	root      string
	workspace *sandbox.Workspace
	backend   sandbox.Backend
}

func NewWithBackend(root string, backend sandbox.Backend) (*Tools, error) {
	if backend == nil {
		return nil, errors.New("file tools require an injected sandbox backend")
	}
	backend, err := sandbox.BindPolicy(backend, sandbox.Options{WorkspaceRoot: root})
	if err != nil {
		return nil, err
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		return nil, err
	}
	return &Tools{root: workspace.Root(), workspace: workspace, backend: backend}, nil
}

func (t *Tools) Register(registry *tool.Registry) error {
	registry.SetSandboxBackend(t.backend)
	for _, kind := range []string{
		"file_read", "file_write", "file_edit", "file_apply", "file_patch", "file_list",
	} {
		executor, err := newOperation(t, kind)
		if err != nil {
			return err
		}
		if err := registry.Register(executor); err != nil {
			return err
		}
	}
	return nil
}

type operation struct {
	typed.Contract[operationInput, tool.Result]
	tools *Tools
	kind  string
}

func newOperation(tools *Tools, kind string) (*operation, error) {
	executor := &operation{tools: tools, kind: kind}
	contract, err := typed.NewResultContract(typed.ResultSpec[operationInput]{
		Name: kind, Disposition: tool.DispositionWaitForTeardown,
		Run: executor.run,
	})
	if err != nil {
		return nil, err
	}
	executor.Contract = contract
	return executor, nil
}

type operationInput struct {
	Path        string          `json:"path"`
	Content     string          `json:"content"`
	Old         string          `json:"old"`
	New         string          `json:"new"`
	Patch       string          `json:"patch"`
	Changes     []changeRequest `json:"changes"`
	DryRun      bool            `json:"dry_run"`
	StartLine   int             `json:"start_line"`
	MaxLines    int             `json:"max_lines"`
	Pages       string          `json:"pages"`
	Offset      int             `json:"offset"`
	Limit       int             `json:"limit"`
	Occurrences int             `json:"occurrences"`
}

type preparedFileMutation struct {
	plan       filebroker.Plan
	changes    []AppliedChange
	diff       string
	operations int
	dryRun     bool
	kind       string
}

func (o *operation) TrustedBinding() tool.TrustedBinding {
	binding := tool.TrustedBindingFromDescriptor(o.Descriptor())
	switch o.kind {
	case "file_read", "file_list":
		binding.Effect = tool.EffectContract{
			Mode: tool.EffectFixed, Kind: tool.EffectWorkspaceRead,
			Risk: tool.RiskLow, Reversibility: tool.Reversible,
			WorkspaceTransaction: tool.TransactionNone,
			Approval:             tool.ApprovalPolicyDefault,
		}
		binding.RecordsWorkspaceRead = o.kind == "file_read"
	case "file_write", "file_edit", "file_apply", "file_patch":
		binding.Effect = tool.EffectContract{
			Mode: tool.EffectFixed, Kind: tool.EffectWorkspaceEdit,
			Risk: tool.RiskLow, Reversibility: tool.Reversible,
			WorkspaceTransaction:   tool.TransactionBeforeImage,
			RequireReadBeforeWrite: true,
			Approval:               tool.ApprovalPolicyDefault,
		}
	}
	return binding
}

func (o *operation) IsAuthorizedFileMutation(
	_ tool.PreparedInvocation,
) bool {
	switch o.kind {
	case "file_write", "file_edit", "file_apply", "file_patch":
		return true
	default:
		return false
	}
}

func (o *operation) PlanEdit(
	ctx context.Context, raw json.RawMessage,
) (tool.EditPlan, error) {
	input, err := typed.DecodeStrict[operationInput](raw)
	if err != nil {
		return tool.EditPlan{}, err
	}
	requests, err := editRequests(o.kind, input)
	if err != nil {
		return tool.EditPlan{}, err
	}
	return o.tools.plan(ctx, requests)
}

// editRequests derives the change list one argument payload decodes into, so
// planning and proof extraction share a single decode instead of each paying
// for its own.
func editRequests(kind string, input operationInput) ([]changeRequest, error) {
	switch kind {
	case "file_write":
		return []changeRequest{{Op: opWrite, Path: input.Path, Content: input.Content}}, nil
	case "file_edit":
		return []changeRequest{{
			Op: opEdit, Path: input.Path, Old: input.Old, New: input.New,
			Occurrences: input.Occurrences,
		}}, nil
	case "file_apply":
		return input.Changes, nil
	default:
		return nil, fmt.Errorf("tool %s does not support edit plans", kind)
	}
}

func (o *operation) ExactEditProofs(
	ctx context.Context,
	raw json.RawMessage,
) ([]tool.ExactEditProof, error) {
	if o.kind != "file_edit" && o.kind != "file_apply" {
		return nil, nil
	}
	input, err := typed.DecodeStrict[operationInput](raw)
	if err != nil {
		return nil, err
	}
	requests, err := editRequests(o.kind, input)
	if err != nil {
		return nil, err
	}
	plan, err := o.tools.plan(ctx, requests)
	if err != nil {
		return nil, err
	}
	digests := make(map[string]string, len(plan.Files))
	for _, file := range plan.Files {
		path := filepath.Join(o.tools.root, filepath.FromSlash(file.Path))
		if file.BeforeExists {
			digests[filepath.Clean(path)] = file.BeforeDigest
		}
	}
	seen := make(map[string]bool)
	var proofs []tool.ExactEditProof
	for _, request := range requests {
		path, resolveErr := o.tools.resolve(request.Path, sandbox.AllowMissing)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if !seen[path] && request.Op == opEdit {
			if digest := digests[path]; digest != "" {
				proofs = append(proofs, tool.ExactEditProof{
					Path: path, Digest: digest,
				})
			}
		}
		seen[path] = true
		if request.To != "" {
			target, resolveErr := o.tools.resolve(request.To, sandbox.AllowMissing)
			if resolveErr != nil {
				return nil, resolveErr
			}
			seen[target] = true
		}
	}
	return proofs, nil
}

func (o *operation) Descriptor() tool.Descriptor {
	properties := map[string]any{
		"path": map[string]any{"type": "string", "minLength": float64(1)},
	}
	required := []string{"path"}
	description := ""
	switch o.kind {
	case "file_list":
		description = "List structured directory entries with bounded pagination. path must be workspace-relative (use \".\" for workspace root); absolute paths inside the workspace are accepted and rewritten."
		properties["path"] = map[string]any{
			"type":        "string",
			"minLength":   float64(1),
			"description": "Workspace-relative directory path, or \".\" for the workspace root",
		}
		properties["offset"] = map[string]any{"type": "integer"}
		properties["limit"] = map[string]any{"type": "integer"}
	case "file_read":
		description = "Read a bounded UTF-8 line range or extract selected PDF pages. " +
			"path is workspace-relative (absolute paths inside workspace are rewritten); " +
			"use exact paths returned by tools, never a filename inferred from a " +
			"title or topic. Prior read text is recoverable through turn_history " +
			"and result_get — check there before re-reading. Line reads report " +
			"pagination in metadata: has_more says whether lines remain, " +
			"next_start_line is the start_line of the next window, and " +
			"returned_lines is this window's line count. Continue with " +
			"start_line=next_start_line; never count returned lines yourself. " +
			"Lines longer than the public per-line rune cap are truncated in " +
			"place and listed in metadata truncated_lines — do not assume a " +
			"truncated line is complete; narrow the window instead of " +
			"re-reading the same lines."
		properties["path"] = map[string]any{
			"type":        "string",
			"minLength":   float64(1),
			"description": "Workspace-relative file path",
		}
		properties["start_line"] = map[string]any{
			"type": "integer",
			"description": "First line of the window to read (defaults to 1). " +
				"Use search hits or pagination metadata to choose a relevant window.",
		}
		properties["max_lines"] = map[string]any{
			"type":        "integer",
			"description": "Maximum number of lines to return from the window",
		}
		properties["pages"] = map[string]any{"type": "string"}
	case "file_write":
		description = "Atomically write a UTF-8 text file. path is workspace-relative. " +
			"Missing parent directories are created safely; write the intended file " +
			"directly instead of running mkdir or creating a placeholder file. " +
			"If the path already exists, call file_read for that exact path first; " +
			"new paths do not require a prior read."
		properties["content"] = map[string]any{"type": "string"}
		required = append(required, "content")
	case "file_edit":
		description = "Atomically replace exact text occurrences. path is workspace-relative. " +
			"The exact old text is a compare-and-swap precondition; call file_read first " +
			"when the current text is not already known. occurrences declares how many " +
			"times old must appear (default 1); a mismatching count fails without " +
			"changing the file and reports the actual match lines. Copy old verbatim " +
			"from file_read. On a miss, two bounded recoveries run before failure: " +
			"a numbered-output prefix on every non-empty line of old is stripped " +
			"once, then surrounding-whitespace or confusable-punctuation drift is " +
			"matched by folding and only the matched original span is replaced. " +
			"Recovery is reported in metadata (normalized_match, " +
			"line_prefixes_stripped); a remaining miss means the file truly " +
			"differs — re-read the reported window."
		properties["old"] = map[string]any{"type": "string"}
		properties["new"] = map[string]any{"type": "string"}
		properties["occurrences"] = map[string]any{
			"type":        "integer",
			"description": "How many times old must appear for the edit to apply. Default 1; set to the observed count to replace every occurrence at once.",
		}
		required = append(required, "old", "new")
	case "file_apply":
		description = "Apply a set of file changes as one transaction: write, edit, " +
			"move and delete across several workspace-relative paths. Every change is " +
			"validated first, so nothing is written unless all of them can be applied. " +
			"Write operations safely create missing parent directories, so do not " +
			"create placeholder files first. " +
			"Later changes see earlier ones, so the same file can be edited twice in " +
			"one call. Read every other existing source or destination path first " +
			"(new paths need no prior read); exact edits take old as their read " +
			"precondition and share file_edit's bounded recovery on a miss. " +
			"Set dry_run to get the unified diff without writing."
		delete(properties, "path")
		properties["changes"] = map[string]any{
			"type": "array", "minItems": float64(1),
			"maxItems": float64(maxTransactionChanges),
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"op": map[string]any{
						"type": "string",
						"enum": []any{opWrite, opEdit, opMove, opDelete},
					},
					"path": map[string]any{
						"type": "string", "minLength": float64(1),
						"description": "Workspace-relative path the change applies to",
					},
					"content": map[string]any{
						"type": "string", "description": `Full file content for op "write"`,
					},
					"old": map[string]any{
						"type": "string", "description": `Exact text to replace for op "edit"`,
					},
					"new": map[string]any{
						"type": "string", "description": `Replacement text for op "edit"`,
					},
					"to": map[string]any{
						"type":        "string",
						"description": `Destination path for op "move"; it must not exist yet`,
					},
					"occurrences": map[string]any{
						"type":        "integer",
						"description": `For op "edit": how many times old must appear; default 1, set to the observed count to replace every occurrence at once`,
					},
				},
				"required": []string{"op", "path"}, "additionalProperties": false,
			},
		}
		properties["dry_run"] = map[string]any{
			"type":        "boolean",
			"description": "Validate and return the unified diff without writing anything",
		}
		required = []string{"changes"}
	case "file_patch":
		description = "Atomically apply a standard unified diff across workspace files. " +
			"Call file_read for every existing file changed or deleted by the patch first."
		delete(properties, "path")
		properties["patch"] = map[string]any{"type": "string", "minLength": float64(1)}
		required = []string{"patch"}
	}
	capability, access := tool.CapabilityRead, tool.AccessRead
	parallel := tool.ParallelConcurrent
	repeat := tool.RepeatExecute
	requirement := tool.SandboxNone
	resolver := tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
		Kind: "file", Field: "path", Access: tool.AccessRead,
	}}}
	var aliases []tool.Alias
	switch o.kind {
	case "file_read":
		repeat = tool.RepeatReplaySameTurn
		aliases = []tool.Alias{{Name: "read_file", Hidden: true}}
	case "file_list":
		repeat = tool.RepeatReplaySameTurn
		resolver.Templates[0].Kind = "directory"
		resolver.Templates[0].Tree = true
		aliases = []tool.Alias{{Name: "list_files", Hidden: true}}
	case "file_write":
		capability, access = tool.CapabilityWrite, tool.AccessWrite
		parallel = tool.ParallelSerial
		resolver.Templates[0].Access = tool.AccessWrite
		aliases = []tool.Alias{{Name: "write_file", Hidden: true}}
	case "file_edit":
		capability, access = tool.CapabilityWrite, tool.AccessWrite
		parallel = tool.ParallelSerial
		resolver.Templates[0].Access = tool.AccessWrite
		aliases = []tool.Alias{{Name: "edit_file", Hidden: true}}
	case "file_apply":
		capability, access = tool.CapabilityWrite, tool.AccessTree
		parallel = tool.ParallelSerial
		resolver = tool.ResourceResolver{ChangesField: "changes"}
	case "file_patch":
		capability, access = tool.CapabilityWrite, tool.AccessTree
		parallel = tool.ParallelSerial
		requirement = tool.SandboxStrong
		resolver = tool.ResourceResolver{PatchField: "patch"}
		aliases = []tool.Alias{{Name: "apply_patch", Hidden: true}}
	}
	return tool.Descriptor{
		Name: o.kind, Description: description, Visibility: tool.VisibleModel,
		DiscoveryTerms: fileDiscoveryTerms(o.kind),
		IdentityKeys:   fileIdentityKeys(o.kind),
		Capability:     capability, AccessMode: access,
		ResourceResolver: resolver, Aliases: aliases,
		ParallelPolicy: parallel, RepeatPolicy: repeat,
		SandboxRequirement: requirement, Availability: tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object", "properties": properties, "required": required, "additionalProperties": false,
		},
	}
}

func fileIdentityKeys(kind string) []string {
	switch kind {
	case "file_read":
		return []string{"path", "start_line", "max_lines"}
	case "file_list", "file_write", "file_edit":
		return []string{"path"}
	default:
		return nil
	}
}

func fileDiscoveryTerms(kind string) []string {
	switch kind {
	case "file_read":
		return []string{"read file", "查看文件", "读取文件"}
	case "file_list":
		return []string{"list files", "目录", "文件列表"}
	case "file_write":
		return []string{"write file", "创建文件", "写文件"}
	case "file_edit":
		return []string{"edit file", "修改文件", "编辑文件"}
	case "file_apply":
		return []string{"multiple files", "批量修改", "移动文件", "删除文件"}
	case "file_patch":
		return []string{"apply patch", "补丁", "diff"}
	default:
		return nil
	}
}

func (o *operation) run(ctx context.Context, input operationInput) (tool.Result, error) {
	switch o.kind {
	case "file_read":
		file, err := o.tools.workspace.OpenFile(input.Path)
		if err != nil {
			return tool.Result{}, o.tools.recoverMissingPath(err, input.Path)
		}
		defer file.Close()
		if strings.EqualFold(filepath.Ext(input.Path), ".pdf") || input.Pages != "" {
			if input.StartLine != 0 || input.MaxLines != 0 {
				return tool.Result{}, errors.New("PDF pages cannot be combined with text line ranges")
			}
			return readPDF(file, input.Pages)
		}
		return readTextRange(file, input.StartLine, input.MaxLines)
	case "file_write", "file_edit", "file_apply", "file_patch":
		return tool.Result{}, errors.New("workspace mutation requires an authorized File Broker lease")
	case "file_list":
		directory, err := o.tools.workspace.OpenDirectory(input.Path)
		if err != nil {
			return tool.Result{}, o.tools.recoverMissingPath(err, input.Path)
		}
		defer directory.Close()
		return listDirectory(directory, input.Offset, input.Limit)
	default:
		return tool.Result{}, errors.New("unknown file operation")
	}
}

func (o *operation) PrepareAuthorizedFile(
	ctx context.Context,
	invocation tool.PreparedInvocation,
) (authority.FileBinding, error) {
	prepared, err := o.prepareMutation(ctx, invocation.Arguments)
	if err != nil {
		return authority.FileBinding{}, err
	}
	return authority.FileBinding{
		MutationDigest: prepared.plan.Digest,
		Value:          prepared,
		Noop:           len(prepared.changes) == 0,
	}, nil
}

func (o *operation) ExecuteAuthorizedFile(
	ctx context.Context,
	_ tool.PreparedInvocation,
	grant authority.AuthorizedFileGrant,
	manager *authority.LeaseAuthority,
	journal *workspacejournal.Manager,
) (tool.Result, tool.Outcome, error) {
	prepared, ok := grant.Plan.(preparedFileMutation)
	if !ok || prepared.plan.Digest == "" {
		return tool.Result{}, tool.Outcome{}, errors.New("authorized file plan is invalid")
	}
	if prepared.dryRun {
		result := applyResult(
			prepared.changes, prepared.diff, prepared.operations, true,
		)
		return result, tool.OutcomeFromResult(result), nil
	}
	broker, err := filebroker.New(o.tools.workspace, manager)
	if err != nil {
		return tool.Result{}, tool.Outcome{}, err
	}
	var transactionJournal filebroker.Journal
	if journal != nil {
		transactionJournal = journal
	}
	if _, err := broker.Commit(ctx, filebroker.Request{
		Lease: grant.Lease, Validation: grant.Validation,
		Plan: prepared.plan, Journal: transactionJournal,
	}); err != nil {
		return tool.Result{}, tool.Outcome{}, err
	}
	result := o.mutationResult(prepared)
	facts := tool.EnsureOutcomeFacts(&result)
	for _, change := range prepared.changes {
		facts.WorkspaceChanges = append(facts.WorkspaceChanges, tool.WorkspaceChange{
			Path: change.Path, Kind: change.Kind,
			Added: change.Added, Removed: change.Removed,
		})
	}
	return result, tool.OutcomeFromResult(result), nil
}

func (o *operation) prepareMutation(
	ctx context.Context,
	raw json.RawMessage,
) (preparedFileMutation, error) {
	input, err := typed.DecodeStrict[operationInput](raw)
	if err != nil {
		return preparedFileMutation{}, err
	}
	var requests []changeRequest
	switch o.kind {
	case "file_write":
		requests = []changeRequest{{
			Op: opWrite, Path: input.Path, Content: input.Content,
		}}
	case "file_edit":
		requests = []changeRequest{{
			Op: opEdit, Path: input.Path, Old: input.Old, New: input.New,
		}}
	case "file_apply":
		requests = input.Changes
	case "file_patch":
		transaction, changes, diff, operations, err :=
			o.tools.preparePatchTransaction(ctx, input.Patch)
		if err != nil {
			return preparedFileMutation{}, err
		}
		plan, err := transaction.brokerPlan()
		if err != nil {
			return preparedFileMutation{}, err
		}
		return preparedFileMutation{
			plan: plan, changes: changes, diff: diff,
			operations: operations, kind: o.kind,
		}, nil
	default:
		return preparedFileMutation{}, errors.New("tool is not a workspace writer")
	}
	transaction, changes, diff, err := o.tools.prepareTransaction(ctx, requests)
	if err != nil {
		return preparedFileMutation{}, err
	}
	plan, err := transaction.brokerPlan()
	if err != nil {
		return preparedFileMutation{}, err
	}
	return preparedFileMutation{
		plan: plan, changes: changes, diff: diff,
		operations: len(requests), dryRun: input.DryRun, kind: o.kind,
	}, nil
}

func (o *operation) mutationResult(prepared preparedFileMutation) tool.Result {
	if len(prepared.changes) == 0 {
		return tool.Result{
			Content: "no changes",
			Metadata: map[string]any{
				"observed_changes": 0,
			},
		}
	}
	switch prepared.kind {
	case "file_write":
		bytes := 0
		if len(prepared.plan.Entries) != 0 {
			bytes = len(prepared.plan.Entries[0].Data)
		}
		return tool.Result{
			Content: "written", Metadata: map[string]any{"bytes": bytes},
		}
	case "file_edit":
		metadata := map[string]any{"replacements": 1}
		content := "edited"
		if len(prepared.changes) > 0 {
			if prepared.changes[0].EditNormalizedMatch {
				metadata["normalized_match"] = true
				content = "edited via folded-match recovery: old text did not " +
					"match byte-for-byte (whitespace or punctuation differed); " +
					"review the diff"
			}
			if prepared.changes[0].EditPrefixStripped {
				metadata["line_prefixes_stripped"] = true
			}
		}
		return tool.Result{Content: content, Metadata: metadata}
	case "file_patch":
		return tool.Result{
			Content:  "patched",
			Metadata: map[string]any{"format": "unified"},
		}
	default:
		return applyResult(
			prepared.changes, prepared.diff, prepared.operations, false,
		)
	}
}

func (t *Tools) recoverMissingPath(err error, path string) error {
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	candidates := t.siblingCandidates(path, 12)
	requiredAction := "file_list"
	if len(candidates) != 0 {
		requiredAction = "use_existing_path"
	}
	return tool.Precondition(tool.WithRecoveryHint(err, tool.RecoveryHint{
		ErrorCategory:  "file_not_found",
		RequiredAction: requiredAction,
		Path:           path,
		RetryOriginal:  false,
		CandidatePaths: candidates,
	}))
}

func (t *Tools) siblingCandidates(path string, limit int) []string {
	parent := filepath.Dir(filepath.Clean(path))
	if parent == "" {
		parent = "."
	}
	resolved, err := t.workspace.ResolveDirectory(parent)
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return nil
	}
	requestedExtension := strings.ToLower(filepath.Ext(path))
	sort.Slice(entries, func(i, j int) bool {
		leftMatch := strings.ToLower(filepath.Ext(entries[i].Name())) ==
			requestedExtension
		rightMatch := strings.ToLower(filepath.Ext(entries[j].Name())) ==
			requestedExtension
		if leftMatch != rightMatch {
			return leftMatch
		}
		return entries[i].Name() < entries[j].Name()
	})
	candidates := make([]string, 0, min(len(entries), limit))
	for _, entry := range entries {
		candidate := entry.Name()
		if parent != "." {
			candidate = filepath.Join(parent, candidate)
		}
		candidates = append(candidates, filepath.ToSlash(candidate))
		if len(candidates) == limit {
			break
		}
	}
	return candidates
}

// readTextRange streams a bounded UTF-8 line window. Lines longer than
// maxReadLineRunes code points are truncated in place and their line numbers
// reported in metadata; the discarded tail is still checked for NUL bytes and
// UTF-8 validity so binary content keeps failing the read instead of hiding
// behind a truncation.
func readTextRange(file *os.File, startLine, maxLines int) (tool.Result, error) {
	if startLine < 0 || maxLines < 0 {
		return tool.Result{}, errors.New("start_line and max_lines must not be negative")
	}
	if startLine == 0 {
		startLine = 1
	}
	if maxLines == 0 {
		maxLines = defaultReadLines
	}
	maxLines = min(maxLines, maxReadLines)

	info, err := file.Stat()
	if err != nil {
		return tool.Result{}, err
	}
	reader := bufio.NewReaderSize(file, 64<<10)
	var lines []string
	var truncatedLines []int
	lineNumber := 0
	hasMore := false
	for {
		line, tainted, truncated, err := readTextLine(reader, maxReadLineRunes)
		if err == io.EOF && line == "" && !tainted {
			break
		}
		if err != nil && err != io.EOF {
			return tool.Result{}, fmt.Errorf("read text lines: %w", err)
		}
		lineNumber++
		if tainted {
			return tool.Result{}, errors.New("binary or non-UTF-8 file cannot be read as text")
		}
		if lineNumber < startLine {
			continue
		}
		if len(lines) == maxLines {
			hasMore = true
			break
		}
		if truncated {
			truncatedLines = append(truncatedLines, lineNumber)
			line += readLineTruncationNote
		}
		lines = append(lines, line)
		if err == io.EOF {
			break
		}
	}
	metadata := map[string]any{
		"bytes": info.Size(), "start_line": startLine, "returned_lines": len(lines),
		"max_lines": maxLines, "has_more": hasMore,
	}
	if hasMore {
		metadata["next_start_line"] = startLine + len(lines)
	}
	if len(truncatedLines) != 0 {
		metadata["truncated_lines"] = truncatedLines
		metadata["max_line_runes"] = maxReadLineRunes
	}
	return tool.Result{
		Content: strings.Join(lines, "\n"), Truncated: hasMore, Metadata: metadata,
	}, nil
}

// readTextLine consumes one line (up to and excluding '\n') from reader,
// keeping at most limit code points of it. It reports whether the line —
// including any discarded tail — contained a NUL byte or invalid UTF-8, and
// whether content was cut at the limit. A trailing '\r' is dropped to match
// bufio.Scanner's ScanLines behavior.
func readTextLine(
	reader *bufio.Reader, limit int,
) (line string, tainted, truncated bool, err error) {
	var kept []byte
	var pending []byte // partial rune carried across chunk boundaries
	runes := 0
	extraContent := false
	validator := utf8LineValidator{}
	for {
		chunk, readErr := reader.ReadSlice('\n')
		if len(chunk) > 0 {
			terminated := chunk[len(chunk)-1] == '\n'
			body := chunk
			if terminated {
				body = chunk[:len(chunk)-1]
			}
			if bytes.IndexByte(body, 0) >= 0 {
				tainted = true
			}
			if validator.feed(body, terminated || readErr == io.EOF) {
				tainted = true
			}
			stream := body
			if len(pending) > 0 {
				stream = append(pending, body...)
				pending = pending[:0]
			}
			for len(stream) > 0 {
				width := runeSequenceWidth(stream[0])
				if width < 0 {
					stream = stream[1:] // validator already flagged the line
					continue
				}
				if len(stream) < width {
					if terminated || readErr == io.EOF {
						break // invalid partial tail, validator flagged it
					}
					pending = append(pending, stream...)
					break
				}
				if runes < limit {
					kept = append(kept, stream[:width]...)
					runes++
				} else {
					extraContent = true
				}
				stream = stream[width:]
			}
		}
		if readErr == bufio.ErrBufferFull {
			continue
		}
		line = strings.TrimSuffix(string(kept), "\r")
		return line, tainted, extraContent, readErr
	}
}

// utf8LineValidator checks UTF-8 validity across chunk boundaries by carrying
// a partial trailing sequence from one chunk to the next.
type utf8LineValidator struct {
	carry  [utf8.UTFMax]byte
	length int
}

// feed validates body (continuing any carried partial sequence). final marks
// the end of the line: leftover partial bytes then mean invalid UTF-8.
func (v *utf8LineValidator) feed(body []byte, final bool) bool {
	invalid := false
	if v.length > 0 {
		need := runeSequenceWidth(v.carry[0])
		if need < 0 {
			invalid = true
			v.length = 0
		} else {
			take := min(len(body), need-v.length)
			copy(v.carry[v.length:], body[:take])
			v.length += take
			body = body[take:]
			if v.length < need {
				if final && len(body) == 0 {
					invalid = true
					v.length = 0
				}
				return invalid
			}
			if _, size := utf8.DecodeRune(v.carry[:need]); size != need {
				invalid = true
			}
			v.length = 0
		}
	}
	for len(body) > 0 {
		need := runeSequenceWidth(body[0])
		if need < 0 {
			invalid = true
			body = body[1:]
			continue
		}
		if len(body) < need {
			if final {
				invalid = true
				return invalid
			}
			v.length = copy(v.carry[:], body)
			return invalid
		}
		if _, size := utf8.DecodeRune(body[:need]); size != need {
			invalid = true
		}
		body = body[need:]
	}
	return invalid
}

// runeSequenceWidth returns the expected UTF-8 length of a sequence led by b,
// or -1 when b cannot start a valid sequence.
func runeSequenceWidth(b byte) int {
	switch {
	case b < 0x80:
		return 1
	case b >= 0xC2 && b <= 0xDF:
		return 2
	case b >= 0xE0 && b <= 0xEF:
		return 3
	case b >= 0xF0 && b <= 0xF4:
		return 4
	default:
		return -1
	}
}

func readPDF(file *os.File, pagesExpression string) (tool.Result, error) {
	info, err := file.Stat()
	if err != nil {
		return tool.Result{}, err
	}
	reader, err := pdf.NewReader(file, info.Size())
	if err != nil {
		return tool.Result{}, fmt.Errorf("open PDF: %w", err)
	}
	pages, err := parsePageRange(pagesExpression, reader.NumPage())
	if err != nil {
		return tool.Result{}, err
	}
	text := make([]string, 0, len(pages))
	for _, pageNumber := range pages {
		value, err := reader.Page(pageNumber).GetPlainText(nil)
		if err != nil {
			return tool.Result{}, fmt.Errorf("extract PDF page %d: %w", pageNumber, err)
		}
		text = append(text, strings.TrimSpace(value))
	}
	return tool.Result{
		Content: strings.Join(text, "\n\f\n"),
		Metadata: map[string]any{
			"format": "pdf", "pages": pages, "page_count": len(pages),
			"total_pages": reader.NumPage(),
		},
	}, nil
}

func parsePageRange(expression string, total int) ([]int, error) {
	if total <= 0 {
		return nil, errors.New("PDF has no pages")
	}
	if strings.TrimSpace(expression) == "" {
		pages := make([]int, total)
		for index := range pages {
			pages[index] = index + 1
		}
		return pages, nil
	}
	seen := make(map[int]bool)
	var pages []int
	for part := range strings.SplitSeq(expression, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errors.New("invalid empty PDF page range")
		}
		start, end := 0, 0
		if left, right, found := strings.Cut(part, "-"); found {
			var err error
			start, err = strconv.Atoi(strings.TrimSpace(left))
			if err != nil {
				return nil, fmt.Errorf("invalid PDF page range %q", part)
			}
			if strings.TrimSpace(right) == "" {
				end = total
			} else if end, err = strconv.Atoi(strings.TrimSpace(right)); err != nil {
				return nil, fmt.Errorf("invalid PDF page range %q", part)
			}
		} else {
			var err error
			start, err = strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid PDF page %q", part)
			}
			end = start
		}
		if start < 1 || end < start || end > total {
			return nil, fmt.Errorf("PDF page range %q is outside 1-%d", part, total)
		}
		for page := start; page <= end; page++ {
			if !seen[page] {
				seen[page] = true
				pages = append(pages, page)
			}
		}
	}
	sort.Ints(pages)
	return pages, nil
}

func listDirectory(directory *os.File, offset, limit int) (tool.Result, error) {
	if offset < 0 || limit < 0 {
		return tool.Result{}, errors.New("offset and limit must not be negative")
	}
	if limit == 0 {
		limit = defaultListLimit
	}
	limit = min(limit, maxListLimit)
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return tool.Result{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	total := len(entries)
	offset = min(offset, total)
	end := min(total, offset+limit)
	items := make([]map[string]any, 0, end-offset)
	for _, entry := range entries[offset:end] {
		info, err := entry.Info()
		if err != nil {
			return tool.Result{}, err
		}
		entryType := "file"
		switch {
		case entry.Type()&os.ModeSymlink != 0:
			entryType = "symlink"
		case entry.IsDir():
			entryType = "directory"
		}
		items = append(items, map[string]any{
			"name": entry.Name(), "path": entry.Name(),
			"type": entryType, "size": info.Size(), "mode": info.Mode().Perm().String(),
		})
	}
	hasMore := end < total
	payload := map[string]any{"entries": items, "total": total, "offset": offset, "has_more": hasMore}
	if hasMore {
		payload["next_offset"] = end
	}
	content, err := json.Marshal(payload)
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{
		Content: string(content), Truncated: hasMore,
		Metadata: map[string]any{
			"total": total, "returned": len(items), "offset": offset,
			"has_more": hasMore, "next_offset": end,
		},
	}, nil
}

func (t *Tools) resolve(name string, mode sandbox.ResolveMode) (string, error) {
	return t.workspace.Resolve(name, mode)
}

func isBinary(data []byte) bool {
	for _, value := range data {
		if value == 0 {
			return true
		}
	}
	return false
}
