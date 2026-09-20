// Package agent adapts model-visible agent tools onto subagent.AgentControl.
// Hosts submit the same lifecycle intents and only render receipts/events.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	filetool "github.com/fwtllh-png/QCode/internal/adapter/tool/file"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/handle"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type Options struct {
	Control    *subagent.AgentControl
	Manager    *subagent.Manager
	Handles    *handle.Store
	SessionID  string
	Root       string
	Gate       subagent.ToolGate
	Budget     subagent.Budget
	Runtime    subagent.RuntimeHost
	Delegation subagent.DelegationMode
	Roles      subagent.RoleCatalog
	Graph      subagent.Graph
	// OnRelease lets the host drop per-agent runtime state (thread engine, wall
	// clock watchdog) when the model closes an agent.
	OnRelease func(agentID string)
	// Files applies merge writes into the parent workspace.
	Files *filetool.Tools
	// Workspace is the parent workspace root used for baseline fingerprinting.
	Workspace string
	// Verify runs affected parent tests after a candidate has been applied.
	Verify verify.Runner
	// Sandbox runs internal merge fingerprinting under the guarded authority.
	Sandbox sandbox.Backend
}

type Tool struct {
	control   *subagent.AgentControl
	handles   *handle.Store
	sessionID string
	onRelease func(agentID string)
	files     *filetool.Tools
	workspace string
	verify    verify.Runner
	sandbox   sandbox.Backend
}

// ArtifactRef is a compact pointer to a child-produced artifact.
type ArtifactRef struct {
	Kind string `json:"kind"`
	Ref  any    `json:"ref"`
}

// Usage accounts for child token spend. At spawn time the child has not run yet,
// so Status is "pending" and the real numbers arrive with the child's Result.
type Usage struct {
	Status string `json:"status"`
}

// Verification distinguishes self-reported child claims from gate-proven evidence.
type Verification struct {
	Status   string `json:"status"`
	Evidence string `json:"evidence,omitempty"`
}

// Receipt is the structured agent run receipt returned to the parent.
type Receipt struct {
	RunID          string                  `json:"run_id"`
	AgentID        string                  `json:"agent_id"`
	ThreadID       string                  `json:"thread_id"`
	Turn           string                  `json:"turn"`
	Role           string                  `json:"role"`
	Profile        string                  `json:"profile"`
	Stance         string                  `json:"stance"`
	TaskName       string                  `json:"task_name"`
	ExpectedOutput string                  `json:"expected_output"`
	OwnedPaths     []string                `json:"owned_paths,omitempty"`
	Trigger        string                  `json:"trigger"`
	FollowUp       bool                    `json:"follow_up"`
	Takeover       bool                    `json:"takeover"`
	Artifacts      []ArtifactRef           `json:"artifacts"`
	Usage          Usage                   `json:"usage"`
	Verification   Verification            `json:"verification"`
	WorkerRecord   map[string]any          `json:"worker_record"`
	Context        subagent.ContextReceipt `json:"context"`
}

func Register(registry *tool.Registry, options Options) error {
	if registry == nil {
		return errors.New("agent tool registry is required")
	}
	if options.Handles == nil {
		return errors.New("agent handle store is required")
	}
	sessionID := strings.TrimSpace(options.SessionID)
	if sessionID == "" {
		sessionID = "session-local"
	}
	control := options.Control
	manager := options.Manager
	if control == nil && manager == nil {
		if options.Gate == nil {
			return errors.New("agent tool gate is required when manager is nil")
		}
		root := strings.TrimSpace(options.Root)
		if root == "" {
			return errors.New("agent root is required when manager is nil")
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			return err
		}
		opened, err := subagent.Open(subagent.Options{
			Root: root, Gate: options.Gate, Runtime: options.Runtime,
			Roles: options.Roles, Budget: options.Budget,
		})
		if err != nil {
			return err
		}
		manager = opened
	}
	if control == nil {
		mode := options.Delegation
		if mode == "" {
			mode = subagent.DelegationExplicit
		}
		policy, err := subagent.NewDelegationPolicy(mode)
		if err != nil {
			return err
		}
		opened, err := subagent.NewAgentControl(manager, options.Roles, policy)
		if err != nil {
			return err
		}
		control = opened
	}
	if options.Graph != nil {
		if err := control.AttachGraph(options.Graph); err != nil {
			return fmt.Errorf("attach agent graph: %w", err)
		}
	}
	workspace := strings.TrimSpace(options.Workspace)
	if workspace != "" {
		canonical, err := sandbox.NewWorkspace(workspace)
		if err != nil {
			return fmt.Errorf("agent parent workspace: %w", err)
		}
		workspace = canonical.Root()
	}
	shared := &Tool{
		control: control, handles: options.Handles, sessionID: sessionID,
		onRelease: options.OnRelease,
		files:     options.Files,
		workspace: workspace,
		verify:    options.Verify,
		sandbox:   options.Sandbox,
	}
	for _, kind := range []string{
		"spawn_agent", "send_message", "wait_agent", "list_agents",
		"followup_task", "interrupt_agent", "close_agent", "integrate_agent",
	} {
		executor, err := newOperation(shared, kind)
		if err != nil {
			return err
		}
		if err := registry.Register(executor); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tool) spawn(ctx context.Context, input operationInput) (tool.Result, error) {
	if t == nil || t.control == nil || t.handles == nil {
		return tool.Result{}, errors.New("agent tool is not configured")
	}
	role, err := subagent.ParseRole(input.Role)
	if err != nil {
		return tool.Result{}, err
	}
	parentID := strings.TrimSpace(input.ParentID)
	trigger := subagent.DelegationTrigger(strings.TrimSpace(input.Trigger))
	sessionID := t.invocationSession(ctx)
	if caller, nested := t.callerAgent(ctx); nested {
		roleSpec, roleErr := t.control.RoleSpec(caller.Role)
		if roleErr != nil {
			return tool.Result{}, roleErr
		}
		if !roleSpec.CanDelegate {
			return tool.Result{}, fmt.Errorf(
				"agent %s role %s cannot delegate", caller.ID, caller.Role,
			)
		}
		if parentID != "" && parentID != caller.ID {
			return tool.Result{}, errors.New(
				"nested spawn parent_id must match the calling agent",
			)
		}
		parentID, trigger = caller.ID, subagent.TriggerSystem
	}
	objective := strings.TrimSpace(input.Objective)
	identity := tool.InvocationIdentityFrom(ctx)
	delegated, err := t.control.Delegate(ctx, subagent.DelegationRequest{
		Intent: subagent.DelegationIntent{
			SessionID:      sessionID,
			TaskName:       strings.TrimSpace(input.TaskName),
			Role:           role,
			Objective:      objective,
			ExpectedOutput: strings.TrimSpace(input.ExpectedOutput),
			OwnedPaths:     input.OwnedPaths,
			ParentID:       parentID,
			Trigger:        trigger,
			Budget: subagent.AgentBudget{
				MaxSteps: input.MaxSteps, MaxTokens: input.MaxTokens,
				MaxCostUSD: input.MaxCostUSD,
			},
		},
		ContextMode: subagent.ContextMode(strings.TrimSpace(input.ContextMode)),
		LastTurns:   input.ContextTurns,
		Source: subagent.ContextSourceRef{
			ThreadID: identity.ThreadID,
			TurnID:   identity.TurnID,
		},
	})
	if err != nil {
		return tool.Result{}, err
	}
	child := delegated.Agent
	fork := delegated.Fork
	turn := delegated.Turn
	threadID := subagent.ThreadIDFor(child.ID)
	transcript := fmt.Sprintf(
		"agent_id=%s\nthread_id=%s\nrole=%s\nprofile=%s\nstance=%s\ndepth=%d\nworktree=%s\nparent=%s\ncontext_mode=%s\ncontext_digest=%s\nprompt=%s\nturn=%s\n",
		child.ID, threadID, child.Role, child.Profile, child.Stance, child.Depth, child.Worktree, child.Parent,
		fork.Receipt.Mode, fork.Receipt.Digest, fork.Prompt, turn,
	)
	handleName := filepath.ToSlash(filepath.Join("agent-"+child.ID, "transcript"))
	varHandle, err := t.handles.PutText(sessionID, handleName, transcript)
	if err != nil {
		return tool.Result{}, errors.Join(err, t.cleanupSpawnedChild(ctx, child.ID))
	}
	receipt := Receipt{
		RunID: child.ID, AgentID: child.ID, ThreadID: threadID, Turn: turn,
		Role: string(child.Role), Profile: child.Profile, Stance: string(child.Stance),
		TaskName: child.TaskName, ExpectedOutput: child.ExpectedOutput,
		OwnedPaths: append([]string(nil), child.OwnedPaths...),
		Trigger:    string(child.DelegationTrigger),
		FollowUp:   false, Takeover: true,
		Artifacts: []ArtifactRef{{Kind: "transcript_handle", Ref: varHandle}},
		// The child turn has only just been submitted here: claiming a
		// verification status now would be a claim about work that has not run.
		Usage:        Usage{Status: "pending"},
		Verification: Verification{Status: "pending"},
		Context:      fork.Receipt,
		WorkerRecord: map[string]any{
			"worktree": child.Worktree, "serialized": child.Serialized,
			"parent_id": child.Parent, "depth": child.Depth,
		},
	}
	receiptBody, err := json.Marshal(receipt)
	if err != nil {
		return tool.Result{}, errors.Join(err, t.cleanupSpawnedChild(ctx, child.ID))
	}
	mailboxTo := subagent.BindSessionParent(child.Parent)
	message, err := t.control.Mailbox().Enqueue(subagent.Message{
		SessionID: sessionID, From: child.ID, To: mailboxTo,
		Kind: subagent.MessageContext, Body: receiptBody,
	})
	if err != nil {
		return tool.Result{}, errors.Join(err, t.cleanupSpawnedChild(ctx, child.ID))
	}
	body := map[string]any{
		"agent_id": child.ID, "agent_path": child.Path, "revision": child.Revision,
		"thread_id": threadID, "role": string(child.Role),
		"profile": child.Profile, "stance": string(child.Stance),
		"depth": child.Depth, "worktree": child.Worktree,
		"serialized": child.Serialized,
		"parent_id":  child.Parent, "turn": turn,
		"task_name": child.TaskName, "expected_output": child.ExpectedOutput,
		"owned_paths": child.OwnedPaths, "trigger": string(child.DelegationTrigger),
		"status":       string(subagent.StatusRunning),
		"context_mode": fork.Receipt.Mode, "context_receipt": fork.Receipt,
		"receipt": map[string]any{
			"sequence": message.Sequence, "from": message.From, "to": message.To,
			"body": json.RawMessage(message.Body),
		},
		"run_receipt":       receipt,
		"transcript_handle": varHandle,
	}
	content, err := json.Marshal(body)
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{
		Content: string(content),
		Metadata: map[string]any{
			"agent_id": child.ID, "thread_id": threadID,
			"receipt_sequence":  message.Sequence,
			"transcript_handle": varHandle,
			"verification":      receipt.Verification.Status,
			"usage":             receipt.Usage.Status,
			"status":            string(subagent.StatusRunning),
			"serialized":        child.Serialized,
			"context_digest":    fork.Receipt.Digest,
			"context_tokens":    fork.Receipt.TokenEstimate,
		},
	}, nil
}

func (t *Tool) cleanupSpawnedChild(ctx context.Context, agentID string) error {
	if err := t.control.CloseContext(ctx, agentID); err != nil {
		return err
	}
	if t.onRelease != nil {
		t.onRelease(agentID)
	}
	return nil
}

func (t *Tool) callerAgent(ctx context.Context) (subagent.Agent, bool) {
	if t == nil || t.control == nil {
		return subagent.Agent{}, false
	}
	identity := tool.InvocationIdentityFrom(ctx)
	if identity.ThreadID == "" {
		return subagent.Agent{}, false
	}
	return t.control.AgentByThread(identity.ThreadID)
}

func (t *Tool) authorizeTarget(ctx context.Context, agentID string) error {
	caller, nested := t.callerAgent(ctx)
	if !nested {
		sessionID, ok := t.control.AgentSession(agentID)
		if !ok {
			return errors.New("agent not found")
		}
		if sessionID != t.invocationSession(ctx) {
			return errors.New("agent belongs to another session")
		}
		return nil
	}
	if !t.control.IsDescendant(caller.ID, agentID) {
		return fmt.Errorf(
			"agent %s may control only its descendant subtree", caller.ID,
		)
	}
	return nil
}

func (t *Tool) invocationSession(ctx context.Context) string {
	if identity := tool.InvocationIdentityFrom(ctx); identity.SessionID != "" {
		return identity.SessionID
	}
	return t.sessionID
}
