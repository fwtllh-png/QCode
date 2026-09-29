package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/platform/workspacequery"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

type GitRequest struct {
	SessionID       string   `json:"session_id,omitempty"`
	Action          string   `json:"action"`
	Branch          string   `json:"branch"`
	Revision        string   `json:"revision"`
	Message         string   `json:"message,omitempty"`
	Paths           []string `json:"paths,omitempty"`
	IncludeUnstaged bool     `json:"include_unstaged,omitempty"`
	Remote          string   `json:"remote,omitempty"`
	NewBranch       string   `json:"new_branch,omitempty"`
}

type GitResult struct {
	Action     string            `json:"action"`
	CommitHash string            `json:"commit_hash,omitempty"`
	Completed  []string          `json:"completed"`
	Problem    *protocol.Problem `json:"problem,omitempty"`
}

// GitControl is wired with the same tools and policy sources as the Agent.
// An explicit UI command is one-shot consent for its exact deterministic calls.
type GitControl struct {
	Workspace *workspacequery.Service
	NewGuard  func(context.Context, protocol.SessionProfile) (*guard.Guard, error)
}

type gitCall struct {
	name string
	args map[string]any
}

func (r *Runtime) ExecuteGit(ctx context.Context, request GitRequest) (GitResult, error) {
	return r.executeGit(ctx, request, "")
}

func (r *Runtime) SwitchGitBranch(ctx context.Context, target string) (workspacequery.GitState, error) {
	if strings.TrimSpace(target) == "" {
		return workspacequery.GitState{}, runtimeProblem(protocol.CodeInvalidArgument, "Git branch is required", nil)
	}
	result, err := r.executeGit(ctx, GitRequest{}, target)
	if err != nil {
		return workspacequery.GitState{}, err
	}
	if result.Problem != nil {
		return workspacequery.GitState{}, result.Problem
	}
	return r.opts.GitControl.Workspace.GitState(ctx)
}

func (r *Runtime) executeGit(ctx context.Context, request GitRequest, switchTarget string) (GitResult, error) {
	result := GitResult{Action: request.Action, Completed: []string{}}
	control := r.opts.GitControl
	if control == nil || control.Workspace == nil || control.NewGuard == nil {
		return result, runtimeProblem(protocol.CodeUnavailable, "managed Git execution is unavailable", nil)
	}
	release, err := r.beginWorkspaceOperation()
	if err != nil {
		return result, err
	}
	defer release()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	defer stop()
	defer cancel()

	profile := r.defaultProfile
	if request.SessionID != "" {
		session, err := r.SessionStatus(ctx, request.SessionID)
		if err != nil {
			return result, err
		}
		if session.Isolation != "shared" || session.Archived {
			return result, runtimeProblem(protocol.CodeConflict, "Git actions require an open shared Session", nil)
		}
		if session.Status != "idle" && session.Status != "completed" && session.Status != "failed" {
			return result, sessionBusyProblem("resume or resolve the Session before changing Git state", session)
		}
		snapshot, err := r.SessionProfile(ctx, request.SessionID)
		if err != nil {
			return result, err
		}
		profile = snapshot.Profile
	}
	overview, err := control.Workspace.GitOverview(ctx)
	if err != nil {
		return result, err
	}
	if switchTarget != "" {
		request = GitRequest{
			Action: "switch_branch", Branch: overview.Branch,
			Revision: overview.Revision, NewBranch: switchTarget,
		}
		result.Action = request.Action
	}
	if !overview.Repository || !overview.Root || overview.Detached {
		return result, runtimeProblem(protocol.CodeConflict, "select a branch at the Git repository root before changing Git state", nil)
	}
	if request.Revision == "" || request.Revision != overview.Revision || request.Branch != overview.Branch {
		return result, runtimeProblem(protocol.CodeConflict, "Git state changed; refresh and review the operation again", nil)
	}
	calls, err := control.calls(ctx, request, overview)
	if err != nil {
		return result, err
	}
	executor, err := control.NewGuard(ctx, profile)
	if err != nil {
		return result, err
	}
	configRevision, err := control.Workspace.GitConfigRevision(ctx)
	if err != nil {
		return result, err
	}
	id := "git_" + rand.Text()
	for index, call := range calls {
		callID := id + "/" + call.name
		arguments, err := json.Marshal(call.args)
		if err != nil {
			return result, err
		}
		executor.SetApprovalHandler(func(_ context.Context, approval guard.ApprovalRequest) error {
			var actual map[string]any
			var expected map[string]any
			if json.Unmarshal(approval.Arguments, &actual) != nil ||
				json.Unmarshal(arguments, &expected) != nil ||
				approval.CallID != callID || approval.Tool != call.name ||
				approval.AdditionalPermission != nil || approval.Network != nil ||
				!reflect.DeepEqual(actual, expected) {
				return runtimeProblem(protocol.CodeConflict, "approval does not match the explicit Git command", nil)
			}
			return executor.Decide(guard.ApprovalDecision{
				RequestID: approval.RequestID, CallID: callID,
				Approved: true, Scope: policy.ApprovalOnce,
			})
		})
		revision, head, inspectErr := control.Workspace.GitRevision(ctx)
		currentConfig, configErr := control.Workspace.GitConfigRevision(ctx)
		branch, branchErr := control.Workspace.GitCurrentBranch(ctx)
		expectedHead := overview.Head
		if result.CommitHash != "" {
			expectedHead = result.CommitHash
		}
		if inspectErr != nil || configErr != nil || branchErr != nil || currentConfig != configRevision ||
			branch != request.Branch ||
			head != expectedHead || index == 0 && revision != request.Revision {
			result.Problem = runtimeProblem(protocol.CodeConflict, "Git state changed before execution", errors.Join(inspectErr, configErr, branchErr))
			return result, nil
		}
		if call.name == "git_commit" {
			staged, stageErr := control.Workspace.GitStagedPaths(ctx)
			paths := slices.Clone(request.Paths)
			slices.Sort(staged)
			slices.Sort(paths)
			if stageErr != nil || !slices.Equal(staged, paths) {
				result.Problem = runtimeProblem(protocol.CodeConflict, "staged files changed before commit", stageErr)
				return result, nil
			}
		}
		value, runErr := executor.Execute(ctx, callID, call.name, arguments)
		if runErr != nil || value.IsError {
			result.Problem = runtimeProblem(protocol.CodeConflict, call.name+" failed; refresh Git state before retrying", runErr)
			var denied *policy.DecisionError
			if errors.As(runErr, &denied) {
				result.Problem = protocol.NewProblemWithDetails(protocol.CodeConflict, denied.Reason, false,
					protocol.ProblemDetails{Reason: denied.Code}, runErr)
			}
			return result, nil
		}
		result.Completed = append(result.Completed, call.name)
		if call.name == "git_commit" {
			var receipt struct {
				Revision string `json:"revision"`
			}
			if err := json.Unmarshal([]byte(value.Content), &receipt); err != nil || receipt.Revision == "" {
				result.Problem = runtimeProblem(protocol.CodeInternal, "commit succeeded but its revision receipt is unavailable; inspect HEAD before retrying", err)
				return result, nil
			}
			result.CommitHash = receipt.Revision
		}
	}
	return result, nil
}

func (c *GitControl) calls(ctx context.Context, request GitRequest, state workspacequery.GitOverview) ([]gitCall, error) {
	invalid := func(message string) ([]gitCall, error) {
		return nil, runtimeProblem(protocol.CodeInvalidArgument, message, nil)
	}
	if !slices.Contains([]string{"commit", "commit_push", "push", "create_branch", "switch_branch"}, request.Action) {
		return invalid("unsupported Git action")
	}
	if request.Action == "create_branch" || request.Action == "switch_branch" {
		if strings.TrimSpace(request.NewBranch) == "" {
			return invalid("new branch name is required")
		}
		if request.Action == "switch_branch" && !slices.Contains(state.Branches, request.NewBranch) {
			return invalid("branch does not exist locally")
		}
		return []gitCall{{"git_switch", map[string]any{
			"branch": request.NewBranch, "create": request.Action == "create_branch",
		}}}, nil
	}
	var calls []gitCall
	if request.Action == "commit" || request.Action == "commit_push" {
		if strings.TrimSpace(request.Message) == "" || len(request.Paths) == 0 {
			return invalid("a commit message and exact file paths are required")
		}
		files := make(map[string]workspacequery.GitChange, len(state.Files))
		for _, file := range state.Files {
			if file.Conflict {
				return invalid("resolve Git conflicts before committing")
			}
			files[file.Path] = file
		}
		selected := make(map[string]bool, len(request.Paths))
		for _, path := range request.Paths {
			file, exists := files[path]
			if !exists || selected[path] || (!request.IncludeUnstaged && file.Staged == nil) {
				return invalid("commit paths must exactly identify visible Git changes")
			}
			selected[path] = true
		}
		staged, err := c.Workspace.GitStagedPaths(ctx)
		if err != nil {
			return nil, err
		}
		for _, path := range staged {
			if !selected[path] {
				return invalid("the index contains additional files; review all staged changes")
			}
		}
		if !request.IncludeUnstaged && len(staged) != len(selected) {
			return invalid("the staged file selection changed")
		}
		if request.IncludeUnstaged {
			calls = append(calls, gitCall{"git_add", map[string]any{"paths": request.Paths}})
		}
		calls = append(calls, gitCall{"git_commit", map[string]any{"message": request.Message}})
	}
	if request.Action == "push" || request.Action == "commit_push" {
		if !slices.Contains(state.Remotes, request.Remote) {
			return invalid("select a configured Git remote")
		}
		calls = append(calls, gitCall{"git_push", map[string]any{"remote": request.Remote, "branch": request.Branch}})
	}
	return calls, nil
}

func (r *Runtime) beginWorkspaceOperation() (func(), error) {
	unlock := r.SessionService.lockMutations()
	defer unlock()
	return r.OperationService.beginWorkspaceOperation()
}
