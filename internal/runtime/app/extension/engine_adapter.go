package extension

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	sessionhistory "github.com/fwtllh-png/QCode/internal/persist/history"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	executionreceipt "github.com/fwtllh-png/QCode/internal/observability/receipt"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

var ErrOperationUnsupported = protocol.NewProblem(
	protocol.CodeConflict, "operation is not supported by this engine", false, nil,
)

type NoopEngine struct{}

func (NoopEngine) StartTurn(
	_ context.Context, payload *protocol.StartTurnPayload, sink EngineSink,
) error {
	if err := sink.Emit(&protocol.TurnStartedData{
		Provider: "local", Model: "noop", QueueID: payload.QueueID,
	}); err != nil {
		return err
	}
	return sink.Emit(&protocol.TurnCompletedData{})
}
func (NoopEngine) CancelTurn(context.Context, *protocol.CancelTurnPayload, EngineSink) error {
	return nil
}
func (NoopEngine) SteerTurn(context.Context, *protocol.SteerTurnPayload, EngineSink) error {
	return ErrOperationUnsupported
}
func (NoopEngine) DecideApproval(context.Context, *protocol.ApprovalDecisionPayload, EngineSink) error {
	return ErrOperationUnsupported
}
func (NoopEngine) ReplyInput(context.Context, *protocol.InputReplyPayload, EngineSink) error {
	return ErrOperationUnsupported
}
func (NoopEngine) CompactThread(context.Context, *protocol.CompactThreadPayload, EngineSink) error {
	return ErrOperationUnsupported
}
func (NoopEngine) ForkThread(context.Context, *protocol.ForkThreadPayload, EngineSink) error {
	return ErrOperationUnsupported
}
func (NoopEngine) RevertTurn(context.Context, *protocol.RevertTurnPayload, EngineSink) error {
	return ErrOperationUnsupported
}

type EngineAdapter struct {
	engine            *agentengine.Engine
	workspaceIdentity protocol.WorkspaceIdentity
	approvalSource    *protocol.ApprovalSource
}

func (a *EngineAdapter) Underlying() *agentengine.Engine {
	if a == nil {
		return nil
	}
	return a.engine
}

func (a *EngineAdapter) WorkspaceIdentity() protocol.WorkspaceIdentity {
	if a == nil {
		return protocol.WorkspaceIdentity{}
	}
	return a.workspaceIdentity
}

func (a *EngineAdapter) SetApprovalSource(source protocol.ApprovalSource) {
	if a == nil {
		return
	}
	copy := source
	a.approvalSource = &copy
}
func (a *EngineAdapter) TurnPhase(turnID protocol.TurnID) (TurnPhase, bool) {
	if a == nil || a.engine == nil {
		return PhaseIdle, false
	}
	phase, ok := a.engine.TurnKernelPhase(string(turnID))
	if !ok {
		return PhaseIdle, false
	}
	switch string(phase) {
	case string(PhaseAwaitingApproval):
		return PhaseAwaitingApproval, true
	case string(PhaseAwaitingInput):
		return PhaseAwaitingInput, true
	default:
		return PhaseRunning, true
	}
}
func (a *EngineAdapter) History() []provider.Message {
	if a == nil || a.engine == nil {
		return nil
	}
	return a.engine.History()
}
func (a *EngineAdapter) RestoreSessionDelta(raw json.RawMessage) error {
	if a == nil || a.engine == nil {
		return errors.New("engine is unavailable")
	}
	return a.engine.RestoreSessionDelta(raw)
}
func (a *EngineAdapter) RestorePendingApproval(
	pending PendingApproval,
) error {
	if a == nil || a.engine == nil {
		return errors.New("engine is unavailable")
	}
	encoded, err := json.Marshal(pending.Data)
	if err != nil {
		return err
	}
	var request toolguard.ApprovalRequest
	if err := json.Unmarshal(encoded, &request); err != nil {
		return err
	}
	return a.engine.RestoreApprovalRequest(request)
}
func (a *EngineAdapter) RestorePendingInput(pending PendingInput) error {
	if a == nil || a.engine == nil {
		return errors.New("engine is unavailable")
	}
	return a.engine.RestoreInputRequest(interact.Request{
		RequestID: pending.Data.RequestID,
		CallID:    pending.Data.CallID,
		Tool:      pending.Data.Tool,
		Prompt:    pending.Data.Prompt,
		Options:   append([]string(nil), pending.Data.Options...),
		ExpiresAt: pending.Data.ExpiresAt,
	})
}
func (a *EngineAdapter) ValidateSessionProfile(
	profile protocol.SessionProfile,
) error {
	if a == nil || a.engine == nil {
		return errors.New("engine is unavailable")
	}
	return a.engine.ValidateSessionProfile(profile)
}
func (a *EngineAdapter) ApplySessionProfile(
	profile protocol.SessionProfile,
) error {
	if a == nil || a.engine == nil {
		return errors.New("engine is unavailable")
	}
	return a.engine.ApplySessionProfile(profile)
}
func (a *EngineAdapter) SetPolicyMode(mode policy.Mode) {
	if a != nil && a.engine != nil {
		a.engine.SetPolicyMode(mode)
	}
}
func (a *EngineAdapter) SetPermission(permission policy.Permission) {
	if a != nil && a.engine != nil {
		a.engine.SetPermission(permission)
	}
}
func (a *EngineAdapter) SetGranular(granular policy.Granular) {
	if a != nil && a.engine != nil {
		a.engine.SetGranular(granular)
	}
}
func AdaptEngine(value *agentengine.Engine) *EngineAdapter {
	return &EngineAdapter{engine: value}
}

func AdaptEngineWithWorkspaceIdentity(
	value *agentengine.Engine,
	identity protocol.WorkspaceIdentity,
) *EngineAdapter {
	return &EngineAdapter{engine: value, workspaceIdentity: identity}
}

// AllowIdleTurn rejects extension/automation idle starts while Plan mode is active (C4).
func (a *EngineAdapter) AllowIdleTurn() error {
	if a == nil || a.engine == nil {
		return nil
	}
	seed := a.engine.OptionsSeed()
	if seed.Security != nil && seed.Security.ModeValue() == policy.ModePlan {
		return protocol.NewProblem(
			protocol.CodeConflict,
			"plan mode rejects automatic idle turns",
			false,
			nil,
		)
	}
	return nil
}

func (a *EngineAdapter) StartTurn(
	ctx context.Context,
	payload *protocol.StartTurnPayload,
	sink EngineSink,
) error {
	if payload != nil && payload.Idle {
		if err := a.AllowIdleTurn(); err != nil {
			return err
		}
	}
	intent := protocol.NormalizeTurnIntent(payload.Intent)
	identity := a.workspaceIdentity
	if payload.WorkspaceIdentity != nil {
		if identity.Version != 0 && identity != *payload.WorkspaceIdentity {
			return protocol.NewProblem(
				protocol.CodeInvalidArgument,
				"turn workspace identity does not match Runtime binding",
				false,
				nil,
			)
		}
		identity = *payload.WorkspaceIdentity
	}
	var identities []protocol.WorkspaceIdentity
	if identity.Version != 0 {
		identities = append(identities, identity)
	}
	contextWorkspace := a.engine.OptionsSeed().Workspace
	if a.workspaceIdentity.Version != 0 {
		// Editor context remains bound to the visible workspace even when this
		// Engine executes tools in an isolated worktree.
		contextWorkspace = a.workspaceIdentity.RuntimePath
	}
	modelPrompt, editorContext, attachments, resolveErr := promptcontext.ResolveEditorContextWithAttachments(
		contextWorkspace, payload.Prompt, payload.Context, identities...,
	)
	if resolveErr != nil {
		return resolveErr
	}
	receipt := executionreceipt.New(payload.Prompt)
	receipt.Configure(
		intent,
		editorContext,
	)
	emit := func(event agentengine.Event) error {
		if event.Commentary != nil {
			return sink.Emit(event.Commentary)
		}
		receipt.Observe(event)
		if event.CatalogChanged != nil {
			convert := func(changes []tool.CatalogChange) []protocol.ToolCatalogChange {
				result := make([]protocol.ToolCatalogChange, len(changes))
				for index, change := range changes {
					result[index] = protocol.ToolCatalogChange{
						Name: change.Name, Source: change.Source, Revision: change.Revision,
					}
				}
				return result
			}
			return sink.Emit(&protocol.ToolCatalogChangedData{
				CatalogID:  event.CatalogChanged.CatalogID,
				Generation: event.CatalogChanged.Generation,
				Digest:     event.CatalogChanged.Digest,
				Added:      convert(event.CatalogChanged.Added),
				Replaced:   convert(event.CatalogChanged.Replaced),
				Revoked:    convert(event.CatalogChanged.Revoked),
			})
		}
		if event.MCPHealthChanged != nil {
			current := event.MCPHealthChanged.Current
			var retryAt *time.Time
			if !current.RetryAt.IsZero() {
				value := current.RetryAt
				retryAt = &value
			}
			return sink.Emit(&protocol.MCPHealthChangedData{
				Server:              current.Server,
				PreviousState:       event.MCPHealthChanged.PreviousState,
				State:               string(current.State),
				ConsecutiveFailures: current.ConsecutiveFailures,
				LastError:           current.LastError,
				ChangedAt:           current.ChangedAt,
				RetryAt:             retryAt,
			})
		}
		switch event.State {
		case agentengine.Preparing:
			displayPrompt := payload.DisplayPrompt
			if displayPrompt == "" {
				displayPrompt = payload.Prompt
			}
			planID, planTransition, _ := turnPlanExecution(payload)
			return sink.Emit(&protocol.TurnStartedData{
				Provider: event.Provider, Model: event.Model,
				ModelMetadata: event.ModelMetadata,
				QueueID:       payload.QueueID, PlanID: planID,
				PlanTransition:  planTransition,
				ProfileRevision: event.ProfileRevision,
				Intent:          intent,
				Mode:            event.Mode, Posture: event.Posture,
				Workspace:          event.Workspace,
				WorkspaceIsolation: event.WorkspaceIsolation,
				Sandbox:            event.Sandbox,
				Prompt:             modelPrompt, DisplayPrompt: displayPrompt,
				EditorContext: editorContext,
				Images:        turnImageAttachments(attachments),
			})
		case agentengine.Completed:
			receipt.SetOutcome(protocol.OutcomeForIntent(intent))
			secondary := terminalIssues(event.SecondaryIssues)
			return a.commitTerminal(ctx, receipt, sink, true, &protocol.TurnCompletedData{
				Text: event.Text, Outcome: protocol.OutcomeForIntent(intent),
				SecondaryIssues: secondary,
			})
		case agentengine.Failed:
			secondary := terminalIssues(event.SecondaryIssues)
			return a.commitTerminal(ctx, receipt, sink, false, &protocol.TurnFailedData{
				Code:            nonEmptyCode(event.ErrorCode, protocol.CodeInternal),
				Message:         nonEmpty(event.Error, "turn failed"),
				Fault:           protocol.CloneFaultMetadata(event.Fault),
				Convergence:     event.Convergence,
				SecondaryIssues: secondary,
			})
		case agentengine.Canceled:
			return a.commitTerminal(ctx, receipt, sink, false, &protocol.TurnCanceledData{
				Reason: protocol.NormalizeCancelReason(event.CancelReason),
			})
		case agentengine.AwaitingApproval:
			if event.ApprovalResolution != nil {
				resolved := &protocol.ApprovalResolvedData{
					RequestID: event.ApprovalResolution.RequestID,
					Decision:  protocol.ApprovalDecision(event.ApprovalResolution.Decision),
					Source:    a.approvalSource,
				}
				if event.ApprovalResolution.Reason != "" {
					resolved.Problem = protocol.NewProblemWithDetails(
						protocol.CodeConflict,
						"tool approval expired",
						false,
						protocol.ProblemDetails{
							Reason: event.ApprovalResolution.Reason,
						},
						nil,
					)
				}
				return sink.Emit(resolved)
			}
			if event.Approval == nil {
				return nil
			}
			resources := make([]protocol.CanonicalResource, len(event.Approval.Resources))
			for index, resource := range event.Approval.Resources {
				resources[index] = protocol.CanonicalResource{
					Kind: resource.Kind, Path: resource.Path, ID: resource.ID,
					Access: string(resource.Access), Tree: resource.Tree,
				}
			}
			scopes := make([]protocol.ApprovalScope, len(event.Approval.AllowedScopes))
			for index, scope := range event.Approval.AllowedScopes {
				scopes[index] = protocol.ApprovalScope(scope)
			}
			var editPlan *protocol.EditPlan
			if event.Approval.EditPlan != nil {
				files := make([]protocol.EditPlanFile, len(event.Approval.EditPlan.Files))
				for index, file := range event.Approval.EditPlan.Files {
					files[index] = protocol.EditPlanFile{
						Path: file.Path, Kind: file.Kind,
						Before: file.Before, After: file.After,
						BeforeExists: file.BeforeExists, AfterExists: file.AfterExists,
						BeforeDigest: file.BeforeDigest, AfterDigest: file.AfterDigest,
					}
				}
				editPlan = &protocol.EditPlan{
					ID: event.Approval.EditPlan.ID, Diff: event.Approval.EditPlan.Diff,
					Files: files,
				}
			}
			var grant *protocol.ApprovalGrantPreview
			if event.Approval.Grant != nil {
				grant = &protocol.ApprovalGrantPreview{
					Kind: event.Approval.Grant.Kind, Key: event.Approval.Grant.Key,
					Summary: event.Approval.Grant.Summary,
				}
			}
			return sink.Emit(&protocol.ApprovalRequiredData{
				RequestID: event.Approval.RequestID, CallID: event.Approval.CallID,
				Tool: event.Approval.Tool, Arguments: event.Approval.Arguments,
				ArgumentsDigest: event.Approval.ArgumentsDigest, Resources: resources,
				AllowedScopes: scopes, ExpiresAt: event.Approval.ExpiresAt,
				ReplacementAllowed:  event.Approval.ReplacementAllowed,
				ModifiableArguments: event.Approval.ModifiableArguments,
				Effect:              string(event.Approval.Effect),
				Risk:                string(event.Approval.Risk),
				ReasonCode:          event.Approval.ReasonCode,
				Network:             protocolNetwork(event.Approval.Network),
				EditPlan:            editPlan,
				GrantPreview:        grant,
				Source:              a.approvalSource,
			})
		case agentengine.AwaitingInput:
			if event.Input == nil {
				return nil
			}
			return sink.Emit(&protocol.InputRequiredData{
				RequestID: event.Input.RequestID, CallID: event.Input.CallID,
				Tool: event.Input.Tool, Prompt: event.Input.Prompt,
				Options: event.Input.Options, ExpiresAt: event.Input.ExpiresAt,
			})
		case agentengine.RunningTools:
			// A tool that samples a model reports its tokens during the tool
			// phase. Dropping it here is what used to keep a vision call out of
			// the ledger even after the engine had measured it.
			if event.Usage != nil {
				return emitRichEngineEvent(sink, event)
			}
			// Output arrives while the call is still open, so it is checked before the
			// start/result pair rather than as one of their shapes.
			if event.ToolOutput != nil {
				return sink.Emit(&protocol.ToolOutputData{
					Tool: event.ToolOutput.Tool, CallID: event.ToolOutput.CallID,
					Stream: event.ToolOutput.Stream, Chunk: event.ToolOutput.Chunk,
					Cursor: event.ToolOutput.Cursor, Truncated: event.ToolOutput.Truncated,
				})
			}
			if event.ToolCall != nil && event.Result == nil {
				return sink.Emit(&protocol.ToolStartData{
					Tool:      event.ToolCall.Name,
					CallID:    event.ToolCall.ID,
					Arguments: ValidToolArguments(event.ToolCall.Arguments),
				})
			}
			if event.ToolCall != nil && event.Result != nil {
				changes := make([]protocol.FileChange, len(event.FileChanges))
				for index, change := range event.FileChanges {
					changes[index] = protocol.FileChange{
						Path: change.Path, Kind: change.Kind,
						Added: change.Added, Removed: change.Removed,
					}
				}
				var recovery *protocol.ToolRecovery
				var completion *protocol.CompletionDeclaration
				var observedChanges *int
				var workspaceWriteScope string
				if metadata := event.Result.Metadata; metadata != nil {
					category, _ := metadata["error_category"].(string)
					action, _ := metadata["required_action"].(string)
					if category != "" && action != "" {
						path, _ := metadata["path"].(string)
						retry, _ := metadata["retry_original"].(bool)
						recovery = &protocol.ToolRecovery{
							ErrorCategory: category, RequiredAction: action,
							Path: path, RetryOriginal: retry,
						}
					}
					if count, ok := metadata["observed_changes"].(int); ok {
						observedChanges = &count
					}
					workspaceWriteScope, _ = metadata["workspace_write_scope"].(string)
				}
				if event.Result.Outcome != nil && event.Result.Outcome.Facts != nil &&
					event.Result.Outcome.Facts.Completion != nil {
					declaration := event.Result.Outcome.Facts.Completion
					accepted, _ := event.Result.Metadata["completion_declaration_accepted"].(bool)
					rejection, _ := event.Result.Metadata["completion_declaration_rejection"].(string)
					completion = &protocol.CompletionDeclaration{
						Status: declaration.Status, Summary: declaration.Summary,
						OutputMode:          declaration.OutputMode,
						ChangedPaths:        append([]string(nil), declaration.ChangedPaths...),
						VerificationCallIDs: append([]string(nil), declaration.VerificationCallIDs...),
						PendingActions:      append([]string(nil), declaration.PendingActions...),
						MutationRevision:    declaration.MutationRevision,
						CallID:              declaration.CallID, Accepted: accepted, Rejection: rejection,
					}
				}
				if err := sink.Emit(&protocol.ToolResultData{
					Tool: event.ToolCall.Name, CallID: event.ToolCall.ID,
					Output: event.Result.Content, IsError: event.Result.IsError,
					Execution: ProjectToolExecutionReceipt(event.Result.Execution),
					Changes:   changes, Recovery: recovery, Completion: completion,
					WorkspaceWriteScope: workspaceWriteScope,
					ObservedChanges:     observedChanges,
					Truncated:           event.Result.Truncated,
				}); err != nil {
					return err
				}
				planDelta, _ := event.Result.Metadata["plan_delta"].(bool)
				planArtifact, _ := event.Result.Metadata["plan_artifact"].(bool)
				if (planDelta || planArtifact) && !event.Result.IsError {
					plan, err := interact.ParseSubmittedPlan([]byte(event.Result.Content))
					if err != nil {
						return err
					}
					if err := sink.Emit(&protocol.PlanDeltaData{
						Body: event.Result.Content, Purpose: plan.Purpose, Done: true,
					}); err != nil {
						return err
					}
				}
				if cmd, ok := commandExecutionFromResult(event.ToolCall.ID, event.Result); ok {
					if err := sink.Emit(cmd); err != nil {
						return err
					}
				}
				if len(event.Diagnostics) != 0 {
					receipts := make([]protocol.DiagnosticReceipt, len(event.Diagnostics))
					for index, receipt := range event.Diagnostics {
						diagnostics := make([]protocol.Diagnostic, len(receipt.Diagnostics))
						for diagnosticIndex, value := range receipt.Diagnostics {
							diagnostics[diagnosticIndex] = protocol.Diagnostic{
								Path: value.Path,
								Range: protocol.DiagnosticRange{
									Start: protocol.DiagnosticPosition{
										Line: value.Range.Start.Line, Character: value.Range.Start.Character,
									},
									End: protocol.DiagnosticPosition{
										Line: value.Range.End.Line, Character: value.Range.End.Character,
									},
								},
								Severity: value.Severity, Code: value.Code,
								Message: value.Message, Source: value.Source,
							}
						}
						receipts[index] = protocol.DiagnosticReceipt{
							Path: receipt.Path, Status: receipt.Status, Runner: receipt.Runner,
							Diagnostics: diagnostics, Message: receipt.Message,
							ErrorCategory: receipt.ErrorCategory, ExitCode: receipt.ExitCode,
						}
					}
					return sink.Emit(&protocol.DiagnosticsData{
						Tool: event.ToolCall.Name, CallID: event.ToolCall.ID, Receipts: receipts,
					})
				}
				return nil
			}
		case agentengine.Verifying:
			if event.Verification == nil {
				return nil
			}
			return sink.Emit(executionreceipt.VerificationData(event.Verification))
		case agentengine.Streaming:
			return emitRichEngineEvent(sink, event)
		case agentengine.CallingModel:
			if data := providerAttemptData(event); data != nil {
				return sink.Emit(data)
			}
			return nil
		case agentengine.Compacting:
			if event.Compaction == nil {
				return nil
			}
			return sink.Emit(ProtocolCompactionData(event.Compaction))
		}
		return sink.Emit(&protocol.ToolStateData{State: string(event.State), Text: event.Text})
	}
	security := a.engine.OptionsSeed().Security
	// PlanSubmitted persists across turns: resetting it per turn forced a
	// plan re-submission before the first consequential action of every
	// turn. ConfigurePlanning still resets on policy/profile changes, and
	// each plan-approved turn re-submits explicitly.
	_, planTransition, planApproved := turnPlanExecution(payload)
	if planApproved {
		mode, permission := security.ModeValue(), security.PermissionValue()
		a.engine.SetPolicyMode(policy.ModeAct)
		security.SubmitPlan()
		if planTransition == protocol.PlanTransitionAutopilot {
			a.engine.SetPermission(policy.PermissionAuto)
		}
		defer func() {
			a.engine.SetPolicyMode(mode)
			a.engine.SetPermission(permission)
		}()
	}
	_, runErr := a.engine.Execute(
		tool.WithTurnIdentity(ctx, string(payload.ThreadID), string(payload.TurnID)),
		agentengine.TurnRequest{
			TurnID: string(payload.TurnID),
			Prompt: modelPrompt, Intent: intent, Attachments: attachments,
			Recovery: payload.Recovery,
		},
		emit,
	)
	return runErr
}

func turnPlanExecution(
	payload *protocol.StartTurnPayload,
) (string, protocol.PlanTransition, bool) {
	switch {
	case payload == nil:
		return "", "", false
	case payload.PlanExecution != nil:
		return payload.PlanExecution.PlanID,
			payload.PlanExecution.Transition,
			true
	case payload.Recovery != nil && payload.Recovery.PlanID != "":
		return payload.Recovery.PlanID,
			payload.Recovery.PlanTransition,
			true
	default:
		return "", "", false
	}
}

func turnImageAttachments(
	attachments []provider.Attachment,
) []protocol.EditorContextReference {
	if len(attachments) == 0 {
		return nil
	}
	result := make([]protocol.EditorContextReference, 0, len(attachments))
	for _, attachment := range attachments {
		digest := sha256.Sum256(attachment.Data)
		result = append(result, protocol.EditorContextReference{
			Kind: protocol.EditorContextImage, Source: protocol.EditorContextSourceNativePicker,
			Label: attachment.Name, MediaType: attachment.MediaType,
			Digest:  hex.EncodeToString(digest[:]),
			Content: base64.StdEncoding.EncodeToString(attachment.Data), Explicit: true,
		})
	}
	return result
}

func (a *EngineAdapter) buildReceipt(
	recorder *executionreceipt.Recorder,
	completed bool,
) (*protocol.ExecutionReceiptData, error) {
	if recorder == nil || !recorder.HasBudget() {
		return nil, errors.New("terminal event is missing a frozen context budget")
	}
	data := recorder.Build()
	if data == nil {
		return nil, nil
	}
	if err := executionreceipt.ValidateTerminal(data, completed); err != nil {
		return nil, err
	}
	return data, nil
}
func (a *EngineAdapter) commitTerminal(
	ctx context.Context,
	recorder *executionreceipt.Recorder,
	sink EngineSink,
	completed bool,
	terminal protocol.EventData,
) error {
	frozen, err := a.engine.FrozenTerminalState(context.WithoutCancel(ctx))
	if err != nil {
		return err
	}
	measurement, err := executionreceipt.FreezeTerminalMeasurement(
		a.engine.FreezeTerminalMeasurement(
			terminalTraceStatus(terminal),
		),
		frozen.State.Usage,
	)
	if err != nil {
		return err
	}
	recorder.Freeze(a.engine, &measurement)
	receipt, err := a.buildReceipt(recorder, completed)
	if err != nil {
		return err
	}
	var sessionDelta json.RawMessage
	if delta, ok := a.engine.PreparedSessionDelta(); ok {
		sessionDelta, err = json.Marshal(delta)
		if err != nil {
			return err
		}
	}
	if commitSink, ok := sink.(TerminalCommitSink); ok {
		return commitSink.CommitTerminal(TerminalMaterial{
			FrozenState:  frozen.State,
			DomainFacts:  frozen.DomainFacts,
			Measurement:  measurement,
			Receipt:      receipt,
			Terminal:     terminal,
			SessionDelta: sessionDelta,
		})
	}
	return errors.New("terminal commit sink is required")
}
func (a *EngineAdapter) CancelTurn(
	_ context.Context, payload *protocol.CancelTurnPayload, _ EngineSink,
) error {
	reason := protocol.NormalizeCancelReason(payload.Reason)
	control, err := a.engine.Control()
	if err != nil {
		return err
	}
	return control.Cancel(reason)
}

func (a *EngineAdapter) SteerTurn(
	_ context.Context,
	payload *protocol.SteerTurnPayload,
	sink EngineSink,
) error {
	control, err := a.engine.Control()
	if err != nil {
		return err
	}
	if err := control.Steer(payload.Prompt); err != nil {
		return err
	}
	return sink.Emit(&protocol.TurnSteeredData{
		Prompt:  payload.Prompt,
		QueueID: payload.QueueID,
	})
}

func (a *EngineAdapter) DecideApproval(
	_ context.Context, payload *protocol.ApprovalDecisionPayload, sink EngineSink,
) error {
	decision := toolguard.ApprovalDecision{
		RequestID:            payload.RequestID,
		Approved:             payload.Decision == protocol.ApprovalApprove,
		Canceled:             payload.Decision == protocol.ApprovalCancel,
		Scope:                policy.ApprovalScope(payload.Scope),
		ExpiresAt:            payload.ExpiresAt,
		ReplacementArguments: payload.ReplacementArguments,
		PlanID:               payload.PlanID,
	}
	control, err := a.engine.Control()
	if err != nil {
		return err
	}
	if err := control.ResolveApproval(decision); err != nil {
		return err
	}
	resolved := &protocol.ApprovalResolvedData{
		RequestID: payload.RequestID, Decision: payload.Decision,
		Source: a.approvalSource,
	}
	if payload.Decision == protocol.ApprovalDeny {
		message := "tool approval was denied"
		if a.approvalSource != nil {
			message = "child tool approval was denied"
		}
		resolved.Problem = protocol.NewProblemWithDetails(
			protocol.CodeConflict,
			message,
			false,
			protocol.ProblemDetails{Reason: "approval_denied"},
			nil,
		)
	}
	return sink.Emit(resolved)
}

func (a *EngineAdapter) ReplyInput(
	_ context.Context, payload *protocol.InputReplyPayload, sink EngineSink,
) error {
	reply := interact.Reply{
		RequestID: payload.RequestID, Answer: payload.Answer, Values: payload.Values,
	}
	control, err := a.engine.Control()
	if err != nil {
		return err
	}
	if err := control.ResolveInput(reply); err != nil {
		return err
	}
	return sink.Emit(&protocol.InputResolvedData{
		RequestID: payload.RequestID, Answer: payload.Answer,
	})
}

func (a *EngineAdapter) CompactThread(
	ctx context.Context,
	payload *protocol.CompactThreadPayload,
	sink EngineSink,
) error {
	beforeID, _ := a.engine.TokenWindowIdentity()
	result, err := a.engine.CompactForcedDurable(
		ctx,
		payload.ThreadID,
		payload.TurnID,
		payload.Focus,
	)
	if err != nil {
		return err
	}
	receipt := result.Receipt
	summary := "context already within budget; no messages compacted"
	if receipt != nil {
		summary = FormatCompactionSummary(receipt)
	}
	encoded, err := sessionhistory.EncodeCompactedHistory(a.engine.History())
	if err != nil {
		return err
	}
	windowID, windowNumber := a.engine.TokenWindowIdentity()
	if receipt == nil && windowID == beforeID {
		windowID, windowNumber = a.engine.AdvanceTokenWindow()
	}
	data := &protocol.ThreadCompactedData{
		Summary:            summary,
		ReplacementHistory: encoded,
		WindowNumber:       windowNumber,
		FirstWindowID:      beforeID,
		PreviousWindowID:   beforeID,
		WindowID:           windowID,
	}
	ApplyThreadCompactionTruth(data, receipt)
	return sink.Emit(data)
}

func ApplyThreadCompactionTruth(
	data *protocol.ThreadCompactedData,
	receipt *agentengine.CompactionReceipt,
) {
	if data == nil || receipt == nil {
		return
	}
	data.TruthGeneration = receipt.TruthGeneration
	data.TruthEntities = receipt.TruthEntities
	data.CriticalFacts = receipt.CriticalFacts
	data.CompatibilityHash = receipt.CompatibilityHash
	data.AuthorityDigest = receipt.AuthorityDigest
	data.AuthorityEquivalent = receipt.AuthorityEquivalent
	data.ModelDownshifted = receipt.ModelDownshifted
	data.DownshiftPolicy = receipt.DownshiftPolicy
	data.NarrativeIncluded = receipt.NarrativeIncluded
	data.MandatoryBytes = receipt.MandatoryBytes
	data.MandatoryEntities = receipt.MandatoryEntities
	data.OmissionCount = receipt.OmissionCount
	data.Retention = protocolRetention(receipt.Retention)
}

func ProtocolCompactionData(
	receipt *agentengine.CompactionReceipt,
) *protocol.TurnCompactionData {
	if receipt == nil {
		return nil
	}
	return &protocol.TurnCompactionData{
		CompactionID: receipt.CompactionID,
		Status:       receipt.Status, Mode: receipt.Mode,
		SourceWindowID: receipt.SourceWindowID,
		TargetWindowID: receipt.TargetWindowID,
		Phase: nonEmpty(
			receipt.Phase,
			agentengine.CompactionPhasePreSampling,
		),
		Summary:               FormatCompactionSummary(receipt),
		RemovedMessages:       receipt.RemovedMessages,
		OriginalBytes:         receipt.OriginalBytes,
		RetainedBytes:         receipt.RetainedBytes,
		Sections:              append([]string(nil), receipt.Sections...),
		SummaryTruncated:      receipt.SummaryTruncated,
		RemovedTurns:          append([]uint64(nil), receipt.RemovedTurns...),
		PrunedToolResults:     receipt.PrunedToolResults,
		PrunedBytes:           receipt.PrunedBytes,
		TruthGeneration:       receipt.TruthGeneration,
		TruthEntities:         receipt.TruthEntities,
		CriticalFacts:         receipt.CriticalFacts,
		CompatibilityHash:     receipt.CompatibilityHash,
		CompatibilityMatched:  receipt.CompatibilityMatched,
		AuthorityDigest:       receipt.AuthorityDigest,
		AuthorityEquivalent:   receipt.AuthorityEquivalent,
		ModelDownshifted:      receipt.ModelDownshifted,
		DownshiftPolicy:       receipt.DownshiftPolicy,
		NarrativeIncluded:     receipt.NarrativeIncluded,
		NarrativeBytes:        receipt.NarrativeBytes,
		NarrativeInputTokens:  receipt.NarrativeInputTokens,
		NarrativeOutputTokens: receipt.NarrativeOutputTokens,
		NarrativeProvider:     receipt.NarrativeProvider,
		NarrativeModel:        receipt.NarrativeModel,
		NarrativeMetadata:     receipt.NarrativeMetadata,
		FallbackReason:        receipt.FallbackReason,
		CapsuleBytes:          receipt.CapsuleBytes,
		MandatoryBytes:        receipt.MandatoryBytes,
		MandatoryEntities:     receipt.MandatoryEntities,
		OmissionCount:         receipt.OmissionCount,
		Retention:             protocolRetention(receipt.Retention),
	}
}

func protocolRetention(
	values []agentcontext.RetentionCount,
) []protocol.TruthRetentionCount {
	result := make([]protocol.TruthRetentionCount, len(values))
	for index, value := range values {
		result[index] = protocol.TruthRetentionCount{
			Class: string(value.Class), Candidates: value.Candidates,
			Retained: value.Retained, Omitted: value.Omitted,
		}
	}
	return result
}

func FormatCompactionSummary(receipt *agentengine.CompactionReceipt) string {
	if receipt.Status == "completed" && receipt.NarrativeIncluded {
		return "semantic narrative committed for the compacted context"
	}
	if receipt.Status == "fallback" {
		return "semantic narrative unavailable; retained deterministic truth and raw tail"
	}
	if receipt.RemovedMessages == 0 && receipt.PrunedToolResults != 0 {
		return fmt.Sprintf(
			"pruned %d tool result surfaces (%d→%d bytes)",
			receipt.PrunedToolResults,
			receipt.OriginalBytes,
			receipt.RetainedBytes,
		)
	}
	return fmt.Sprintf(
		"compacted history: removed %d messages and pruned %d tool results "+
			"(%d→%d bytes); removed turns=%v",
		receipt.RemovedMessages,
		receipt.PrunedToolResults,
		receipt.OriginalBytes,
		receipt.RetainedBytes,
		receipt.RemovedTurns,
	)
}

func (a *EngineAdapter) ForkThread(
	context.Context, *protocol.ForkThreadPayload, EngineSink,
) error {
	return ErrOperationUnsupported
}

func (a *EngineAdapter) RevertTurn(
	ctx context.Context,
	payload *protocol.RevertTurnPayload,
	sink EngineSink,
) error {
	receipt, revertErr := a.engine.RevertWorkspace(ctx, string(payload.TargetTurnID))
	if receipt.TurnID == "" {
		return revertErr
	}
	conflicts := make([]protocol.RevertConflict, len(receipt.Conflicts))
	for index, conflict := range receipt.Conflicts {
		conflicts[index] = protocol.RevertConflict{Path: conflict.Path, Reason: conflict.Reason}
	}
	if err := sink.Emit(&protocol.TurnRevertedData{
		TargetTurnID: payload.TargetTurnID, Restored: receipt.Restored, Conflicts: conflicts,
		NonFileSideEffectsReverted: receipt.NonFileSideEffectsReverted,
		NonFileSideEffectsNote:     receipt.NonFileSideEffectsNote,
	}); err != nil {
		return err
	}
	return revertErr
}

// CostMicrounits converts USD to millionths, rounding to nearest. Unknown or
// non-positive pricing stays zero so persisted rows never imply a false cost.
func CostMicrounits(costUSD float64) uint64 {
	if costUSD <= 0 || math.IsNaN(costUSD) || math.IsInf(costUSD, 0) {
		return 0
	}
	return uint64(math.Round(costUSD * 1e6))
}
func emitRichEngineEvent(sink EngineSink, event agentengine.Event) error {
	if event.ReasoningCompleted != nil {
		return sink.Emit(&protocol.ReasoningCompletedData{
			Text:     event.ReasoningCompleted.Text,
			SampleID: event.ReasoningCompleted.SampleID,
		})
	}
	if event.OutputDiscarded != nil {
		return sink.Emit(&protocol.OutputDiscardedData{
			SampleID: event.OutputDiscarded.SampleID,
			Reason:   event.OutputDiscarded.Reason,
		})
	}
	if event.Usage != nil {
		return sink.Emit(&protocol.UsageData{
			Sample: event.Sample, Provider: event.Provider, Model: event.Model,
			ModelMetadata: event.ModelMetadata,
			Context:       event.SampleContext,
			InputTokens:   event.Usage.InputTokens, OutputTokens: event.Usage.OutputTokens,
			ReasoningTokens: event.Usage.ReasoningTokens,
			CachedTokens:    event.Usage.CachedTokens,
			CostMicrounits:  CostMicrounits(event.CostUSD),
			CostKnown:       event.CostKnown,
		})
	}
	if event.Search != nil {
		sources := make([]protocol.Source, len(event.Search.Sources))
		for index, source := range event.Search.Sources {
			sources[index] = protocol.Source{ID: source.ID, Title: source.Title, URL: source.URL}
		}
		return sink.Emit(&protocol.SearchResultData{Query: event.Search.Query, Sources: sources})
	}
	if event.Citation != nil {
		return sink.Emit(&protocol.CitationData{
			SourceID: event.Citation.SourceID, Title: event.Citation.Title,
			URL: event.Citation.URL, Start: event.Citation.Start, End: event.Citation.End,
		})
	}
	if event.Block != nil {
		switch event.Block.Type {
		case provider.ContentText:
			return sink.Emit(&protocol.OutputDraftData{
				Text:     event.Block.Text,
				SampleID: event.SampleID,
			})
		case provider.ContentReasoning:
			if event.Block.Text != "" {
				return sink.Emit(&protocol.ReasoningDeltaData{
					Text:     event.Block.Text,
					SampleID: event.SampleID,
				})
			}
			return nil
		}
	}
	return nil
}

func providerAttemptData(event agentengine.Event) *protocol.ProviderAttemptData {
	execution := event.ModelExecution
	if execution == nil || execution.Kind != "provider_attempt" || execution.Status == "" {
		return nil
	}
	data := &protocol.ProviderAttemptData{
		SampleID: execution.SampleID, Attempt: execution.Attempt,
		Status: execution.Status, Reason: execution.Reason,
		ProjectedInputTokens: execution.ProjectedInputTokens,
		RequestBytes:         execution.Transport.RequestBytes,
		RouteCooldownWaitMS:  execution.Transport.RouteCooldownWaitMS,
		StopReason:           string(execution.StopReason),
	}
	if !execution.StartedAt.IsZero() {
		started := execution.StartedAt
		data.StartedAt = &started
	}
	if !execution.FinishedAt.IsZero() {
		finished := execution.FinishedAt
		data.FinishedAt = &finished
	}
	projection := execution.Transport.Projection
	if projection.Mode != "" {
		data.Projection = &protocol.ProviderProjectionData{
			Mode:                       string(projection.Mode),
			IncrementalEligible:        projection.IncrementalEligible,
			FallbackReason:             string(projection.FallbackReason),
			RouteDigest:                projection.RouteDigest,
			PropertyDigest:             projection.PropertyDigest,
			StablePrefixDigest:         projection.StablePrefixDigest,
			InputDigest:                projection.InputDigest,
			DeltaDigest:                projection.DeltaDigest,
			ContextRevision:            projection.ContextRevision,
			WindowID:                   projection.WindowID,
			WindowNumber:               projection.WindowNumber,
			LogicalItems:               projection.LogicalItems,
			TransportItems:             projection.TransportItems,
			LogicalTransportEquivalent: projection.LogicalTransportEquivalent,
		}
	}
	if retry := event.ProviderRetry; retry != nil {
		data.FailureCode = string(retry.Failure.Code)
		data.ErrorCode = retry.Code
		data.HTTPStatus = retry.Failure.HTTPStatus
		data.ProviderRetryAfterMS = retry.Failure.RetryAfterMS
		if retry.EffectiveDelay > 0 {
			data.EffectiveDelayMS = uint64(retry.EffectiveDelay / time.Millisecond)
		}
		retryAt := retry.RetryAt
		data.RetryAt = &retryAt
		data.PolicyRevision = retry.PolicyRevision
	}
	data.RateLimitRetries = execution.RateLimitRetries
	data.RateLimitRetryLimit = execution.RateLimitRetryLimit
	if execution.RateLimitWaited > 0 {
		data.RateLimitWaitedMS = uint64(execution.RateLimitWaited / time.Millisecond)
	}
	if execution.RateLimitWaitBudget > 0 {
		data.RateLimitWaitBudgetMS = uint64(execution.RateLimitWaitBudget / time.Millisecond)
	}
	return data
}

func nonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func terminalIssues(source []agentengine.TerminalIssue) []protocol.TerminalIssue {
	result := make([]protocol.TerminalIssue, len(source))
	for index, issue := range source {
		result[index] = protocol.TerminalIssue{
			Phase: issue.Phase, Code: issue.Code, Message: issue.Message,
		}
	}
	return result
}

func commandExecutionFromResult(callID string, result *tool.Result) (*protocol.CommandExecutionData, bool) {
	if result == nil || result.Metadata == nil {
		return nil, false
	}
	raw, ok := result.Metadata["command_execution"].(map[string]any)
	if !ok || raw == nil {
		return nil, false
	}
	command, _ := raw["command"].(string)
	status, _ := raw["status"].(string)
	if command == "" || status == "" {
		return nil, false
	}
	if origin, _ := raw["call_id"].(string); origin != "" {
		callID = origin
	}
	data := &protocol.CommandExecutionData{
		CallID: callID, Command: command, Status: status,
	}
	if sessionID, _ := raw["session_id"].(string); sessionID != "" {
		data.SessionID = sessionID
	}
	if handle, _ := raw["handle"].(string); handle != "" {
		data.Handle = handle
	}
	if tail, _ := raw["output_tail"].(string); tail != "" {
		data.OutputTail = tail
	}
	switch v := raw["duration_ms"].(type) {
	case int64:
		data.DurationMS = v
	case int:
		data.DurationMS = int64(v)
	case float64:
		data.DurationMS = int64(v)
	}
	switch v := raw["exit_code"].(type) {
	case int:
		data.ExitCode = &v
	case int64:
		code := int(v)
		data.ExitCode = &code
	case float64:
		code := int(v)
		data.ExitCode = &code
	}
	return data, true
}
func protocolNetwork(value *toolguard.NetworkApprovalContext) *protocol.NetworkApprovalPayload {
	if value == nil {
		return nil
	}
	return &protocol.NetworkApprovalPayload{
		Host: value.Host, Protocol: value.Protocol, Mode: value.Mode,
	}
}
func ValidToolArguments(value string) json.RawMessage {
	raw := json.RawMessage(value)
	if !json.Valid(raw) {
		return nil
	}
	return raw
}
func nonEmptyCode(value, fallback protocol.ErrorCode) protocol.ErrorCode {
	if value == "" {
		return fallback
	}
	return value
}
