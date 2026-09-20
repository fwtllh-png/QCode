package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	filetool "github.com/fwtllh-png/QCode/internal/adapter/tool/file"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/filebroker"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

const (
	mergePreview = "preview"
	mergeApply   = "apply"
	mergeDiscard = "discard"
	mergeRetry   = "retry"
)

// mergeInput is the model-visible integrate_agent payload. Changes is
// runtime-expanded so Guard can journal and claim the exact write paths.
type mergeInput struct {
	Op            string            `json:"op"`
	AgentID       string            `json:"agent_id"`
	PreviewDigest string            `json:"preview_digest,omitempty"`
	Paths         []string          `json:"paths,omitempty"`
	Changes       []filetool.Change `json:"changes,omitempty"`
	Worktree      string            `json:"worktree,omitempty"`
}

type authorizedMerge struct {
	input    mergeInput
	prepared filetool.PreparedApply
	dryRun   bool
}

func (o *operation) IsAuthorizedFileMutation(
	invocation tool.PreparedInvocation,
) bool {
	if o == nil || o.kind != "integrate_agent" {
		return false
	}
	input, err := typed.DecodeStrict[mergeInput](invocation.Arguments)
	return err == nil &&
		strings.TrimSpace(input.Op) == mergeApply
}

func (o *operation) PlanEdit(
	ctx context.Context,
	raw json.RawMessage,
) (tool.EditPlan, error) {
	binding, err := o.PrepareAuthorizedFile(ctx, tool.PreparedInvocation{
		Arguments: raw,
	})
	if err != nil {
		return tool.EditPlan{}, err
	}
	value := binding.Value.(authorizedMerge)
	return editPlanFromPrepared(value.prepared), nil
}

func (o *operation) PrepareAuthorizedFile(
	ctx context.Context,
	invocation tool.PreparedInvocation,
) (authority.FileBinding, error) {
	if o == nil || o.tools == nil || o.kind != "integrate_agent" {
		return authority.FileBinding{}, errors.New("agent operation is not a workspace writer")
	}
	input, err := typed.DecodeStrict[mergeInput](invocation.Arguments)
	if err != nil {
		return authority.FileBinding{}, err
	}
	op, err := normalizeMergeOp(input.Op)
	if err != nil {
		return authority.FileBinding{}, err
	}
	var prepared filetool.PreparedApply
	if op == mergeDiscard {
		plan, planErr := filebroker.NewPlan(nil)
		if planErr != nil {
			return authority.FileBinding{}, planErr
		}
		prepared.Plan = plan
	} else {
		prepared, err = o.tools.files.PrepareApply(ctx, input.Changes)
		if err != nil {
			return authority.FileBinding{}, err
		}
	}
	return authority.FileBinding{
		MutationDigest: prepared.Plan.Digest,
		Value: authorizedMerge{
			input: input, prepared: prepared, dryRun: op != mergeApply,
		},
	}, nil
}

func (o *operation) ExecuteAuthorizedFile(
	ctx context.Context,
	invocation tool.PreparedInvocation,
	grant authority.AuthorizedFileGrant,
	manager *authority.LeaseAuthority,
	journal *workspacejournal.Manager,
) (tool.Result, tool.Outcome, error) {
	value, ok := grant.Plan.(authorizedMerge)
	if !ok || value.prepared.Plan.Digest == "" {
		return tool.Result{}, tool.Outcome{}, errors.New("authorized agent merge plan is invalid")
	}
	if value.dryRun {
		result, err := o.Execute(ctx, invocation.Arguments)
		return result, tool.OutcomeFromResult(result), err
	}
	result, err := o.tools.applyMergeAuthorized(
		ctx, value, grant, manager, journal,
	)
	return result, tool.OutcomeFromResult(result), err
}

func editPlanFromPrepared(prepared filetool.PreparedApply) tool.EditPlan {
	plan := tool.EditPlan{ID: prepared.Plan.Digest, Diff: prepared.Diff}
	for _, entry := range prepared.Plan.Entries {
		kind := workspacejournal.ChangeModified
		switch {
		case !entry.Before.Exists:
			kind = workspacejournal.ChangeCreated
		case !entry.After.Exists:
			kind = workspacejournal.ChangeDeleted
		}
		plan.Files = append(plan.Files, tool.EditPlanFile{
			Path: entry.Path, Kind: kind,
			BeforeExists: entry.Before.Exists, AfterExists: entry.After.Exists,
			BeforeDigest: digestOrMissing(entry.Before.Exists, entry.Before.Digest),
			AfterDigest:  digestOrMissing(entry.After.Exists, entry.After.Digest),
		})
	}
	return plan
}

func digestOrMissing(exists bool, digest string) string {
	if !exists {
		return "missing"
	}
	return digest
}

func (o *operation) ExpandArguments(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if o == nil || o.tools == nil {
		return raw, nil
	}
	if o.kind != "integrate_agent" {
		return raw, nil
	}
	return o.tools.expandMerge(ctx, raw)
}

func (t *Tool) expandMerge(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	input, err := typed.DecodeStrict[mergeInput](raw)
	if err != nil {
		return nil, err
	}
	agentID := strings.TrimSpace(input.AgentID)
	if agentID == "" {
		return nil, errors.New("agent_id is required")
	}
	op, err := normalizeMergeOp(input.Op)
	if err != nil {
		return nil, err
	}
	input.Op, input.AgentID = op, agentID
	input.PreviewDigest = strings.TrimSpace(input.PreviewDigest)
	switch op {
	case mergePreview:
		plan, planErr := t.planMerge(ctx, agentID, input.Paths)
		if planErr != nil {
			return nil, planErr
		}
		input.Paths, input.Changes = plan.paths, plan.changes
		input.Worktree = plan.agent.Worktree
	case mergeRetry:
		candidate, candidateErr := t.loadMergeCandidate(
			agentID, input.PreviewDigest, subagent.IntegrationFailed,
		)
		if candidateErr != nil {
			return nil, candidateErr
		}
		plan, planErr := t.planMerge(ctx, agentID, candidate.Paths)
		if planErr != nil {
			return nil, planErr
		}
		input.Paths, input.Changes = plan.paths, plan.changes
		input.Worktree = plan.agent.Worktree
	case mergeApply:
		candidate, candidateErr := t.loadMergeCandidate(
			agentID, input.PreviewDigest, subagent.IntegrationPreviewed,
		)
		if candidateErr != nil {
			return nil, candidateErr
		}
		plan, planErr := t.planMerge(ctx, agentID, candidate.Paths)
		if planErr != nil {
			return nil, planErr
		}
		if err := validateMergeCandidate(candidate, plan); err != nil {
			return nil, err
		}
		input.Paths, input.Changes = plan.paths, plan.changes
		input.Worktree = plan.agent.Worktree
	case mergeDiscard:
		if _, candidateErr := t.loadMergeCandidate(
			agentID, input.PreviewDigest, subagent.IntegrationPreviewed,
		); candidateErr != nil {
			return nil, candidateErr
		}
		input.Paths, input.Changes, input.Worktree = nil, nil, ""
	}
	return json.Marshal(input)
}

type mergePlan struct {
	agent     subagent.Agent
	result    subagent.Result
	paths     []string
	changes   []filetool.Change
	conflicts []string
}

func (t *Tool) planMerge(ctx context.Context, agentID string, filter []string) (mergePlan, error) {
	if err := t.authorizeTarget(ctx, agentID); err != nil {
		return mergePlan{}, err
	}
	if t.files == nil {
		return mergePlan{}, errors.New("integrate_agent requires parent file tools")
	}
	workspace := strings.TrimSpace(t.workspace)
	if workspace == "" {
		return mergePlan{}, errors.New("integrate_agent requires parent workspace")
	}
	agent, ok := t.control.Agent(agentID)
	if !ok {
		if _, hasResult := t.control.Result(agentID); hasResult {
			return mergePlan{}, fmt.Errorf("agent %s is closed; integrate before close_agent", agentID)
		}
		return mergePlan{}, fmt.Errorf("agent %s not found", agentID)
	}
	if agent.Closed || agent.Status == subagent.StatusShutdown {
		return mergePlan{}, fmt.Errorf("agent %s is closed; integrate before close_agent", agentID)
	}
	if !agent.Isolated || strings.TrimSpace(agent.Worktree) == "" {
		return mergePlan{}, fmt.Errorf("agent %s has no isolated worktree to merge", agentID)
	}
	if strings.TrimSpace(agent.BaseRev) == "" {
		return mergePlan{}, fmt.Errorf("agent %s has no base revision for conflict detection", agentID)
	}
	result, ok := t.control.IntegrationResult(agentID)
	if !ok {
		return mergePlan{}, fmt.Errorf(
			"agent %s has no successful integration result yet; wait for a completed writing turn",
			agentID,
		)
	}
	if !subagent.IsTerminal(agent.Status) {
		return mergePlan{}, fmt.Errorf(
			"agent %s is still %s; wait for the current turn before integration",
			agentID, agent.Status,
		)
	}
	paths := result.WritePaths()
	if len(filter) != 0 {
		allowed := make(map[string]struct{}, len(filter))
		for _, path := range filter {
			path = filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
			if path == "" {
				continue
			}
			allowed[path] = struct{}{}
		}
		filtered := make([]string, 0, len(paths))
		for _, path := range paths {
			if _, ok := allowed[path]; ok {
				filtered = append(filtered, path)
			}
		}
		paths = filtered
	}
	if len(paths) == 0 {
		return mergePlan{}, errors.New("nothing to merge: child wrote no matching paths")
	}
	sort.Strings(paths)
	kinds := changeKinds(result)
	changes := make([]filetool.Change, 0, len(paths))
	var conflicts []string
	for _, path := range paths {
		if err := validateMergePath(path); err != nil {
			return mergePlan{}, err
		}
		if !pathOwnedByAgent(path, agent.OwnedPaths) {
			return mergePlan{}, fmt.Errorf(
				"agent %s wrote %s outside its owned paths", agentID, path,
			)
		}
		if owner, claimed := t.control.WriteOwner(path); claimed && owner != agentID {
			conflicts = append(conflicts, fmt.Sprintf(
				"merge conflict on %s: also claimed by %s", path, owner,
			))
		}
		if err := checkBaseline(
			ctx,
			t.sandbox,
			workspace,
			agent.Worktree,
			agent.BaseRev,
			path,
		); err != nil {
			conflicts = append(conflicts, err.Error())
		}
		kind := kinds[path]
		switch kind {
		case "deleted":
			changes = append(changes, filetool.Change{Op: "delete", Path: path})
		default:
			body, err := os.ReadFile(filepath.Join(agent.Worktree, filepath.FromSlash(path)))
			if err != nil {
				if os.IsNotExist(err) && kind == "" {
					changes = append(changes, filetool.Change{Op: "delete", Path: path})
					continue
				}
				return mergePlan{}, fmt.Errorf("read child %s: %w", path, err)
			}
			changes = append(changes, filetool.Change{
				Op: "write", Path: path, Content: string(body),
			})
		}
	}
	sort.Strings(conflicts)
	return mergePlan{
		agent: agent, result: result, paths: paths,
		changes: changes, conflicts: conflicts,
	}, nil
}

func (t *Tool) merge(ctx context.Context, input mergeInput) (tool.Result, error) {
	if t.files == nil {
		return tool.Result{}, errors.New("integrate_agent requires parent file tools")
	}
	agentID := strings.TrimSpace(input.AgentID)
	if agentID == "" {
		return tool.Result{}, errors.New("agent_id is required")
	}
	op, err := normalizeMergeOp(input.Op)
	if err != nil {
		return tool.Result{}, err
	}
	switch op {
	case mergePreview, mergeRetry:
		return t.previewMerge(ctx, op, agentID, strings.TrimSpace(input.PreviewDigest), input.Paths)
	case mergeApply:
		return tool.Result{}, errors.New(
			"integrate_agent apply requires an authorized File Broker lease",
		)
	case mergeDiscard:
		return t.discardMerge(ctx, agentID, strings.TrimSpace(input.PreviewDigest))
	default:
		return tool.Result{}, fmt.Errorf("unsupported integrate_agent op %q", op)
	}
}

func (t *Tool) previewMerge(
	ctx context.Context,
	op, agentID, previousDigest string,
	paths []string,
) (tool.Result, error) {
	retryOf := ""
	if op == mergeRetry {
		previous, err := t.loadMergeCandidate(
			agentID, previousDigest, subagent.IntegrationFailed,
		)
		if err != nil {
			return tool.Result{}, err
		}
		retryOf, paths = previous.PreviewDigest, previous.Paths
	}
	plan, err := t.planMerge(ctx, agentID, paths)
	if err != nil {
		return tool.Result{}, err
	}
	attemptID, err := newMergeAttemptID()
	if err != nil {
		return tool.Result{}, err
	}
	candidate := candidateFromMergePlan(plan, attemptID, retryOf)
	candidate.PreviewDigest, err = digestMergeCandidate(candidate)
	if err != nil {
		return tool.Result{}, err
	}
	applied, diff, err := t.files.Apply(ctx, plan.changes, true)
	if err != nil {
		return tool.Result{}, err
	}
	if err := t.control.SaveIntegration(candidate); err != nil {
		return tool.Result{}, err
	}
	result := filetool.ResultFromApply(applied, diff, true)
	result.Content = fmt.Sprintf(
		"preview_digest=%s\nconflicts=%d\n%s",
		candidate.PreviewDigest, len(candidate.Conflicts), result.Content,
	)
	addIntegrationMetadata(&result, candidate)
	result.Metadata["op"] = op
	return result, nil
}

func (t *Tool) applyMergeAuthorized(
	ctx context.Context,
	authorized authorizedMerge,
	grant authority.AuthorizedFileGrant,
	manager *authority.LeaseAuthority,
	journal *workspacejournal.Manager,
) (tool.Result, error) {
	agentID := strings.TrimSpace(authorized.input.AgentID)
	previewDigest := strings.TrimSpace(authorized.input.PreviewDigest)
	candidate, err := t.loadMergeCandidate(
		agentID, previewDigest, subagent.IntegrationPreviewed,
	)
	if err != nil {
		return tool.Result{}, err
	}
	plan, err := t.planMerge(ctx, agentID, candidate.Paths)
	if err != nil {
		return tool.Result{}, err
	}
	if err := validateMergeCandidate(candidate, plan); err != nil {
		return tool.Result{}, err
	}
	current, err := t.files.PrepareApply(ctx, plan.changes)
	if err != nil {
		return tool.Result{}, err
	}
	if current.Plan.Digest != authorized.prepared.Plan.Digest {
		return tool.Result{}, errors.New("agent merge file plan is stale")
	}
	candidate.Status = subagent.IntegrationApplying
	candidate.Message = "integration apply started"
	if err := t.control.SaveIntegration(candidate); err != nil {
		return tool.Result{}, err
	}
	if err := t.control.BeginIntegration(agentID); err != nil {
		return tool.Result{}, t.failMergeCandidate(candidate, err, false)
	}
	if err := t.files.CommitApply(
		ctx, current, grant, manager, journal,
	); err != nil {
		return tool.Result{}, t.failMergeCandidate(candidate, err, true)
	}
	changedPaths := appliedPaths(current.Changes)
	verification, verifyMessage := t.verifyParent(ctx, changedPaths)
	candidate.Status = subagent.IntegrationApplied
	candidate.Verification = verification
	candidate.Receipt = &subagent.IntegrationReceipt{
		ChangedPaths: changedPaths,
		Verification: verification,
		AppliedAt:    time.Now().UTC(),
	}
	candidate.Message = verifyMessage
	if err := t.control.SaveIntegration(candidate); err != nil {
		_ = t.control.FinishIntegration(agentID, err)
		return tool.Result{}, err
	}
	if err := t.control.FinishIntegration(agentID, nil); err != nil {
		return tool.Result{}, err
	}
	result := filetool.ResultFromApply(
		current.Changes, current.Diff, false,
	)
	result.Content = fmt.Sprintf(
		"%s\npreview_digest=%s\nparent_verification=%s",
		result.Content, candidate.PreviewDigest, verification.Verify,
	)
	addIntegrationMetadata(&result, candidate)
	result.Metadata["op"] = mergeApply
	for _, change := range current.Changes {
		tool.EnsureOutcomeFacts(&result).WorkspaceChanges = append(
			tool.EnsureOutcomeFacts(&result).WorkspaceChanges,
			tool.WorkspaceChange{
				Path: change.Path, Kind: change.Kind,
				Added: change.Added, Removed: change.Removed,
			},
		)
	}
	return result, nil
}

func (t *Tool) discardMerge(
	ctx context.Context,
	agentID, previewDigest string,
) (tool.Result, error) {
	candidate, err := t.loadMergeCandidate(
		agentID, previewDigest, subagent.IntegrationPreviewed,
	)
	if err != nil {
		return tool.Result{}, err
	}
	candidate.Status = subagent.IntegrationDiscarded
	candidate.Message = "integration candidate discarded"
	if err := t.control.SaveIntegration(candidate); err != nil {
		return tool.Result{}, err
	}
	if err := t.control.CloseContext(ctx, agentID); err != nil {
		return tool.Result{}, err
	}
	if t.onRelease != nil {
		t.onRelease(agentID)
	}
	body, err := json.Marshal(map[string]any{
		"agent_id": agentID, "preview_digest": previewDigest,
		"status": string(candidate.Status), "closed": true,
	})
	if err != nil {
		return tool.Result{}, err
	}
	result := tool.Result{Content: string(body)}
	addIntegrationMetadata(&result, candidate)
	result.Metadata["op"] = mergeDiscard
	result.Metadata["closed"] = true
	_ = ctx
	return result, nil
}

func (t *Tool) failMergeCandidate(
	candidate subagent.IntegrationCandidate,
	applyErr error,
	finish bool,
) error {
	candidate.Status = subagent.IntegrationFailed
	candidate.Message = applyErr.Error()
	saveErr := t.control.SaveIntegration(candidate)
	var finishErr error
	if finish {
		finishErr = t.control.FinishIntegration(candidate.AgentID, applyErr)
	}
	return errors.Join(applyErr, saveErr, finishErr)
}

func (t *Tool) loadMergeCandidate(
	agentID, previewDigest string,
	status subagent.IntegrationStatus,
) (subagent.IntegrationCandidate, error) {
	if previewDigest == "" {
		return subagent.IntegrationCandidate{}, errors.New("preview_digest is required")
	}
	candidate, ok, err := t.control.Integration(agentID, previewDigest)
	if err != nil {
		return subagent.IntegrationCandidate{}, err
	}
	if !ok {
		return subagent.IntegrationCandidate{}, fmt.Errorf(
			"integration candidate %s not found for agent %s",
			previewDigest, agentID,
		)
	}
	if candidate.Status != status {
		return subagent.IntegrationCandidate{}, fmt.Errorf(
			"integration candidate %s is %s, want %s",
			previewDigest, candidate.Status, status,
		)
	}
	return candidate, nil
}

func (t *Tool) verifyParent(
	ctx context.Context,
	paths []string,
) (protocol.ReceiptVerification, string) {
	outcome := protocol.ReceiptUnavailable
	message := "parent verification unavailable"
	if t.verify != nil {
		receipt, err := t.verify.Verify(ctx, verify.Request{
			Scope: verify.ScopeAffected, Paths: paths,
		})
		if err != nil {
			message = "parent verification unavailable: " + err.Error()
		} else {
			outcome = protocolVerificationStatus(receipt.Status)
			message = "parent verification " + outcome
			if detail := strings.TrimSpace(receipt.Message); detail != "" {
				message += ": " + detail
			}
		}
	}
	return protocol.ReceiptVerification{
		Diagnostics: protocol.ReceiptNotEvaluated,
		Tests:       outcome,
		Verify:      outcome,
	}, message
}

func protocolVerificationStatus(status string) string {
	switch status {
	case verify.StatusPassed:
		return protocol.ReceiptPassed
	case verify.StatusFailed:
		return protocol.ReceiptFailed
	case verify.StatusUnavailable:
		return protocol.ReceiptUnavailable
	default:
		return protocol.ReceiptNotEvaluated
	}
}

func normalizeMergeOp(op string) (string, error) {
	switch normalized := strings.ToLower(strings.TrimSpace(op)); normalized {
	case "", mergePreview:
		return mergePreview, nil
	case mergeApply, mergeDiscard, mergeRetry:
		return normalized, nil
	default:
		return "", fmt.Errorf(
			"unsupported integrate_agent op %q; want preview, apply, discard, or retry",
			op,
		)
	}
}

func candidateFromMergePlan(
	plan mergePlan,
	attemptID, retryOf string,
) subagent.IntegrationCandidate {
	return subagent.IntegrationCandidate{
		AgentID: plan.agent.ID, AgentPath: plan.agent.Path,
		ParentID: plan.agent.Parent, ParentPath: plan.agent.ParentPath,
		AttemptID: attemptID, RetryOf: retryOf,
		Status:       subagent.IntegrationPreviewed,
		BaseRevision: plan.agent.BaseRev, ResultTurnID: plan.result.TurnID,
		Paths:     append([]string(nil), plan.paths...),
		Changes:   integrationChanges(plan.changes),
		Conflicts: append([]string(nil), plan.conflicts...),
		Verification: protocol.ReceiptVerification{
			Diagnostics: protocol.ReceiptNotEvaluated,
			Tests:       protocol.ReceiptNotEvaluated,
			Verify:      protocol.ReceiptNotEvaluated,
		},
	}
}

func validateMergeCandidate(
	candidate subagent.IntegrationCandidate,
	plan mergePlan,
) error {
	if len(plan.conflicts) != 0 {
		return fmt.Errorf(
			"integration preview is stale or conflicted: %s",
			strings.Join(plan.conflicts, "; "),
		)
	}
	current := candidateFromMergePlan(
		plan, candidate.AttemptID, candidate.RetryOf,
	)
	digest, err := digestMergeCandidate(current)
	if err != nil {
		return err
	}
	if digest != candidate.PreviewDigest {
		return errors.New(
			"integration preview is stale: child content, result, paths, or base revision changed",
		)
	}
	return nil
}

func digestMergeCandidate(candidate subagent.IntegrationCandidate) (string, error) {
	payload := struct {
		Version      int                          `json:"version"`
		AgentID      string                       `json:"agent_id"`
		ParentID     string                       `json:"parent_id,omitempty"`
		AttemptID    string                       `json:"attempt_id"`
		RetryOf      string                       `json:"retry_of,omitempty"`
		ResultTurnID string                       `json:"result_turn_id"`
		BaseRevision string                       `json:"base_revision"`
		Paths        []string                     `json:"paths"`
		Changes      []subagent.IntegrationChange `json:"changes"`
		Conflicts    []string                     `json:"conflicts,omitempty"`
	}{
		Version: 1, AgentID: candidate.AgentID, ParentID: candidate.ParentID,
		AttemptID: candidate.AttemptID, RetryOf: candidate.RetryOf,
		ResultTurnID: candidate.ResultTurnID, BaseRevision: candidate.BaseRevision,
		Paths: candidate.Paths, Changes: candidate.Changes,
		Conflicts: candidate.Conflicts,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func newMergeAttemptID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("create integration attempt: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func integrationChanges(changes []filetool.Change) []subagent.IntegrationChange {
	out := make([]subagent.IntegrationChange, len(changes))
	for index, change := range changes {
		out[index] = subagent.IntegrationChange{
			Op: change.Op, Path: change.Path, Content: change.Content,
		}
	}
	return out
}

func appliedPaths(changes []filetool.AppliedChange) []string {
	paths := make([]string, 0, len(changes))
	for _, change := range changes {
		if change.Path != "" {
			paths = append(paths, change.Path)
		}
	}
	sort.Strings(paths)
	return paths
}

func addIntegrationMetadata(
	result *tool.Result,
	candidate subagent.IntegrationCandidate,
) {
	if result.Metadata == nil {
		result.Metadata = map[string]any{}
	}
	result.Metadata["agent_id"] = candidate.AgentID
	result.Metadata["preview_digest"] = candidate.PreviewDigest
	result.Metadata["integration_status"] = string(candidate.Status)
	result.Metadata["paths"] = append([]string(nil), candidate.Paths...)
	result.Metadata["conflicts"] = append([]string(nil), candidate.Conflicts...)
	result.Metadata["verification"] = candidate.Verification
	if candidate.Receipt != nil {
		result.Metadata["integration_receipt"] = *candidate.Receipt
	}
}

func pathOwnedByAgent(path string, owned []string) bool {
	if len(owned) == 0 {
		return true
	}
	path = filepath.ToSlash(filepath.Clean(path))
	for _, root := range owned {
		root = filepath.ToSlash(filepath.Clean(strings.TrimSpace(root)))
		if root == "." || root == "" {
			continue
		}
		if path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/") {
			return true
		}
	}
	return false
}

func changeKinds(result subagent.Result) map[string]string {
	kinds := make(map[string]string, len(result.Diff))
	for _, change := range result.Diff {
		path := strings.TrimSpace(change.Path)
		if path == "" {
			continue
		}
		kinds[path] = change.Kind
	}
	return kinds
}

func validateMergePath(path string) error {
	clean := filepath.Clean(path)
	if clean == "." || clean == "" || filepath.IsAbs(clean) ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return fmt.Errorf("refusing to merge unsafe path %q", path)
	}
	return nil
}

// checkBaseline compares the parent workspace file to the child's spawn-time
// revision. Content equality (Exists + SHA256) is the D8 level-2 gate.
func checkBaseline(
	ctx context.Context,
	backend sandbox.Backend,
	workspace, worktree, baseRev, relPath string,
) error {
	parentPath := filepath.Join(workspace, filepath.FromSlash(relPath))
	parent, _, _, err := workspacejournal.Snapshot(parentPath)
	if err != nil {
		return fmt.Errorf("fingerprint parent %s: %w", relPath, err)
	}
	baselineExists, baselineHash, err := gitBlobHash(
		ctx,
		backend,
		workspace,
		worktree,
		baseRev,
		relPath,
	)
	if err != nil {
		return err
	}
	if parent.Exists != baselineExists {
		return fmt.Errorf(
			"merge conflict on %s: parent exists=%v but child base exists=%v",
			relPath, parent.Exists, baselineExists,
		)
	}
	if parent.Exists && parent.SHA256 != baselineHash {
		return fmt.Errorf(
			"merge conflict on %s: parent drifted from child base revision",
			relPath,
		)
	}
	return nil
}

func gitBlobHash(
	ctx context.Context,
	backend sandbox.Backend,
	workspace, worktree, baseRev, relPath string,
) (bool, string, error) {
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 30*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	revision := strings.TrimSpace(baseRev)
	directory, err := process.OpenPinnedDirectory(backend, workspace)
	if err != nil {
		return false, "", err
	}
	defer directory.Close()
	run := func(arguments ...string) (process.Result, error) {
		return process.Run(ctx, process.Options{
			Path: gitMergeExecutable(),
			Args: append([]string{"-C", worktree}, arguments...),
			Dir:  workspace, DirFile: directory,
			Sandbox: backend, RequireSandbox: true,
			WorkspaceReadOnly:   true,
			AdditionalReadPaths: []string{worktree},
		})
	}
	baseSpec := revision + "^{commit}"
	base, err := run("cat-file", "-e", baseSpec)
	if err != nil {
		return false, "", fmt.Errorf("git cat-file %s: %w", baseSpec, err)
	}
	if base.ExitCode != 0 {
		return false, "", gitCommandError("cat-file", baseSpec, base)
	}
	spec := revision + ":" + filepath.ToSlash(relPath)
	exists, err := run("cat-file", "-e", spec)
	if err != nil {
		return false, "", fmt.Errorf("git cat-file %s: %w", spec, err)
	}
	if exists.ExitCode != 0 {
		return false, "", nil
	}
	result, err := run("show", spec)
	if err != nil {
		return false, "", fmt.Errorf("git show %s: %w", spec, err)
	}
	if result.ExitCode != 0 {
		return false, "", gitCommandError("show", spec, result)
	}
	sum := sha256.Sum256([]byte(result.Stdout))
	return true, hex.EncodeToString(sum[:]), nil
}

func gitCommandError(command, spec string, result process.Result) error {
	message := strings.TrimSpace(result.Stderr)
	if message == "" {
		message = strings.TrimSpace(result.Stdout)
	}
	return fmt.Errorf("git %s %s exited with code %d: %s", command, spec, result.ExitCode, message)
}

func gitMergeExecutable() string {
	for _, candidate := range []string{
		"/Library/Developer/CommandLineTools/usr/bin/git",
		"/Applications/Xcode.app/Contents/Developer/usr/bin/git",
	} {
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}
	return "git"
}
