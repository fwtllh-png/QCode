package git

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolresult "github.com/fwtllh-png/QCode/internal/adapter/tool/result"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
)

type MutationRuntime interface {
	ReadVCS(context.Context, string, ...string) (string, error)
	AddIndex(context.Context, string, []string) error
	Commit(context.Context, string, string) error
	AmendCommit(context.Context, string, string) error
	SwitchBranch(context.Context, string, string) error
	CreateBranch(context.Context, string, string) error
	Fetch(context.Context, string, string) error
	Pull(context.Context, string, string, string) error
	Push(context.Context, string, string, string) error
	Merge(context.Context, string, string) error
	Rebase(context.Context, string, string) error
	CherryPick(context.Context, string, string) error
	Restore(context.Context, string, []string, bool) error
	StashPush(context.Context, string, string) error
	StashPop(context.Context, string) error
	Tag(context.Context, string, string, string) error
	ResolveConflict(context.Context, string, string) error
}

type mutationInput struct {
	Paths    []string `json:"paths"`
	Message  string   `json:"message"`
	Branch   string   `json:"branch"`
	Remote   string   `json:"remote"`
	Create   bool     `json:"create"`
	Revision string   `json:"revision"`
	Staged   bool     `json:"staged"`
	Action   string   `json:"action"`
	Tag      string   `json:"tag"`
}

type mutationTool struct {
	typed.Contract[mutationInput, tool.Result]
	root    string
	kind    string
	runtime MutationRuntime
}

func RegisterMutations(
	registry *tool.Registry,
	root string,
	runtime MutationRuntime,
) error {
	for _, kind := range []string{
		"git_add", "git_commit", "git_switch", "git_fetch", "git_pull", "git_push",
		"git_amend", "git_merge", "git_rebase", "git_cherry_pick",
		"git_restore", "git_stash", "git_tag",
		"git_conflict",
	} {
		executor := &mutationTool{
			root: root, kind: kind, runtime: runtime,
		}
		contract, err := typed.NewResultContract(typed.ResultSpec[mutationInput]{
			Name: kind, Disposition: tool.DispositionWaitForTeardown,
			Run: executor.run,
		})
		if err != nil {
			return err
		}
		executor.Contract = contract
		if err := registry.Register(executor); err != nil {
			return err
		}
	}
	return nil
}

func (t *mutationTool) Descriptor() tool.Descriptor {
	properties := map[string]any{}
	required := []string{}
	description := ""
	switch t.kind {
	case "git_add":
		description = "Stage exact workspace paths for the next Git commit"
		properties["paths"] = map[string]any{
			"type": "array", "minItems": 1, "maxItems": 512,
			"items": map[string]any{"type": "string", "minLength": 1},
		}
		required = []string{"paths"}
	case "git_commit":
		description = "Create a Git commit from the currently staged changes"
		properties["message"] = map[string]any{
			"type": "string", "minLength": 1, "maxLength": 4096,
		}
		required = []string{"message"}
	case "git_amend":
		description = "Replace the current Git commit with the staged changes and a new message"
		properties["message"] = map[string]any{
			"type": "string", "minLength": 1, "maxLength": 4096,
		}
		required = []string{"message"}
	case "git_switch":
		description = "Switch to an existing local Git branch, or create and switch to a new branch"
		properties["branch"] = map[string]any{
			"type": "string", "minLength": 1, "maxLength": 255,
		}
		properties["create"] = map[string]any{"type": "boolean"}
		required = []string{"branch"}
	case "git_fetch":
		description = "Fetch and prune refs from a named Git remote"
		properties["remote"] = remoteSchema()
		required = []string{"remote"}
	case "git_pull":
		description = "Fast-forward the current branch from a named Git remote branch"
		properties["remote"] = remoteSchema()
		properties["branch"] = branchSchema()
		required = []string{"remote", "branch"}
	case "git_push":
		description = "Push HEAD to a named branch on a named Git remote"
		properties["remote"] = remoteSchema()
		properties["branch"] = branchSchema()
		required = []string{"remote", "branch"}
	case "git_merge":
		description = "Merge one validated branch or revision into the current branch"
		properties["revision"] = revisionSchema()
		required = []string{"revision"}
	case "git_rebase":
		description = "Rebase the current branch onto one validated branch or revision"
		properties["revision"] = revisionSchema()
		required = []string{"revision"}
	case "git_cherry_pick":
		description = "Apply one validated commit onto the current branch"
		properties["revision"] = revisionSchema()
		required = []string{"revision"}
	case "git_restore":
		description = "Restore exact workspace paths from the index, or unstage them when staged is true"
		properties["paths"] = pathsSchema()
		properties["staged"] = map[string]any{"type": "boolean"}
		required = []string{"paths"}
	case "git_stash":
		description = "Push workspace changes to the stash or pop the latest stash"
		properties["action"] = map[string]any{
			"type": "string", "enum": []any{"push", "pop"},
		}
		properties["message"] = map[string]any{
			"type": "string", "maxLength": 4096,
		}
		required = []string{"action"}
	case "git_tag":
		description = "Create a validated lightweight or annotated Git tag"
		properties["tag"] = branchSchema()
		properties["message"] = map[string]any{
			"type": "string", "maxLength": 4096,
		}
		required = []string{"tag"}
	case "git_conflict":
		description = "Continue or abort an in-progress Git merge, rebase, or cherry-pick"
		properties["action"] = map[string]any{
			"type": "string",
			"enum": []any{
				"merge_abort", "rebase_abort", "rebase_continue",
				"cherry_pick_abort", "cherry_pick_continue",
			},
		}
		required = []string{"action"}
	}
	availability, unavailable := tool.AvailabilityAvailable, ""
	if t.runtime == nil {
		availability, unavailable = tool.AvailabilityUnavailable,
			"managed VCS mutation runtime is unavailable"
	}
	resources := []tool.ResourceTemplate{{
		Kind: "vcs", ID: ".", Access: tool.AccessWrite,
	}}
	switch t.kind {
	case "git_switch":
		resources = append(resources, tool.ResourceTemplate{
			Kind: "vcs_branch", Field: "branch", Access: tool.AccessWrite,
		})
	case "git_fetch":
		resources = append(resources, tool.ResourceTemplate{
			Kind: "vcs_remote", Field: "remote", Access: tool.AccessRead,
		})
	case "git_pull", "git_push":
		resources = append(resources,
			tool.ResourceTemplate{
				Kind: "vcs_remote", Field: "remote", Access: tool.AccessWrite,
			},
			tool.ResourceTemplate{
				Kind: "vcs_branch", Field: "branch", Access: tool.AccessWrite,
			},
		)
	}
	return tool.Descriptor{
		Name: t.kind, Description: description, Visibility: tool.VisibleModel,
		DiscoveryTerms: gitMutationDiscoveryTerms(t.kind),
		Capability:     t.capability(), AccessMode: tool.AccessWrite,
		ResourceResolver:   tool.ResourceResolver{Templates: resources},
		ParallelPolicy:     tool.ParallelSerial,
		RepeatPolicy:       tool.RepeatExecute,
		SandboxRequirement: tool.SandboxNone,
		Availability:       availability, UnavailableReason: unavailable,
		InputSchema: map[string]any{
			"type": "object", "properties": properties,
			"required": required, "additionalProperties": false,
		},
	}
}

func gitMutationDiscoveryTerms(kind string) []string {
	switch kind {
	case "git_add":
		return []string{"git add", "stage", "暂存", "提交"}
	case "git_commit":
		return []string{"git commit", "commit", "提交"}
	case "git_amend":
		return []string{"git amend", "amend commit", "修改提交", "追加提交"}
	case "git_switch":
		return []string{"git switch", "checkout", "切换分支", "创建分支"}
	case "git_fetch":
		return []string{"git fetch", "fetch", "拉取引用", "同步远端"}
	case "git_pull":
		return []string{"git pull", "pull", "拉取代码", "同步远端"}
	case "git_push":
		return []string{"git push", "push", "推送", "提交代码"}
	case "git_merge":
		return []string{"git merge", "merge branch", "合并分支"}
	case "git_rebase":
		return []string{"git rebase", "rebase", "变基"}
	case "git_cherry_pick":
		return []string{"cherry-pick", "cherry pick", "挑选提交"}
	case "git_restore":
		return []string{"git restore", "restore file", "恢复文件", "取消暂存"}
	case "git_stash":
		return []string{"git stash", "stash", "暂存工作区", "储藏"}
	case "git_tag":
		return []string{"git tag", "tag release", "标签", "打标签"}
	case "git_conflict":
		return []string{"git conflict", "continue rebase", "abort merge", "解决冲突", "继续变基", "中止合并"}
	default:
		return nil
	}
}

func (t *mutationTool) TrustedBinding() tool.TrustedBinding {
	binding := tool.TrustedBindingFromDescriptor(t.Descriptor())
	kind, risk, reversibility := tool.EffectProcessMutating, tool.RiskMedium, tool.Bounded
	approval := tool.ApprovalPolicyDefault
	switch t.kind {
	case "git_fetch":
		kind = tool.EffectNetworkRead
	case "git_pull", "git_merge", "git_rebase", "git_cherry_pick",
		"git_amend", "git_restore", "git_stash", "git_conflict":
		kind, risk, approval = tool.EffectNetworkMutating, tool.RiskHigh, tool.ApprovalPolicyOnce
		if t.kind != "git_pull" {
			kind = tool.EffectProcessMutating
		}
	case "git_push":
		kind, risk, reversibility, approval =
			tool.EffectExternalMutation, tool.RiskHigh, tool.Irreversible, tool.ApprovalPolicyOnce
	}
	binding.Effect = tool.EffectContract{
		Mode: tool.EffectFixed, Kind: kind, Risk: risk,
		Reversibility: reversibility, WorkspaceTransaction: tool.TransactionNone,
		Approval: approval,
	}
	if t.kind == "git_push" {
		binding.Effect.Planning = tool.PlanningExempt
	}
	return binding
}

func (t *mutationTool) capability() tool.Capability {
	switch t.kind {
	case "git_fetch", "git_pull", "git_push":
		return tool.CapabilityExternal
	default:
		return tool.CapabilityWrite
	}
}

func (t *mutationTool) run(
	ctx context.Context,
	input mutationInput,
) (tool.Result, error) {
	if t.runtime == nil {
		return toolresult.Unavailable("managed VCS mutation runtime is unavailable"), nil
	}
	before := ""
	if t.kind == "git_switch" || t.kind == "git_pull" ||
		t.kind == "git_merge" || t.kind == "git_rebase" ||
		t.kind == "git_cherry_pick" || t.kind == "git_conflict" {
		before, _ = t.runtime.ReadVCS(ctx, t.root, "rev-parse", "--verify", "HEAD")
		before = strings.TrimSpace(before)
	}
	var err error
	switch t.kind {
	case "git_add":
		err = validatePaths(input.Paths)
		if err == nil {
			paths := make([]string, len(input.Paths))
			for index, path := range input.Paths {
				paths[index] = ":(literal)" + path
			}
			err = t.runtime.AddIndex(ctx, t.root, paths)
		}
	case "git_commit":
		if strings.TrimSpace(input.Message) == "" {
			err = errors.New("commit message must not be empty")
		} else {
			err = t.runtime.Commit(ctx, t.root, input.Message)
		}
	case "git_amend":
		if strings.TrimSpace(input.Message) == "" {
			err = errors.New("commit message must not be empty")
		} else {
			err = t.runtime.AmendCommit(ctx, t.root, input.Message)
		}
	case "git_switch":
		if input.Create {
			err = t.runtime.CreateBranch(ctx, t.root, input.Branch)
		} else {
			err = t.runtime.SwitchBranch(ctx, t.root, input.Branch)
		}
	case "git_fetch":
		err = t.runtime.Fetch(ctx, t.root, input.Remote)
	case "git_pull":
		err = t.runtime.Pull(ctx, t.root, input.Remote, input.Branch)
	case "git_push":
		err = t.runtime.Push(ctx, t.root, input.Remote, input.Branch)
	case "git_merge":
		err = validateRevision(input.Revision)
		if err == nil {
			err = t.runtime.Merge(ctx, t.root, input.Revision)
		}
	case "git_rebase":
		err = validateRevision(input.Revision)
		if err == nil {
			err = t.runtime.Rebase(ctx, t.root, input.Revision)
		}
	case "git_cherry_pick":
		err = validateRevision(input.Revision)
		if err == nil {
			err = t.runtime.CherryPick(ctx, t.root, input.Revision)
		}
	case "git_restore":
		err = validatePaths(input.Paths)
		if err == nil {
			err = t.runtime.Restore(ctx, t.root, input.Paths, input.Staged)
		}
	case "git_stash":
		switch input.Action {
		case "push":
			err = t.runtime.StashPush(ctx, t.root, strings.TrimSpace(input.Message))
		case "pop":
			if strings.TrimSpace(input.Message) != "" {
				err = errors.New("stash pop does not accept a message")
			} else {
				err = t.runtime.StashPop(ctx, t.root)
			}
		default:
			err = errors.New(`stash action must be "push" or "pop"`)
		}
	case "git_tag":
		err = validateTag(input.Tag)
		if err == nil {
			err = t.runtime.Tag(ctx, t.root, input.Tag, strings.TrimSpace(input.Message))
		}
	case "git_conflict":
		err = t.runtime.ResolveConflict(ctx, t.root, input.Action)
	default:
		err = errors.New("unsupported Git mutation")
	}
	if err != nil {
		return tool.Result{}, tool.WithRecoveryHint(err, tool.RecoveryHint{
			ErrorCategory:  "git_operation_failed",
			RequiredAction: "git_status",
			RetryOriginal:  false,
		})
	}
	receipt := map[string]any{"schema_version": 1, "operation": t.kind}
	switch t.kind {
	case "git_add":
		receipt["paths"] = input.Paths
	case "git_commit":
		head, readErr := t.runtime.ReadVCS(ctx, t.root, "rev-parse", "HEAD")
		if readErr != nil {
			return tool.Result{}, readErr
		}
		receipt["revision"] = strings.TrimSpace(head)
	case "git_amend":
		head, readErr := t.runtime.ReadVCS(ctx, t.root, "rev-parse", "HEAD")
		if readErr != nil {
			return tool.Result{}, readErr
		}
		receipt["revision"] = strings.TrimSpace(head)
	case "git_switch":
		receipt["branch"], receipt["created"] = input.Branch, input.Create
	case "git_fetch":
		receipt["remote"] = input.Remote
	case "git_pull", "git_push":
		receipt["remote"], receipt["branch"] = input.Remote, input.Branch
	case "git_merge", "git_rebase", "git_cherry_pick":
		receipt["revision"] = input.Revision
	case "git_restore":
		receipt["paths"], receipt["staged"] = input.Paths, input.Staged
	case "git_stash":
		receipt["action"] = input.Action
	case "git_tag":
		receipt["tag"] = input.Tag
	case "git_conflict":
		receipt["action"] = input.Action
	}
	result, encodeErr := toolresult.Success(receipt, nil)
	if encodeErr != nil {
		return tool.Result{}, encodeErr
	}
	if before != "" {
		after, _ := t.runtime.ReadVCS(ctx, t.root, "rev-parse", "--verify", "HEAD")
		after = strings.TrimSpace(after)
		if after != "" && after != before {
			changes, changeErr := t.changedBetween(ctx, before, after)
			if changeErr != nil {
				return tool.Result{}, changeErr
			}
			tool.EnsureOutcomeFacts(&result).WorkspaceChanges = changes
			result.Metadata = map[string]any{
				"head_before": before,
				"head_after":  after,
			}
		}
	}
	return result, nil
}

func pathsSchema() map[string]any {
	return map[string]any{
		"type": "array", "minItems": 1, "maxItems": 512,
		"items": map[string]any{"type": "string", "minLength": 1},
	}
}

func revisionSchema() map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "maxLength": 512}
}

func validateRevision(revision string) error {
	revision = strings.TrimSpace(revision)
	if revision == "" || strings.HasPrefix(revision, "-") ||
		strings.ContainsAny(revision, "\x00\r\n") {
		return errors.New("invalid Git revision")
	}
	return nil
}

func validateTag(tag string) error {
	if err := validateRevision(tag); err != nil ||
		strings.ContainsAny(tag, "~^:?*[\\") {
		return errors.New("invalid Git tag")
	}
	return nil
}

func (t *mutationTool) changedBetween(
	ctx context.Context,
	before, after string,
) ([]tool.WorkspaceChange, error) {
	output, err := t.runtime.ReadVCS(
		ctx, t.root, "diff", "--name-status", "--no-renames", before, after,
	)
	if err != nil {
		return nil, fmt.Errorf("inspect Git workspace changes: %w", err)
	}
	var changes []tool.WorkspaceChange
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		status, path, found := strings.Cut(line, "\t")
		if !found || path == "" {
			return nil, errors.New("Git returned an invalid changed-path record")
		}
		kind := tool.WorkspaceModified
		switch status {
		case "A":
			kind = tool.WorkspaceCreated
		case "D":
			kind = tool.WorkspaceDeleted
		}
		changes = append(changes, tool.WorkspaceChange{
			Path: path, Kind: kind,
			Summary: fmt.Sprintf("Git HEAD changed from %s to %s", before, after),
		})
	}
	return changes, nil
}

func validatePaths(paths []string) error {
	if len(paths) == 0 {
		return errors.New("at least one Git path is required")
	}
	for _, path := range paths {
		if path == "" || filepath.IsAbs(path) {
			return errors.New("Git paths must be non-empty and workspace-relative")
		}
		clean := filepath.Clean(path)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) ||
			strings.ContainsAny(path, "\x00\r\n") {
			return errors.New("Git path is outside the workspace")
		}
	}
	return nil
}

func remoteSchema() map[string]any {
	return map[string]any{
		"type": "string", "minLength": 1, "maxLength": 255,
		"pattern": "^[A-Za-z0-9][A-Za-z0-9._-]*$",
	}
}

func branchSchema() map[string]any {
	return map[string]any{
		"type": "string", "minLength": 1, "maxLength": 255,
		"pattern": "^[A-Za-z0-9][A-Za-z0-9._/-]*$",
	}
}
