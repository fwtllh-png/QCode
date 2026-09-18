package turnkernel

import (
	"context"
	"errors"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

type ToolEffect struct {
	Context             context.Context
	Calls               []provider.ToolCall
	Executed            map[string]tool.Result
	Cache               *tool.ResultCache
	Registry            *tool.Registry
	Admit               func([]provider.ToolCall) error
	Execute             func(context.Context, provider.ToolCall) (tool.Result, error)
	Recover             func(provider.ToolCall, tool.Result, error) (tool.Result, bool)
	FailureCategory     func(error) string
	BeforeStart         func(provider.ToolCall)
	PublishStart        func(provider.ToolCall) error
	PublishAborted      func(provider.ToolCall, tool.Result) error
	BeforeClose         func(provider.ToolCall, *tool.Result, bool, uint64)
	CompletionCandidate func(provider.ToolCall, tool.Result, bool, int, uint64) CompletionCandidate
	AfterClose          func(provider.ToolCall, tool.Result) error
	PublishResult       func(provider.ToolCall, tool.Result) error
}

func (s *RuntimeKernel) ExecuteToolEffect(
	effect ToolEffect,
) ([]tool.Result, error) {
	if effect.Cache == nil || effect.Registry == nil {
		return nil, errors.New("tool effect dependencies are incomplete")
	}
	plan := effect.Cache.Plan(effect.Calls, effect.Executed, effect.Registry)
	planned := make([]provider.ToolCall, 0, len(effect.Calls))
	for _, call := range effect.Calls {
		if _, exists := effect.Executed[call.ID]; !exists {
			planned = append(planned, call)
		}
	}
	if effect.Admit != nil {
		if err := effect.Admit(planned); err != nil {
			return nil, err
		}
	}
	if err := s.ValidateToolStarts(planned); err != nil {
		return nil, err
	}
	published := make([]provider.ToolCall, 0, len(planned))
	for _, call := range planned {
		if effect.BeforeStart != nil {
			effect.BeforeStart(call)
		}
		if err := s.StartTools([]provider.ToolCall{call}); err != nil {
			return nil, errors.Join(
				err,
				s.abortPublishedTools(
					published,
					"tool batch aborted during lifecycle registration",
					effect,
				),
				s.AbortTools("tool lifecycle registration failed"),
			)
		}
		if err := s.StartTool(call.ID); err != nil {
			return nil, errors.Join(
				err,
				s.abortPublishedTools(
					published,
					"tool batch aborted before execution",
					effect,
				),
				s.AbortTools("tool durable start failed"),
			)
		}
		if effect.PublishStart != nil {
			if err := effect.PublishStart(call); err != nil {
				return nil, errors.Join(
					err,
					s.abortPublishedTools(
						published,
						"tool batch aborted before execution",
						effect,
					),
					s.AbortTools("tool start publication failed"),
				)
			}
		}
		published = append(published, call)
	}
	batch := tool.ExecuteBatch(tool.BatchExecution{
		Context:         effect.Context,
		Calls:           effect.Calls,
		Plan:            plan,
		Execute:         effect.Execute,
		Recover:         effect.Recover,
		FailureCategory: effect.FailureCategory,
	})
	results := batch.Results
	names := make([]string, len(effect.Calls))
	for index, call := range effect.Calls {
		names[index] = call.Name
	}
	results = effect.Registry.AdmitBatchWithin(
		names, results,
		tool.ResultTokenBudget(effect.Context),
		tool.ResultBatchBudget(effect.Context),
	)
	batchMutated := false
	for _, result := range results {
		if len(ObservedFileChanges(result)) != 0 {
			batchMutated = true
			break
		}
	}
	type completionEvaluation struct {
		index     int
		call      provider.ToolCall
		candidate CompletionCandidate
	}
	// Declarations are evaluated only after every call in the batch has
	// closed: the evaluator accepts PhaseSampling, which the kernel enters
	// when the last open call closes, so a declaration must not depend on
	// its position within the batch. Candidates are still built during the
	// close loop so verification evidence keeps binding to the mutation
	// revision observed at that point.
	pending := make([]completionEvaluation, 0, 1)
	var projectionErr error
	finishClosed := func(index int, call provider.ToolCall) {
		effect.Executed[call.ID] = results[index]
		if effect.AfterClose != nil {
			projectionErr = errors.Join(
				projectionErr,
				effect.AfterClose(call, results[index]),
			)
		}
		if effect.PublishResult != nil {
			projectionErr = errors.Join(
				projectionErr,
				effect.PublishResult(call, results[index]),
			)
		}
	}
	for index, call := range effect.Calls {
		if plan.AlreadyExecuted[index] {
			continue
		}
		mutationRevision := s.MutationRevision()
		if effect.BeforeClose != nil {
			effect.BeforeClose(
				call,
				&results[index],
				batchMutated,
				mutationRevision,
			)
		}
		changes := ObservedFileChanges(results[index])
		if err := s.CloseTool(call, results[index], changes); err != nil {
			projectionErr = errors.Join(projectionErr, err)
			continue
		}
		submittedPlan, _ := results[index].Metadata["submitted_plan"].(bool)
		defersFinish := false
		if (call.Name == "turn_complete" || submittedPlan) &&
			effect.CompletionCandidate != nil {
			candidate := effect.CompletionCandidate(
				call,
				results[index],
				batchMutated,
				len(effect.Calls),
				mutationRevision,
			)
			if call.Name == "turn_complete" || candidate.DeclarationValid {
				pending = append(pending, completionEvaluation{
					index: index, call: call, candidate: candidate,
				})
				// The turn_complete result is rewritten with the decision,
				// so its execution record and publication wait for the
				// evaluation below.
				defersFinish = call.Name == "turn_complete"
			}
		}
		if !defersFinish {
			finishClosed(index, call)
		}
	}
	for _, evaluation := range pending {
		decision, err := s.EvaluateCompletion(evaluation.candidate)
		if err != nil {
			projectionErr = errors.Join(projectionErr, err)
			continue
		}
		if evaluation.call.Name != "turn_complete" {
			continue
		}
		BindCompletionDecision(&results[evaluation.index], decision)
		finishClosed(evaluation.index, evaluation.call)
	}
	if projectionErr != nil {
		return results, projectionErr
	}
	effect.Cache.Commit(effect.Calls, plan, results, batchMutated)
	if batch.Error != nil {
		return results, batch.Error
	}
	return results, nil
}

func (s *RuntimeKernel) abortPublishedTools(
	calls []provider.ToolCall,
	reason string,
	effect ToolEffect,
) error {
	var resultErr error
	for _, call := range calls {
		result := tool.Result{
			Content: reason,
			IsError: true,
			Metadata: map[string]any{
				"error_category": "tool_batch_aborted",
				"fatal":          true,
			},
		}
		result, _ = effect.Registry.AdmitResultWithin(call.Name, result, tool.ResultTokenBudget(effect.Context))
		if effect.PublishAborted != nil {
			resultErr = errors.Join(
				resultErr,
				effect.PublishAborted(call, result),
			)
		}
	}
	return resultErr
}
